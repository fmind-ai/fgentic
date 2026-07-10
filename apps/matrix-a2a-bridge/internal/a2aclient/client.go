// Package a2aclient is a thin, bridge-focused wrapper over the official A2A Go SDK
// (github.com/a2aproject/a2a-go/v2). It resolves an agent's AgentCard, sends a single
// non-streaming message (message/send), extracts the reply text from the returned
// Task-or-Message sum type, and polls tasks/get for long-running tasks (SPEC §6).
// Streaming is intentionally not used (fire-and-forget delegation).
package a2aclient

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	sdk "github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// userHeader carries the Matrix sender to kagent for session/audit attribution (SPEC §4 F11):
// kagent's default auth mode reads the caller identity from this header.
const userHeader = "X-User-Id"

type userKey struct{}

// WithUser returns a context that stamps userID onto every outgoing A2A HTTP request.
func WithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userKey{}, userID)
}

// userTransport injects the user header from the request context (requests inherit the
// context passed to the SDK call, so per-call attribution works with one cached client).
type userTransport struct {
	base http.RoundTripper
}

func (t *userTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if user, ok := req.Context().Value(userKey{}).(string); ok && user != "" {
		req = req.Clone(req.Context())
		req.Header.Set(userHeader, user)
	}
	return t.base.RoundTrip(req)
}

// Result is the bridge-shaped outcome of an A2A call: the extracted reply text, the contextId
// to reuse for the next turn, and — for long-running tasks — the task ID to poll until Terminal.
// Failed marks a task that ended without completing (failed/canceled/rejected): its Text is
// agent-side error detail for the logs, never for the room (SPEC §6).
type Result struct {
	Text      string
	ContextID string
	TaskID    string
	Terminal  bool
	Failed    bool
}

// Client dials A2A agents under a common base URL (the agentgateway proxy, or kagent directly).
// SDK clients are resolved once per agent path and cached.
type Client struct {
	baseURL    string
	log        *slog.Logger
	httpClient *http.Client
	resolver   *agentcard.Resolver

	mu    sync.Mutex
	cache map[string]*sdk.Client
}

// New returns a Client that resolves agents relative to baseURL (e.g. the agentgateway proxy).
func New(baseURL string, log *slog.Logger) *Client {
	httpClient := &http.Client{Transport: &userTransport{base: http.DefaultTransport}}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		log:        log,
		httpClient: httpClient,
		resolver:   agentcard.NewResolver(httpClient),
		cache:      make(map[string]*sdk.Client),
	}
}

// Call delegates text to the agent served under agentPath (e.g. "/api/a2a/kagent/k8s-agent")
// via A2A message/send. A non-empty contextID threads the conversation; the returned Result
// carries the contextId for the next turn and, when the agent answered with a still-running
// Task, the TaskID to poll (Terminal=false).
func (c *Client) Call(ctx context.Context, agentPath, text, contextID string) (Result, error) {
	client, err := c.clientFor(ctx, agentPath)
	if err != nil {
		return Result{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if contextID != "" {
		msg.ContextID = contextID // thread the room's conversation so the agent keeps context
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		return Result{}, fmt.Errorf("a2a message/send to %s: %w", agentPath, err)
	}
	return toResult(res), nil
}

// PollTask fetches the current state of a task previously returned by Call (tasks/get).
func (c *Client) PollTask(ctx context.Context, agentPath string, taskID string) (Result, error) {
	client, err := c.clientFor(ctx, agentPath)
	if err != nil {
		return Result{}, err
	}
	task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID)})
	if err != nil {
		return Result{}, fmt.Errorf("a2a tasks/get %s from %s: %w", taskID, agentPath, err)
	}
	return taskResult(task), nil
}

// clientFor resolves (and caches) an SDK client for a given agent path by fetching its AgentCard.
// Routing baseURL + card path keeps clients pointing through agentgateway when it rewrites the
// card's advertised URL (a no-op when talking to kagent directly).
func (c *Client) clientFor(ctx context.Context, agentPath string) (*sdk.Client, error) {
	key := agentPath
	c.mu.Lock()
	cached := c.cache[key]
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	cardPath := strings.TrimRight(agentPath, "/") + "/.well-known/agent-card.json"
	card, err := c.resolver.Resolve(ctx, c.baseURL, agentcard.WithPath(cardPath))
	if err != nil {
		return nil, fmt.Errorf("resolve agent card %s%s: %w", c.baseURL, cardPath, err)
	}
	// The user-stamping HTTP client is registered for both wire flavors kagent may advertise.
	client, err := sdk.NewFromCard(
		ctx, card,
		sdk.WithJSONRPCTransport(c.httpClient),
		sdk.WithRESTTransport(c.httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("build a2a client for %s: %w", agentPath, err)
	}

	c.mu.Lock()
	c.cache[key] = client
	c.mu.Unlock()
	c.log.Info("resolved a2a agent", "path", agentPath, "card_name", card.Name)
	return client, nil
}

// toResult maps a SendMessageResult (a *a2a.Message or *a2a.Task sum type) to a Result.
func toResult(res a2a.SendMessageResult) Result {
	switch v := res.(type) {
	case *a2a.Message:
		return Result{Text: partsText(v.Parts), ContextID: v.ContextID, Terminal: true}
	case *a2a.Task:
		return taskResult(v)
	default:
		return Result{Terminal: true}
	}
}

func taskResult(t *a2a.Task) Result {
	r := Result{
		ContextID: t.ContextID,
		TaskID:    string(t.ID),
		Terminal:  t.Status.State.Terminal(),
		Failed:    t.Status.State != a2a.TaskStateCompleted && t.Status.State.Terminal(),
	}
	if r.Terminal {
		r.Text = taskText(t)
	} else if t.Status.Message != nil {
		r.Text = partsText(t.Status.Message.Parts) // interim status, e.g. "working"
	}
	return r
}

// taskText extracts human-readable text from a finished task: the produced artifacts first,
// then the status message, then the last agent turn in the history.
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
