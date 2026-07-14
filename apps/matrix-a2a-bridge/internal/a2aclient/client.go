// Package a2aclient is a thin, bridge-focused wrapper over the official A2A Go SDK
// (github.com/a2aproject/a2a-go/v2). It resolves an agent's AgentCard, sends a single
// non-streaming message (message/send), extracts the reply text from the returned
// Task-or-Message sum type, and polls tasks/get for long-running tasks (SPEC §6).
// Streaming is intentionally not used (fire-and-forget delegation).
package a2aclient

import (
	"context"
	"encoding/json"
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
	resp, err := t.base.RoundTrip(req)
	// Record which extensions the server echoed as activated (A2A-Extensions response header) so a
	// remote delegation can audit what negotiation settled on. Best-effort and observation-only.
	if err == nil && resp != nil {
		if capture := activationCaptureFrom(req.Context()); capture != nil {
			capture.record(resp.Header.Values(a2a.SvcParamExtensions))
		}
	}
	return resp, err
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
	// InputRequired marks a task paused in TASK_STATE_INPUT_REQUIRED: Text is the agent's question
	// and the task resumes by re-sending message/send with the same TaskID+ContextID (#116).
	InputRequired bool
	// AuthRequired marks a task paused in TASK_STATE_AUTH_REQUIRED. The bridge does not forward
	// caller credentials, so it terminates the delegation with an honest notice rather than resuming.
	AuthRequired bool
	// ActivatedExtensions is the A2A extension set the remote server echoed as activated on this
	// message/send (the `A2A-Extensions` response header). Empty for local targets or a server that
	// does not echo; it feeds the delegation audit, never a control decision.
	ActivatedExtensions []string
	// Files, Data, and Links are the non-text content the agent produced (#115): raw byte parts
	// (candidate Matrix media uploads), structured data parts pre-rendered as compact JSON, and URL
	// parts kept as untrusted labeled links the bridge never fetches server-side. They carry no
	// policy decision — the bridge applies its MIME/size gate before anything reaches a room.
	Files []ResultFile
	Data  []string
	Links []ResultLink
}

// ResultFile is one raw-bytes part the agent emitted (an A2A Raw part), a candidate for upload to the
// Matrix content repository. Name and MIMEType are the agent's self-declared metadata and are treated
// as untrusted hints, not verified facts.
type ResultFile struct {
	Name     string
	MIMEType string
	Bytes    []byte
}

// ResultLink is one URL part the agent emitted. The bridge surfaces it as a labeled untrusted link
// and never dereferences it server-side (an agent-controlled URL is an SSRF vector).
type ResultLink struct {
	Label    string
	URL      string
	MIMEType string
}

