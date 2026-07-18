package apgateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/budget"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/delivery"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/policy"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/testhttp"
)

const validGroups = `schemaVersion: 1
groups:
  collab:
    name: Fgentic collaboration
    description: Cross-org agent collaboration room.
`

func writeGroups(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "groups.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write groups: %v", err)
	}
	return path
}

// remotePeer is an in-process Fediverse actor: it serves its actor document (with an inbox) and
// captures the signed activities the group delivers to that inbox.
type remotePeer struct {
	server    *httptest.Server
	host      string
	mu        sync.Mutex
	delivered []map[string]any
}

func newRemotePeer(t *testing.T, host string) *remotePeer {
	t.Helper()
	p := &remotePeer{host: host}
	mux := http.NewServeMux()
	p.server = httptest.NewTLSServer(mux)
	t.Cleanup(p.server.Close)
	mux.HandleFunc("GET /users/bob", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id":%q,"type":"Person","inbox":%q}`, p.actor(), p.inbox())
	})
	mux.HandleFunc("POST /users/bob/inbox", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err == nil {
			p.mu.Lock()
			p.delivered = append(p.delivered, doc)
			p.mu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return p
}

func (p *remotePeer) actor() string { return testhttp.URL(p.host) + "/users/bob" }
func (p *remotePeer) inbox() string { return testhttp.URL(p.host) + "/users/bob/inbox" }

func (p *remotePeer) client(t *testing.T) *http.Client {
	t.Helper()
	return testhttp.Client(t, map[string]*httptest.Server{p.host: p.server})
}

func (p *remotePeer) deliveries() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, len(p.delivered))
	copy(out, p.delivered)
	return out
}

func (p *remotePeer) typesDelivered() []string {
	types := []string{}
	for _, d := range p.deliveries() {
		if s, ok := d["type"].(string); ok {
			types = append(types, s)
		}
	}
	return types
}

// newGroupGateway builds a gateway with the Group surface enabled (signer + deliverer + groups),
// dialing peers through client. When border is non-nil it also gates inbound group traffic.
func newGroupGateway(t *testing.T, del Delegator, client *http.Client, border *Border) *Gateway {
	t.Helper()
	registry, err := LoadRegistry(writeAgents(t, validAgents), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	g, err := New("https://fgentic.localhost", "fgentic.localhost", registry, del, prometheus.NewRegistry(), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, objectPriv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := integrity.NewSigner(objectPriv, "ed25519-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	g.UseSigner(signer)
	groups, err := LoadGroupRegistry(writeGroups(t, validGroups))
	if err != nil {
		t.Fatalf("LoadGroupRegistry: %v", err)
	}
	httpPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Generate HTTP-signature key: %v", err)
	}
	if err := g.UseGroups(groups, delivery.New(client, httpPriv, slog.Default()), client); err != nil {
		t.Fatalf("UseGroups: %v", err)
	}
	if border != nil {
		g.UseBorder(border)
	}
	return g
}

func TestGroupCollaborationHappyPath(t *testing.T) {
	author := newRemotePeer(t, "author.example.com")     // posts the message
	observer := newRemotePeer(t, "observer.example.com") // a second follower who should see the fan-out
	del := &fakeDelegator{reply: "Fgentic is a sovereignty-first agent platform."}
	client := testhttp.Client(t, map[string]*httptest.Server{
		author.host:   author.server,
		observer.host: observer.server,
	})
	g := newGroupGateway(t, del, client, nil) // border nil: focus on the AP-native flow
	groupActor := "https://fgentic.localhost/ap/groups/collab"

	// 1. Both remotes follow the group; each gets a signed Accept and is recorded.
	for i, peer := range []*remotePeer{author, observer} {
		follow := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","id":%q,"type":"Follow","actor":%q,"object":%q}`,
			fmt.Sprintf("%s/activities/%d", testhttp.URL(peer.host), i), peer.actor(), groupActor)
		if rec := do(t, g, http.MethodPost, "/ap/groups/collab/inbox", follow); rec.Code != http.StatusAccepted {
			t.Fatalf("follow code = %d", rec.Code)
		}
	}
	if g.followers.count("collab") != 2 {
		t.Fatalf("followers = %d, want 2", g.followers.count("collab"))
	}

	// 2. The author posts a message mentioning an agent.
	create := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","id":%q,"type":"Create","actor":%q,"object":{"id":%q,"type":"Note","attributedTo":%q,"content":"@agent-docs-qa what is fgentic?"}}`,
		testhttp.URL(author.host)+"/activities/9", author.actor(), testhttp.URL(author.host)+"/notes/1", author.actor())
	if rec := do(t, g, http.MethodPost, "/ap/groups/collab/inbox", create); rec.Code != http.StatusAccepted {
		t.Fatalf("create code = %d", rec.Code)
	}

	// One governed A2A delegation happened.
	if len(del.calls) != 1 {
		t.Fatalf("delegations = %d, want 1", len(del.calls))
	}

	// The observer sees the post fanned out AND the agent reply (2 Announce); it also got an Accept.
	obs := countTypes(observer.typesDelivered())
	if obs["Accept"] != 1 || obs["Announce"] != 2 {
		t.Errorf("observer deliveries = %v, want 1 Accept + 2 Announce (post + reply)", observer.typesDelivered())
	}
	// The author is NOT echoed its own post, but DOES receive the agent reply Announce.
	auth := countTypes(author.typesDelivered())
	if auth["Announce"] != 1 {
		t.Errorf("author Announce = %d, want 1 (the reply only, no self-echo)", auth["Announce"])
	}
	// The agent reply carries the reply content to followers.
	if body := mustJSON(t, observer.deliveries()); !bytes.Contains(body, []byte("sovereignty-first")) {
		t.Errorf("agent reply not fanned out to the group")
	}
}

