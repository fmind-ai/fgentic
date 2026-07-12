// Package a2a is a thin, gateway-focused wrapper over the official A2A Go SDK
// (github.com/a2aproject/a2a-go/v2) — the same client kagent uses. It resolves a local agent's
// AgentCard through agentgateway, sends one non-streaming message (message/send), extracts the
// reply text from the returned Task-or-Message sum type, and polls tasks/get for long-running
// tasks. Streaming is intentionally unused (fire-and-forget delegation, docs/bridge.md §6).
//
// Only LOCAL kagent targets are dialed here: reaching kagent solely through agentgateway keeps
// the model-credential chokepoint intact (docs/design-decisions.md D6). Pinned remote A2A agents
// and their Signed AgentCard verification are a separate outbound trust boundary landed by later
// federation work, never by this gateway's inbound AP surface.
package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	sdk "github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// userHeader carries the asserted AP actor to kagent for session/audit attribution. kagent's
// default auth mode reads the caller identity from this header (docs/audit.md). It is NOT a
// credential — the workload credential is the separate Authorization bearer.
const userHeader = "X-User-Id"

type userKey struct{}

// WithUser returns a context that stamps userID onto every outgoing A2A HTTP request.
func WithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userKey{}, userID)
}

// userTransport injects the asserted user header and the workload bearer from each request's
// context, so one cached client serves per-call attribution.
type userTransport struct {
	base   http.RoundTripper
	apiKey string
}

func (t *userTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	user, _ := req.Context().Value(userKey{}).(string)
	req = req.Clone(req.Context())
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if user != "" {
		req.Header.Set(userHeader, user)
	}
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	return t.base.RoundTrip(req)
}

// Client dials local kagent agents beneath a common base URL (agentgateway). SDK clients are
// resolved once per agent from the AgentCard and cached.
type Client struct {
	baseURL        string
	requestTimeout time.Duration
	taskTimeout    time.Duration
	log            *slog.Logger
	httpClient     *http.Client
	resolver       *agentcard.Resolver

	mu    sync.Mutex
	cache map[string]*sdk.Client
}

// New returns a Client that resolves agents relative to baseURL (the agentgateway proxy). apiKey
// is the workload credential enforced by the A2A route; it may be empty only when dialing an
// unsecured development fixture directly.
func New(baseURL, apiKey string, requestTimeout, taskTimeout time.Duration, log *slog.Logger) *Client {
	httpClient := &http.Client{Transport: &userTransport{base: http.DefaultTransport, apiKey: apiKey}}
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		requestTimeout: requestTimeout,
		taskTimeout:    taskTimeout,
		log:            log,
		httpClient:     httpClient,
		resolver:       agentcard.NewResolver(httpClient),
		cache:          make(map[string]*sdk.Client),
	}
}

// agentPath is the kagent A2A route for a namespace/name pair, served beneath baseURL through
// agentgateway (…/api/a2a/<namespace>/<name>, card at …/.well-known/agent-card.json).
func agentPath(namespace, name string) string {
	return "/api/a2a/" + namespace + "/" + name
}

// Call delegates text to the local kagent agent namespace/name via A2A message/send. A non-empty
// contextID threads the conversation (kagent maps contextId 1:1 to a persistent session). It
// returns the agent's reply text, polling tasks/get until the task is terminal when the agent
// runs long. The caller stamps the asserted actor with WithUser before invoking.
func (c *Client) Call(ctx context.Context, namespace, name, text, contextID string) (string, error) {
	if namespace == "" || name == "" {
		return "", fmt.Errorf("a2a call: empty namespace or name")
	}
	client, err := c.clientFor(ctx, namespace, name)
	if err != nil {
		return "", err
	}

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if contextID != "" {
		msg.ContextID = contextID
	}

	sendCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	res, err := client.SendMessage(sendCtx, &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		return "", fmt.Errorf("a2a message/send to %s/%s: %w", namespace, name, err)
	}

	switch v := res.(type) {
	case *a2a.Message:
		return partsText(v.Parts), nil
	case *a2a.Task:
		return c.awaitTask(ctx, client, namespace, name, v)
	default:
		return "", fmt.Errorf("a2a message/send to %s/%s: unexpected result type", namespace, name)
	}
}

// awaitTask polls tasks/get until the task reaches a terminal state or TaskTimeout elapses,
// then returns its text. A failed/canceled/rejected terminal state is surfaced as an error so
// the inbox never publishes agent-side error detail as a governed reply.
func (c *Client) awaitTask(ctx context.Context, client *sdk.Client, namespace, name string, task *a2a.Task) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, c.taskTimeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if task.Status.State.Terminal() {
			if task.Status.State != a2a.TaskStateCompleted {
				return "", fmt.Errorf("a2a task %s from %s/%s ended in state %q", task.ID, namespace, name, task.Status.State)
			}
			return taskText(task), nil
		}
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("a2a task %s from %s/%s: %w", task.ID, namespace, name, deadline.Err())
		case <-ticker.C:
		}
		polled, err := client.GetTask(deadline, &a2a.GetTaskRequest{ID: task.ID})
		if err != nil {
			return "", fmt.Errorf("a2a tasks/get %s from %s/%s: %w", task.ID, namespace, name, err)
		}
		task = polled
	}
}

// clientFor resolves (and caches) an SDK client for a local agent by fetching its AgentCard.
// Routing baseURL + card path keeps clients pointing through agentgateway when it rewrites the
// card's advertised URL (a no-op when talking to kagent directly).
func (c *Client) clientFor(ctx context.Context, namespace, name string) (*sdk.Client, error) {
	key := namespace + "/" + name
	c.mu.Lock()
	cached := c.cache[key]
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	cardPath := agentPath(namespace, name) + "/.well-known/agent-card.json"
	resolveCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	card, err := c.resolver.Resolve(resolveCtx, c.baseURL, agentcard.WithPath(cardPath))
	if err != nil {
		return nil, fmt.Errorf("resolve agent card %s%s: %w", c.baseURL, cardPath, err)
	}
	if card == nil {
		return nil, fmt.Errorf("resolve agent card %s%s: empty response", c.baseURL, cardPath)
	}
	client, err := sdk.NewFromCard(
		ctx, card,
		sdk.WithJSONRPCTransport(c.httpClient),
		sdk.WithRESTTransport(c.httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("build a2a client for %s: %w", key, err)
	}

	c.mu.Lock()
	if existing := c.cache[key]; existing != nil {
		client = existing
	} else {
		c.cache[key] = client
	}
	c.mu.Unlock()
	c.log.Info("resolved a2a agent", "agent", key, "card_name", card.Name)
	return client, nil
}

func taskText(t *a2a.Task) string {
	if s := artifactsText(t.Artifacts); s != "" {
		return s
	}
	if t.Status.Message != nil {
		if s := partsText(t.Status.Message.Parts); s != "" {
			return s
		}
	}
	for i := len(t.History) - 1; i >= 0; i-- {
		if t.History[i].Role == a2a.MessageRoleAgent {
			if s := partsText(t.History[i].Parts); s != "" {
				return s
			}
		}
	}
	return fmt.Sprintf("(agent finished in state %q with no text output)", t.Status.State)
}

func artifactsText(artifacts []*a2a.Artifact) string {
	var b strings.Builder
	for _, a := range artifacts {
		if s := partsText(a.Parts); s != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(s)
		}
	}
	return b.String()
}

func partsText(parts a2a.ContentParts) string {
	var b strings.Builder
	for _, p := range parts {
		if t := p.Text(); t != "" {
			b.WriteString(t)
		}
	}
	return strings.TrimSpace(b.String())
}