// InboundFile is a room-attached file the bridge forwards to an agent as an A2A Raw part (#115). The
// caller (the bridge) is responsible for having applied its media policy before constructing these;
// the client transports the bytes verbatim.
type InboundFile struct {
	Name     string
	MIMEType string
	Bytes    []byte
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
	// remoteTransports memoizes the per-target mTLS RoundTripper (keyed by target ID) so the card
	// fetch and the SDK client reuse one connection pool instead of cloning a fresh transport on every
	// periodic refresh (#244). A cert rotation yields a new target ID and therefore a new entry.
	remoteTransports sync.Map
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
func (c *Client) Call(ctx context.Context, target Target, text, contextID string, files []InboundFile) (Result, error) {
	return c.send(ctx, target, text, contextID, "", files)
}

// Continue resumes a task paused in TASK_STATE_INPUT_REQUIRED by re-sending message/send with the
// same taskID and contextID (A2A continuation semantics — #116). text is the room member's answer.
func (c *Client) Continue(ctx context.Context, target Target, text, contextID, taskID string) (Result, error) {
	return c.send(ctx, target, text, contextID, taskID, nil)
}

// send is the shared message/send path for a new delegation (Call, taskID empty) and a resumed one
// (Continue, taskID set). A non-empty taskID is stamped onto the message so the agent continues its
// existing task rather than starting a fresh one. files, when present, ride as A2A Raw parts (#115);
// the bridge gates them by its media policy before they reach here.
func (c *Client) send(ctx context.Context, target Target, text, contextID, taskID string, files []InboundFile) (Result, error) {
	client, err := c.clientFor(ctx, target)
	if err != nil {
		return Result{}, err
	}
	var capture *activationCapture
	parts := make([]*a2a.Part, 0, 1+len(files))
	parts = append(parts, a2a.NewTextPart(text))
	for _, f := range files {
		part := a2a.NewRawPart(f.Bytes)
		part.Filename = f.Name
		part.MediaType = f.MIMEType
		parts = append(parts, part)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, parts...)
	if target.IsRemote() {
		// Request the negotiated extension set (token-budget base + configured extras); the SDK
		// activator drops any the card does not advertise. Token-budget is the only one carrying
		// request metadata today — data-only extensions ride the signed card, not the message.
		activated := target.ActivatedExtensions()
		msg.Extensions = activated
		msg.Metadata = map[string]any{
			TokenBudgetExtensionURI: map[string]any{"maxTokens": target.TokenBudget()},
		}
		ctx, capture = withActivationCapture(ctx, activated)
	}
	if contextID != "" {
		msg.ContextID = contextID // thread the room's conversation so the agent keeps context
	}
	if taskID != "" {
		msg.TaskID = a2a.TaskID(taskID) // continue the existing input-required task (#116)
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		return Result{}, fmt.Errorf("a2a message/send to %s: %w", target.String(), err)
	}
	result := toResult(res)
	if capture != nil {
		result.ActivatedExtensions = capture.snapshot()
	}
	return result, nil
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

// CancelTask asks the agent to stop a task previously returned by Call (tasks/cancel). It is
// best-effort: the bridge stops polling and reports the cancellation to the room regardless, but a
// successful call also releases the agent's own resources and halts token burn at the source. Like
// Call/PollTask it routes local targets through agentgateway and pinned remotes to their exact URL.
func (c *Client) CancelTask(ctx context.Context, target Target, taskID string) error {
	client, err := c.clientFor(ctx, target)
	if err != nil {
		return err
	}
	if _, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)}); err != nil {
		return fmt.Errorf("a2a tasks/cancel %s on %s: %w", taskID, target.String(), err)
	}
	return nil
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
	// Local targets activate no extensions: their token budget and capabilities are governed by the
	// local gateway, not a partner-enforced request contract.
	client, err := buildSDKClient(ctx, card, c.localHTTPClient, nil)
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

func (c *Client) remoteSDKHTTPClient(target Target, generation uint64) *http.Client {
	return &http.Client{
		Transport: &generationTransport{
			base:       c.remoteUserTransport(target),
			client:     c,
			targetID:   target.ID(),
			generation: generation,
		},
		CheckRedirect: c.remoteHTTPClient.CheckRedirect,
	}
}

// remoteUserTransport returns the RoundTripper for dialing a remote target. Without configured mTLS
// it reuses the shared remote transport (a userTransport with no API key), preserving existing
// behavior. With mTLS (#244) it wraps a per-target http.Transport — cloned from DefaultTransport to
// keep its timeouts/proxy behavior — pinned to the mapping's client certificate and optional server
// roots, in its own userTransport that likewise carries no local gateway credential.
func (c *Client) remoteUserTransport(target Target) http.RoundTripper {
	tlsConfig := target.clientTLSConfig()
	if tlsConfig == nil {
		return c.remoteHTTPClient.Transport
	}
	if cached, ok := c.remoteTransports.Load(target.ID()); ok {
		return cached.(http.RoundTripper)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	shared, _ := c.remoteTransports.LoadOrStore(target.ID(), &userTransport{base: transport})
	return shared.(http.RoundTripper)
}

func buildSDKClient(ctx context.Context, card *a2a.AgentCard, httpClient *http.Client, activatedExtensions []string) (*sdk.Client, error) {
	options := []sdk.FactoryOption{
		sdk.WithJSONRPCTransport(httpClient),
		sdk.WithRESTTransport(httpClient),
	}
	if len(activatedExtensions) > 0 {
		// The activator sends A2A-Extensions for exactly the intersection of this set and the card's
		// declared extensions, so activating a superset is safe.
		options = append(options, sdk.WithCallInterceptors(a2aext.NewActivator(activatedExtensions...)))
	}
	return sdk.NewFromCard(ctx, card, options...)
}

// activationCapture collects the A2A-Extensions a remote server echoed as activated on the most
// recent response for one message/send, so the delegation audit can record it. It is shared across
// the transport boundary through the call context and is safe for concurrent access.
type activationCapture struct {
	requested []string // the set the bridge requested; bounds and filters what may be recorded
	mu        sync.Mutex
	set       []string
}

// record keeps only the extensions the bridge actually requested that the server also echoed, in
// deterministic requested order. The response header is not covered by the card signature, so this
// bounds the audit field to a known small set and drops any server-injected or oversized URIs
// rather than trusting the wire (docs/bridge.md §6).
func (a *activationCapture) record(headerValues []string) {
	echoed := parseExtensionHeader(headerValues)
	if len(echoed) == 0 {
		return
	}
	echoedSet := make(map[string]struct{}, len(echoed))
	for _, uri := range echoed {
		echoedSet[uri] = struct{}{}
	}
	confirmed := make([]string, 0, len(a.requested))
	for _, uri := range a.requested {
		if _, ok := echoedSet[uri]; ok {
			confirmed = append(confirmed, uri)
		}
	}
	if len(confirmed) == 0 {
		return // nothing we requested was confirmed; keep any earlier non-empty capture
	}
	a.mu.Lock()
	a.set = confirmed
	a.mu.Unlock()
}

func (a *activationCapture) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.set) == 0 {
		return nil
	}
	return append([]string(nil), a.set...)
}

