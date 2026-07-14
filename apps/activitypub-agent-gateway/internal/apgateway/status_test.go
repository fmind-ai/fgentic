package apgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/budget"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/delivery"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/policy"
)

// newStatusGateway builds a gateway with the follow-to-subscribe status feed enabled, returning the
// gateway and the signer's public key (to verify delivered status Notes).
func newStatusGateway(t *testing.T, client *http.Client, maxPerWindow int, border *Border) (*Gateway, ed25519.PublicKey) {
	t.Helper()
	registry, err := LoadRegistry(writeAgents(t, validAgents), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	g, err := New("https://fgentic.localhost", "fgentic.localhost", registry, &fakeDelegator{}, prometheus.NewRegistry(), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := integrity.NewSigner(priv, "ed25519-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	g.UseSigner(signer)
	g.UseDelivery(delivery.New(client, priv, slog.Default()), client)
	fixed := time.Unix(1_700_000_000, 0)
	g.UseStatusFeed(budget.NewWithClock(time.Minute, 64, func() time.Time { return fixed }), uint64(maxPerWindow))
	if border != nil {
		g.UseBorder(border)
	}
	return g, pub
}

func fireAlert(t *testing.T, g *Gateway, ghost, alertname, summary string) int {
	t.Helper()
	payload := fmt.Sprintf(`{"alerts":[{"status":"firing","labels":{"alertname":%q,"agent":%q},"annotations":{"summary":%q}}]}`,
		alertname, ghost, summary)
	req := httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	g.AlertsHandler()(rec, req)
	return rec.Code
}

// followAgent subscribes peer to agent-docs-qa via a Follow to its inbox.
func followAgent(t *testing.T, g *Gateway, peer *remotePeer) {
	t.Helper()
	const ghost = "agent-docs-qa"
	follow := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","id":%q,"type":"Follow","actor":%q,"object":"https://fgentic.localhost/ap/agents/%s"}`,
		peer.server.URL+"/activities/1", peer.actor(), ghost)
	if rec := do(t, g, http.MethodPost, "/ap/agents/"+ghost+"/inbox", follow); rec.Code != http.StatusAccepted {
		t.Fatalf("follow code = %d", rec.Code)
	}
}

func TestStatusFeedSubscribeAndReceiveSignedNote(t *testing.T) {
	peer := newRemotePeer(t)
	g, pub := newStatusGateway(t, peer.server.Client(), 6, nil)

	followAgent(t, g, peer)
	if g.followers.count(agentFollowerKey("agent-docs-qa")) != 1 {
		t.Fatalf("subscriber not recorded")
	}
	// The Accept was delivered to the follower.
	if got := countTypes(peer.typesDelivered())["Accept"]; got != 1 {
		t.Fatalf("Accept deliveries = %d, want 1", got)
	}

	// A simulated cost alert fires: the follower receives a signed status Create(Note).
	if code := fireAlert(t, g, "agent-docs-qa", "LLMTokenBurnHigh", "token burn exceeded budget"); code != http.StatusNoContent {
		t.Fatalf("alert code = %d", code)
	}
	creates := 0
	for _, d := range peer.deliveries() {
		if d["type"] != "Create" {
			continue
		}
		creates++
		// The delivered status Note carries a verifiable FEP-8b32 proof.
		if _, err := integrity.Verify(d, pub); err != nil {
			t.Errorf("status Note proof did not verify: %v", err)
		}
		obj := d["object"].(map[string]any)
		if !strings.Contains(obj["content"].(string), "token burn") {
			t.Errorf("status content = %v", obj["content"])
		}
	}
	if creates != 1 {
		t.Errorf("status Create deliveries = %d, want 1", creates)
	}
}

func TestStatusFeedUndoStopsDelivery(t *testing.T) {
	peer := newRemotePeer(t)
	g, _ := newStatusGateway(t, peer.server.Client(), 6, nil)
	followAgent(t, g, peer)

	fireAlert(t, g, "agent-docs-qa", "A1", "first")
	before := countTypes(peer.typesDelivered())["Create"]
	if before != 1 {
		t.Fatalf("first alert deliveries = %d, want 1", before)
	}

	// Unsubscribe via Undo(Follow).
	undo := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","type":"Undo","actor":%q,"object":{"type":"Follow","actor":%q,"object":"https://fgentic.localhost/ap/agents/agent-docs-qa"}}`,
		peer.actor(), peer.actor())
	if rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", undo); rec.Code != http.StatusAccepted {
		t.Fatalf("undo code = %d", rec.Code)
	}
	if g.followers.count(agentFollowerKey("agent-docs-qa")) != 0 {
		t.Fatalf("Undo must drop the subscriber")
	}

	fireAlert(t, g, "agent-docs-qa", "A2", "second")
	after := countTypes(peer.typesDelivered())["Create"]
	if after != before {
		t.Errorf("after Undo, no further status Notes should be delivered (before=%d after=%d)", before, after)
	}
}

func TestStatusFeedRateLimits(t *testing.T) {
	peer := newRemotePeer(t)
	g, _ := newStatusGateway(t, peer.server.Client(), 2, nil) // cap 2 per window
	followAgent(t, g, peer)

	for i := 0; i < 5; i++ {
		fireAlert(t, g, "agent-docs-qa", fmt.Sprintf("A%d", i), "burst")
	}
	if got := countTypes(peer.typesDelivered())["Create"]; got != 2 {
		t.Errorf("delivered status Notes = %d, want 2 (rate limited; a flapping alert cannot spam)", got)
	}
}

func TestStatusFeedIgnoresUnknownAgentAndResolved(t *testing.T) {
	peer := newRemotePeer(t)
	g, _ := newStatusGateway(t, peer.server.Client(), 6, nil)
	followAgent(t, g, peer)

	// An alert for an unlisted agent is ignored.
	fireAlert(t, g, "agent-nope", "X", "unknown")
	// A resolved (not firing) alert is ignored.
	resolved := `{"alerts":[{"status":"resolved","labels":{"alertname":"X","agent":"agent-docs-qa"},"annotations":{"summary":"ok"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(resolved))
	g.AlertsHandler()(httptest.NewRecorder(), req)

	if got := countTypes(peer.typesDelivered())["Create"]; got != 0 {
		t.Errorf("no status Note should be delivered for unknown/resolved alerts, got %d", got)
	}
}

func TestStatusFollowBorderGated(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store := policy.NewStore(writePolicyFile(t, `{"version":1,"allowed_domains":["other.example"]}`), slog.Default())
	verifier := httpsig.NewVerifier(staticResolver{key: pub, owner: borderTestActor}, time.Hour)
	border := NewBorder(verifier, store, slog.Default())

	g, _ := newStatusGateway(t, http.DefaultClient, 6, border)
	follow := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","id":"https://mastodon.example/a/1","type":"Follow","actor":%q,"object":"https://fgentic.localhost/ap/agents/agent-docs-qa"}`, borderTestActor)
	req := signedGroupReq(t, priv, borderTestActor, "/ap/agents/agent-docs-qa/inbox", []byte(follow))
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-allowlist follow code = %d, want 403", rec.Code)
	}
	if g.followers.count(agentFollowerKey("agent-docs-qa")) != 0 {
		t.Errorf("off-allowlist actor must not be subscribed")
	}
}
