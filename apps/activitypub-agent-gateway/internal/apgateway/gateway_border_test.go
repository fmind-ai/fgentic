package apgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fmind/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind/activitypub-agent-gateway/internal/policy"
)

// gatewayWithBorder wires a test gateway whose inbox is gated by the federation border.
func gatewayWithBorder(t *testing.T, del Delegator, pub ed25519.PublicKey, policyBody string) *Gateway {
	t.Helper()
	g := newTestGateway(t, del)
	store := policy.NewStore(writePolicyFile(t, policyBody), slog.Default())
	verifier := httpsig.NewVerifier(staticResolver{key: pub, owner: borderTestActor}, time.Hour)
	g.UseBorder(NewBorder(verifier, store, slog.Default()))
	return g
}

func TestInboxBorderEnforcement(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	body := []byte(createNote) // actor in createNote is borderTestActor

	t.Run("signed allowlisted inbound delegates", func(t *testing.T) {
		del := &fakeDelegator{reply: "hi"}
		g := gatewayWithBorder(t, del, pub, `{"version":1,"allowed_domains":["mastodon.example"]}`)
		req := signedInbox(t, priv, body)
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		if rec.Code != 202 {
			t.Fatalf("code = %d, want 202 (body %s)", rec.Code, rec.Body)
		}
		if len(del.calls) != 1 {
			t.Errorf("delegations = %d, want 1", len(del.calls))
		}
	})

	t.Run("unsigned inbound is dropped with zero A2A calls", func(t *testing.T) {
		del := &fakeDelegator{reply: "hi"}
		g := gatewayWithBorder(t, del, pub, `{"version":1,"allowed_domains":["mastodon.example"]}`)
		rec := do(t, g, "POST", "/ap/agents/agent-docs-qa/inbox", createNote)
		if rec.Code != 403 {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
		if len(del.calls) != 0 {
			t.Errorf("unsigned must not delegate, got %d calls", len(del.calls))
		}
	})

	t.Run("off-allowlist signed inbound is dropped with zero A2A calls", func(t *testing.T) {
		del := &fakeDelegator{reply: "hi"}
		g := gatewayWithBorder(t, del, pub, `{"version":1,"allowed_domains":["other.example"]}`)
		req := signedInbox(t, priv, body)
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
		if len(del.calls) != 0 {
			t.Errorf("off-allowlist must not delegate, got %d calls", len(del.calls))
		}
	})
}
