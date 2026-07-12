package apgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fmind/activitypub-agent-gateway/internal/budget"
	"github.com/fmind/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind/activitypub-agent-gateway/internal/policy"
)

// gatewayWithBudgetBorder builds a gateway whose border enforces the allowlist plus a token budget,
// verifying signatures from priv. A fixed clock keeps successive reservations in one window so the
// budget is deterministic. It returns the gateway, its delegator, and the metrics registry.
func gatewayWithBudgetBorder(t *testing.T, policyBody string, priv ed25519.PrivateKey) (*Gateway, *fakeDelegator, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	registry, err := LoadRegistry(writeAgents(t, validAgents), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	del := &fakeDelegator{reply: "hi"}
	g, err := New("https://fgentic.localhost", "fgentic.localhost", registry, del, reg, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pub := priv.Public().(ed25519.PublicKey)
	store := policy.NewStore(writePolicyFile(t, policyBody), slog.Default())
	verifier := httpsig.NewVerifier(staticResolver{key: pub, owner: borderTestActor}, time.Hour)
	border := NewBorder(verifier, store, slog.Default())
	fixed := time.Unix(1_700_000_000, 0)
	border.RequireBudget(budget.NewWithClock(time.Minute, 64, func() time.Time { return fixed }))
	g.UseBorder(border)
	return g, del, reg
}

func reservationCount(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "apgateway_budget_reservations_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == outcome {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestInboxTokenBudgetAdmission(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// reservation 1000, domain pool 1500: the first delegation fits, the second is over budget.
	policyBody := `{"version":1,"allowed_domains":["mastodon.example"],"budgets":{"reservation_tokens":1000,"domains":{"mastodon.example":1500}}}`
	g, del, reg := gatewayWithBudgetBorder(t, policyBody, priv)

	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, signedInbox(t, priv, []byte(createNote)))
		return rec
	}

	if rec := serve(); rec.Code != 202 {
		t.Fatalf("in-budget code = %d, body = %s", rec.Code, rec.Body)
	}
	if len(del.calls) != 1 {
		t.Fatalf("in-budget must delegate once, got %d", len(del.calls))
	}
	if rec := serve(); rec.Code != 403 {
		t.Fatalf("over-budget code = %d, want 403", rec.Code)
	}
	if len(del.calls) != 1 {
		t.Errorf("over-budget must NOT delegate (no LLM spend), got %d calls", len(del.calls))
	}
	if got := reservationCount(t, reg, "reserved"); got != 1 {
		t.Errorf("reserved reservations = %v, want 1", got)
	}
	if got := reservationCount(t, reg, "rejected"); got != 1 {
		t.Errorf("rejected reservations = %v, want 1", got)
	}
}

func TestInboxDeniesUnbudgetedAllowlistedDomain(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// mastodon.example is allowlisted but has no budget entry and no default → deny-by-default.
	policyBody := `{"version":1,"allowed_domains":["mastodon.example"],"budgets":{"reservation_tokens":1000,"domains":{"other.example":6000}}}`
	g, del, reg := gatewayWithBudgetBorder(t, policyBody, priv)

	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, signedInbox(t, priv, []byte(createNote)))
	if rec.Code != 403 {
		t.Fatalf("unbudgeted domain code = %d, want 403", rec.Code)
	}
	if len(del.calls) != 0 {
		t.Errorf("deny-by-default must not delegate, got %d calls", len(del.calls))
	}
	if got := reservationCount(t, reg, "rejected"); got != 1 {
		t.Errorf("rejected reservations = %v, want 1", got)
	}
}
