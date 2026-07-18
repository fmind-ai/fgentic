package apgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeDelegator records A2A calls and returns a scripted reply or error.
type fakeDelegator struct {
	mu    sync.Mutex
	reply string
	err   error
	calls []call
}

type call struct{ namespace, name, text, contextID, user string }

func (f *fakeDelegator) Call(ctx context.Context, namespace, name, text, contextID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{namespace, name, text, contextID, userFromCtx(ctx)})
	return f.reply, f.err
}

func (f *fakeDelegator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// userFromCtx reads the asserted user the gateway stamps via a2a.WithUser (best-effort; the a2a
// package owns the key, so this only confirms non-empty threading in the happy path).
func userFromCtx(ctx context.Context) string {
	if v := ctx.Value(userProbeKey); v != nil {
		return v.(string)
	}
	return ""
}

type ctxKey struct{}

var userProbeKey = ctxKey{}

func newTestGateway(t *testing.T, del Delegator) *Gateway {
	t.Helper()
	reg, err := LoadRegistry(writeAgents(t, validAgents), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	g, err := New("https://fgentic.localhost", "fgentic.localhost", reg, del, prometheus.NewRegistry(), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

const createNote = `{
  "@context": "https://www.w3.org/ns/activitystreams",
  "id": "https://mastodon.example/activities/1",
  "type": "Create",
  "actor": "https://mastodon.example/users/bob",
  "object": {
    "type": "Note",
    "id": "https://mastodon.example/notes/1",
    "attributedTo": "https://mastodon.example/users/bob",
    "content": "@agent-docs-qa what is fgentic?",
    "tag": [
      {"type": "Mention", "href": "https://fgentic.localhost/ap/agents/agent-docs-qa", "name": "@agent-docs-qa@fgentic.localhost"}
    ]
  }
}`

func do(t *testing.T, g *Gateway, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebFinger(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	rec := do(t, g, http.MethodGet, "/.well-known/webfinger?resource=acct:agent-docs-qa@fgentic.localhost", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	var doc jrd
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal jrd: %v", err)
	}
	if doc.Subject != "acct:agent-docs-qa@fgentic.localhost" {
		t.Errorf("subject = %q", doc.Subject)
	}
	// One WebFinger resolution yields both the AP actor (rel=self) and the A2A card pointer (#215).
	links := make(map[string]string, len(doc.Links))
	for _, l := range doc.Links {
		links[l.Rel] = l.Href
	}
	if links["self"] != "https://fgentic.localhost/ap/agents/agent-docs-qa" {
		t.Errorf("self link = %q", links["self"])
	}
	if links[a2aAgentCardRel] != "https://fgentic.localhost/ap/agents/agent-docs-qa/agent-card.json" {
		t.Errorf("a2a card link = %q (links %+v)", links[a2aAgentCardRel], doc.Links)
	}
}

func TestWebFingerErrors(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	cases := map[string]struct {
		resource string
		want     int
	}{
		"wrong host":    {"acct:agent-docs-qa@evil.example", http.StatusBadRequest},
		"unknown agent": {"acct:agent-nope@fgentic.localhost", http.StatusNotFound},
		"not acct":      {"https://fgentic.localhost/ap/agents/agent-docs-qa", http.StatusBadRequest},
		"empty":         {"", http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := do(t, g, http.MethodGet, "/.well-known/webfinger?resource="+tc.resource, "")
			if rec.Code != tc.want {
				t.Errorf("code = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestActorDocument(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	rec := do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/activity+json") {
		t.Errorf("content-type = %q", ct)
	}
	var actor map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &actor); err != nil {
		t.Fatalf("unmarshal actor: %v", err)
	}
	if actor["type"] != "Service" {
		t.Errorf("type = %v, want Service", actor["type"])
	}
	if actor["preferredUsername"] != "agent-docs-qa" {
		t.Errorf("preferredUsername = %v", actor["preferredUsername"])
	}
	if actor["inbox"] != "https://fgentic.localhost/ap/agents/agent-docs-qa/inbox" {
		t.Errorf("inbox = %v", actor["inbox"])
	}
	if _, unknown := g.registry.Lookup("agent-none"); unknown {
		t.Fatalf("fixture drift")
	}
	if rec := do(t, g, http.MethodGet, "/ap/agents/agent-none", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown actor code = %d", rec.Code)
	}
}

func TestInboxDelegatesAndPublishes(t *testing.T) {
	del := &fakeDelegator{reply: "Fgentic is a sovereignty-first agent platform."}
	g := newTestGateway(t, del)

	rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", createNote)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/ap/agents/agent-docs-qa/activities/") {
		t.Errorf("Location = %q", loc)
	}

	if len(del.calls) != 1 {
		t.Fatalf("delegations = %d, want 1", len(del.calls))
	}
	c := del.calls[0]
	if c.namespace != "kagent" || c.name != "docs-qa" {
		t.Errorf("routed to %s/%s", c.namespace, c.name)
	}
	if !strings.Contains(c.text, "what is fgentic?") {
		t.Errorf("text = %q", c.text)
	}
	if c.contextID == "" {
		t.Errorf("contextID must thread the conversation")
	}

	out := do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa/outbox", "")
	if out.Code != http.StatusOK {
		t.Fatalf("outbox code = %d", out.Code)
	}
	var oc map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &oc); err != nil {
		t.Fatalf("unmarshal outbox: %v", err)
	}
	if oc["type"] != "OrderedCollection" {
		t.Errorf("type = %v", oc["type"])
	}
	if got := oc["totalItems"]; got != float64(1) {
		t.Errorf("totalItems = %v, want 1", got)
	}
	if !strings.Contains(out.Body.String(), "sovereignty-first") {
		t.Errorf("outbox missing reply content: %s", out.Body)
	}
	if !strings.Contains(out.Body.String(), "https://mastodon.example/notes/1") {
		t.Errorf("reply should be inReplyTo the triggering note: %s", out.Body)
	}
}

func TestInboxNotMentionedIsNoop(t *testing.T) {
	del := &fakeDelegator{reply: "should not run"}
	g := newTestGateway(t, del)
	body := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://m.example/activities/2","type":"Create","actor":"https://m.example/users/bob","object":{"type":"Note","id":"https://m.example/n/2","content":"unrelated chatter"}}`
	rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	if len(del.calls) != 0 {
		t.Errorf("must not delegate an un-mentioned note, got %d calls", len(del.calls))
	}
}

func TestInboxRejectsConflictingActivityIDReplay(t *testing.T) {
	del := &fakeDelegator{reply: "ok"}
	g := newTestGateway(t, del)
	if rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", createNote); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery code = %d", rec.Code)
	}
	changed := strings.Replace(createNote, "what is fgentic?", "run a different task", 1)
	if rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", changed); rec.Code != http.StatusConflict {
		t.Fatalf("conflicting replay code = %d, want 409", rec.Code)
	}
	if got := del.callCount(); got != 1 {
		t.Fatalf("delegations = %d, want exactly 1", got)
	}
}

func TestInboxRejections(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{reply: "ok"})
	cases := map[string]struct {
		path string
		body string
		want int
	}{
		"unknown agent":        {"/ap/agents/agent-none/inbox", createNote, http.StatusNotFound},
		"bad json":             {"/ap/agents/agent-docs-qa/inbox", "{not json", http.StatusBadRequest},
		"not create":           {"/ap/agents/agent-docs-qa/inbox", `{"type":"Like","actor":"https://m/u","object":"https://m/o"}`, http.StatusBadRequest},
		"empty content":        {"/ap/agents/agent-docs-qa/inbox", `{"type":"Create","actor":"https://m.example/users/bob","object":{"type":"Note","id":"https://m.example/n/9","content":""}}`, http.StatusBadRequest},
		"missing activity id":  {"/ap/agents/agent-docs-qa/inbox", `{"type":"Create","actor":"https://m.example/users/bob","object":{"type":"Note","id":"https://m.example/n/9","content":"@agent-docs-qa help"}}`, http.StatusBadRequest},
		"insecure activity id": {"/ap/agents/agent-docs-qa/inbox", `{"id":"http://m.example/activities/9","type":"Create","actor":"https://m.example/users/bob","object":{"type":"Note","id":"https://m.example/n/9","content":"@agent-docs-qa help"}}`, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := do(t, g, http.MethodPost, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Errorf("code = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestInboxDelegationError(t *testing.T) {
	del := &fakeDelegator{err: errors.New("agent unreachable")}
	g := newTestGateway(t, del)
	rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", createNote)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want prompt 202", rec.Code)
	}
	if out := do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa/outbox", ""); strings.Contains(out.Body.String(), "activities") {
		t.Errorf("no reply must be published on delegation error: %s", out.Body)
	}
}

func TestDeriveContextIDNeverCrossesAgents(t *testing.T) {
	a := deriveContextID("agent-docs-qa", "actor", "thread")
	b := deriveContextID("agent-scribe", "actor", "thread")
	if a == b {
		t.Errorf("contextId must differ per agent")
	}
	if a != deriveContextID("agent-docs-qa", "actor", "thread") {
		t.Errorf("contextId must be stable for the same triple")
	}
}

func TestHealthEndpoints(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := do(t, g, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("%s code = %d", path, rec.Code)
		}
	}
}