type activationCaptureKey struct{}

// withActivationCapture attaches a fresh capture to ctx, bounded to the requested extension set.
// Each call gets its own, so concurrent delegations never cross-contaminate their activation sets.
func withActivationCapture(ctx context.Context, requested []string) (context.Context, *activationCapture) {
	capture := &activationCapture{requested: requested}
	return context.WithValue(ctx, activationCaptureKey{}, capture), capture
}

func activationCaptureFrom(ctx context.Context) *activationCapture {
	capture, _ := ctx.Value(activationCaptureKey{}).(*activationCapture)
	return capture
}

// parseExtensionHeader splits comma-separated and multi-valued A2A-Extensions header values into a
// de-duplicated, order-preserving URI list.
func parseExtensionHeader(headerValues []string) []string {
	seen := make(map[string]struct{})
	var uris []string
	for _, value := range headerValues {
		for _, part := range strings.Split(value, ",") {
			uri := strings.TrimSpace(part)
			if uri == "" {
				continue
			}
			if _, dup := seen[uri]; dup {
				continue
			}
			seen[uri] = struct{}{}
			uris = append(uris, uri)
		}
	}
	return uris
}

// toResult maps a SendMessageResult (a *a2a.Message or *a2a.Task sum type) to a Result.
func toResult(res a2a.SendMessageResult) Result {
	switch v := res.(type) {
	case *a2a.Message:
		r := Result{Text: partsText(v.Parts), ContextID: v.ContextID, Terminal: true}
		r.Files, r.Data, r.Links = extractParts(v.Parts)
		return r
	case *a2a.Task:
		return taskResult(v)
	default:
		return Result{Terminal: true}
	}
}

func taskResult(t *a2a.Task) Result {
	r := Result{
		ContextID:     t.ContextID,
		TaskID:        string(t.ID),
		Terminal:      t.Status.State.Terminal(),
		Failed:        t.Status.State != a2a.TaskStateCompleted && t.Status.State.Terminal(),
		InputRequired: t.Status.State == a2a.TaskStateInputRequired,
		AuthRequired:  t.Status.State == a2a.TaskStateAuthRequired,
	}
	if r.Terminal {
		r.Text = taskText(t)
		// A finished task's file/data/link products live in its artifacts (SPEC §6): extract them for
		// the bridge to post as media, deliberately not from status/history (those are working turns).
		for _, a := range t.Artifacts {
			files, data, links := extractParts(a.Parts)
			r.Files = append(r.Files, files...)
			r.Data = append(r.Data, data...)
			r.Links = append(r.Links, links...)
		}
	} else if t.Status.Message != nil {
		// Interim status, e.g. "working"; for input-required this is the agent's question.
		r.Text = partsText(t.Status.Message.Parts)
	}
	return r
}

// extractParts splits a part list into the bridge's non-text content buckets (#115): Raw parts become
// candidate media files, URL parts become untrusted links (never fetched), and Data parts are
// rendered to compact JSON for a fenced code block. Text parts are ignored here — text is handled by
// partsText/taskText. A part matches at most one bucket, tested Raw→URL→Data so a Raw part with an
// incidental empty URL is still treated as a file.
func extractParts(parts a2a.ContentParts) (files []ResultFile, data []string, links []ResultLink) {
	for _, p := range parts {
		switch {
		case len(p.Raw()) > 0:
			files = append(files, ResultFile{Name: p.Filename, MIMEType: p.MediaType, Bytes: p.Raw()})
		case p.URL() != "":
			links = append(links, ResultLink{Label: p.Filename, URL: string(p.URL()), MIMEType: p.MediaType})
		case p.Data() != nil:
			if encoded, err := json.Marshal(p.Data()); err == nil {
				data = append(data, string(encoded))
			}
		}
	}
	return files, data, links
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