func TestFetchInboxRejectsPrivateDestination(t *testing.T) {
	t.Parallel()
	const host = "private-inbox.example.com"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"https://private-inbox.example.com/users/bob","inbox":"https://169.254.169.254/latest/meta-data"}`)
	}))
	defer srv.Close()
	client := testhttp.Client(t, map[string]*httptest.Server{host: srv})
	g := newGroupGateway(t, &fakeDelegator{}, client, nil)

	if _, err := g.fetchInbox(context.Background(), testhttp.URL(host)+"/users/bob"); err == nil {
		t.Fatal("private follower inbox must be rejected")
	}
}

func countTypes(types []string) map[string]int {
	counts := map[string]int{}
	for _, ty := range types {
		counts[ty]++
	}
	return counts
}

func TestGroupBorderDropsOffAllowlist(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	// Border allows only other.example, so borderTestActor (mastodon.example) is off-allowlist.
	store := policy.NewStore(writePolicyFile(t, `{"version":1,"allowed_domains":["other.example"]}`), slog.Default())
	verifier := httpsig.NewVerifier(staticResolver{key: pub, owner: borderTestActor}, time.Hour)
	border := NewBorder(verifier, store, slog.Default())

	del := &fakeDelegator{reply: "should not run"}
	g := newGroupGateway(t, del, http.DefaultClient, border)

	create := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","id":"https://mastodon.example/a/1","type":"Create","actor":%q,"object":{"id":"https://mastodon.example/n/1","type":"Note","attributedTo":%q,"content":"@agent-docs-qa hi"}}`,
		borderTestActor, borderTestActor)
	req := signedGroupReq(t, priv, borderTestActor, "/ap/groups/collab/inbox", []byte(create))
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-allowlist code = %d, want 403", rec.Code)
	}
	if len(del.calls) != 0 {
		t.Errorf("off-allowlist must not delegate, got %d", len(del.calls))
	}
}

func TestGroupBorderDropsOverBudget(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	// reservation 1000, pool 1000: the first mention fits, the second exhausts the window.
	store := policy.NewStore(writePolicyFile(t, `{"version":1,"allowed_domains":["mastodon.example"],"budgets":{"reservation_tokens":1000,"domains":{"mastodon.example":1000}}}`), slog.Default())
	verifier := httpsig.NewVerifier(staticResolver{key: pub, owner: borderTestActor}, time.Hour)
	border := NewBorder(verifier, store, slog.Default())
	fixed := time.Unix(1_700_000_000, 0)
	border.RequireBudget(budget.NewWithClock(time.Minute, 64, func() time.Time { return fixed }))

	del := &fakeDelegator{reply: "ok"}
	g := newGroupGateway(t, del, http.DefaultClient, border)

	post := func(id string) int {
		create := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","id":%q,"type":"Create","actor":%q,"object":{"id":%q,"type":"Note","attributedTo":%q,"content":"@agent-docs-qa hi"}}`,
			id, borderTestActor, id+"#note", borderTestActor)
		req := signedGroupReq(t, priv, borderTestActor, "/ap/groups/collab/inbox", []byte(create))
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("https://mastodon.example/a/1"); code != http.StatusAccepted {
		t.Fatalf("first post code = %d", code)
	}
	if code := post("https://mastodon.example/a/2"); code != http.StatusAccepted {
		t.Fatalf("second post code = %d", code)
	}
	// The first mention reserves and delegates; the second is over budget and is dropped before A2A.
	if len(del.calls) != 1 {
		t.Errorf("delegations = %d, want 1 (second mention over budget, no A2A)", len(del.calls))
	}
}

func TestGroupWebFingerResolves(t *testing.T) {
	g := newGroupGateway(t, &fakeDelegator{}, http.DefaultClient, nil)
	rec := do(t, g, http.MethodGet, "/.well-known/webfinger?resource=acct:collab@fgentic.localhost", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var doc jrd
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Links) != 1 || doc.Links[0].Href != "https://fgentic.localhost/ap/groups/collab" {
		t.Errorf("group webfinger links = %+v", doc.Links)
	}
}

func TestGroupActorPublishesKey(t *testing.T) {
	g := newGroupGateway(t, &fakeDelegator{}, http.DefaultClient, nil)
	rec := do(t, g, http.MethodGet, "/ap/groups/collab", "")
	var actor map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &actor); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if actor["type"] != "Group" {
		t.Errorf("type = %v, want Group", actor["type"])
	}
	pk, ok := actor["publicKey"].(map[string]any)
	if !ok || pk["publicKeyPem"] == "" || pk["publicKeyPem"] == nil {
		t.Errorf("group must publish an HTTP-Signature publicKey, got %v", actor["publicKey"])
	}
}

// signedGroupReq builds an Ed25519 HTTP-signed POST to a group path for actorURI.
func signedGroupReq(t *testing.T, priv ed25519.PrivateKey, actorURI, path string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://fgentic.localhost"+path, bytes.NewReader(body))
	req.Host = "fgentic.localhost"
	if err := httpsig.Sign(req, body, actorURI+"#main-key", priv, time.Now()); err != nil {
		t.Fatalf("sign group request: %v", err)
	}
	return req
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
