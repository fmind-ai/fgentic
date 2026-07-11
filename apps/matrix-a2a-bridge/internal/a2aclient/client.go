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
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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
	base   http.RoundTripper
	apiKey string
}

// generationTransport holds the cache read lock through the HTTP handoff. A quarantine or
// verified-card replacement therefore completes only after older requests have crossed the
// transport boundary, and a client copied before that state change cannot start afterwards.
type generationTransport struct {
	base       http.RoundTripper
	client     *Client
	targetID   string
	generation uint64
}

func (t *generationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.client.mu.RLock()
	defer t.client.mu.RUnlock()
	cached := t.client.cache[t.targetID]
	if !cached.ready || cached.client == nil || cached.generation != t.generation {
		return nil, fmt.Errorf("remote transport trust generation changed: %w", ErrRemoteTargetUntrusted)
	}
	return t.base.RoundTrip(req)
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
	// The bridge owns the client span, while this boundary injects its W3C context into both
	// JSON-RPC and REST requests produced by the A2A SDK.
	otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
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

// Client dials local A2A agents under a common base URL and remote agents at their exact,
// operator-configured URL. Separate transports prevent the local gateway credential from ever
// crossing an organization boundary.
type Client struct {
	baseURL          string
	log              *slog.Logger
	localHTTPClient  *http.Client
	remoteHTTPClient *http.Client
	localResolver    *agentcard.Resolver

	mu           sync.RWMutex
	cache        map[string]cachedTarget
	refreshLocks sync.Map
}

// New returns a Client that resolves agents relative to baseURL (e.g. the agentgateway proxy).
// apiKey is the bridge workload credential enforced by the A2A route; it may be empty only when
// directly dialing an unsecured development fixture or kagent endpoint.
func New(baseURL, apiKey string, log *slog.Logger) *Client {
	localHTTPClient := &http.Client{Transport: &userTransport{base: http.DefaultTransport, apiKey: apiKey}}
	remoteHTTPClient := &http.Client{
		Transport: &userTransport{base: http.DefaultTransport},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		baseURL:          strings.TrimRight(baseURL, "/"),
		log:              log,
		localHTTPClient:  localHTTPClient,
		remoteHTTPClient: remoteHTTPClient,
		localResolver:    agentcard.NewResolver(localHTTPClient),
		cache:            make(map[string]cachedTarget),
	}
}

// Call delegates text to target via A2A message/send. A non-empty contextID threads the
// conversation. ReturnImmediately keeps
// long-running agents from holding the bridge request open: a non-terminal Task is returned as
// soon as it exists and the bridge follows it with tasks/get polling.
func (c *Client) Call(ctx context.Context, target Target, text, contextID string) (Result, error) {
	client, err := c.clientFor(ctx, target)
	if err != nil {
		return Result{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if target.IsRemote() {
		msg.Extensions = []string{TokenBudgetExtensionURI}
		msg.Metadata = map[string]any{
			TokenBudgetExtensionURI: map[string]any{"maxTokens": target.TokenBudget()},
		}
	}
	if contextID != "" {
		msg.ContextID = contextID // thread the room's conversation so the agent keeps context
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		return Result{}, fmt.Errorf("a2a message/send to %s: %w", target.String(), err)
	}
	return toResult(res), nil
}

// PollTask fetches the current state of a task previously returned by Call (tasks/get).
func (c *Client) PollTask(ctx context.Context, target Target, taskID string) (Result, error) {
	client, err := c.clientFor(ctx, target)
	if err != nil {
		return Result{}, err
	}
	task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID)})
	if err != nil {
		return Result{}, fmt.Errorf("a2a tasks/get %s from %s: %w", taskID, target.String(), err)
	}
	return taskResult(task), nil
}

// ResolveAgentCard fetches the current public AgentCard for target. Local targets deliberately
// bypass the SDK-client cache. Remote cards are installed only after their pinned identity,
// endpoint, extension contract, and detached JWS signature have all been verified.
func (c *Client) ResolveAgentCard(ctx context.Context, target Target) (*a2a.AgentCard, error) {
	if !target.valid() {
		return nil, fmt.Errorf("resolve agent card: invalid target")
	}
	if target.IsRemote() {
		return c.resolveRemoteAgentCard(ctx, target)
	}
	cardPath := strings.TrimRight(target.String(), "/") + "/.well-known/agent-card.json"
	card, err := c.localResolver.Resolve(ctx, c.baseURL, agentcard.WithPath(cardPath))
	if err != nil {
		return nil, fmt.Errorf("resolve agent card %s%s: %w", c.baseURL, cardPath, err)
	}
	if card == nil {
		return nil, fmt.Errorf("resolve agent card %s%s: empty response", c.baseURL, cardPath)
	}
	return card, nil
}

// IsReady reports whether target can be called without doing network trust discovery. Local
// targets are resolved lazily on first use; a remote target is ready only while a verified card
// and SDK client are atomically installed.
func (c *Client) IsReady(target Target) bool {
	if !target.valid() {
		return false
	}
	if !target.IsRemote() {
		return true
	}
	c.mu.RLock()
	cached := c.cache[target.ID()]
	c.mu.RUnlock()
	return cached.ready && cached.client != nil
}

// clientFor resolves (and caches) an SDK client for a target by fetching its AgentCard.
// Routing baseURL + card path keeps clients pointing through agentgateway when it rewrites the
// card's advertised URL (a no-op when talking to kagent directly).
func (c *Client) clientFor(ctx context.Context, target Target) (*sdk.Client, error) {
	if !target.valid() {
		return nil, fmt.Errorf("build a2a client: invalid target")
	}

	c.mu.RLock()
	cached := c.cache[target.ID()]
	c.mu.RUnlock()
	if cached.ready && cached.client != nil {
		return cached.client, nil
	}
	if target.IsRemote() {
		return nil, fmt.Errorf("call remote target %s: %w", target.String(), ErrRemoteTargetUntrusted)
	}

	card, err := c.ResolveAgentCard(ctx, target)
	if err != nil {
		return nil, err
	}
	client, err := buildSDKClient(ctx, card, c.localHTTPClient, false)
	if err != nil {
		return nil, fmt.Errorf("build a2a client for %s: %w", target.String(), err)
	}

	c.mu.Lock()
	if existing := c.cache[target.ID()]; existing.ready && existing.client != nil {
		client = existing.client
	} else {
		c.cache[target.ID()] = cachedTarget{client: client, card: card, ready: true}
	}
	c.mu.Unlock()
	c.log.Info("resolved a2a agent", "target", target.String(), "card_name", card.Name)
	return client, nil
}

func (c *Client) remoteSDKHTTPClient(targetID string, generation uint64) *http.Client {
	return &http.Client{
		Transport: &generationTransport{
			base:       c.remoteHTTPClient.Transport,
			client:     c,
			targetID:   targetID,
			generation: generation,
		},
		CheckRedirect: c.remoteHTTPClient.CheckRedirect,
	}
}

func buildSDKClient(ctx context.Context, card *a2a.AgentCard, httpClient *http.Client, activateBudget bool) (*sdk.Client, error) {
	options := []sdk.FactoryOption{
		sdk.WithJSONRPCTransport(httpClient),
		sdk.WithRESTTransport(httpClient),
	}
	if activateBudget {
		options = append(options, sdk.WithCallInterceptors(a2aext.NewActivator(TokenBudgetExtensionURI)))
	}
	return sdk.NewFromCard(ctx, card, options...)
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
