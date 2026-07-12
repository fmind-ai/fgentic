package apgateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAgentCardIsServedForAllowlistedAgent(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	g.SetA2APublicBase("https://a2a.fgentic.localhost")

	rec := do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa/agent-card.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var card map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	if card["url"] != "https://a2a.fgentic.localhost/api/a2a/kagent/docs-qa" {
		t.Errorf("card url = %v", card["url"])
	}
	if card["name"] != "agent-docs-qa" {
		t.Errorf("card name = %v", card["name"])
	}
	if card["protocolVersion"] == "" || card["protocolVersion"] == nil {
		t.Errorf("card must advertise a protocolVersion")
	}
	if card["preferredTransport"] != "JSONRPC" {
		t.Errorf("preferredTransport = %v", card["preferredTransport"])
	}
}

func TestAgentCardHiddenForUnlistedAgent(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	if rec := do(t, g, http.MethodGet, "/ap/agents/agent-nope/agent-card.json", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unlisted agent card must 404, got %d", rec.Code)
	}
}

func TestActorAdvertisesA2AImplements(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	g.SetA2APublicBase("https://a2a.fgentic.localhost")

	rec := do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa", "")
	var actor map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &actor); err != nil {
		t.Fatalf("unmarshal actor: %v", err)
	}
	impls, ok := actor["implements"].([]any)
	if !ok || len(impls) != 1 {
		t.Fatalf("implements = %v", actor["implements"])
	}
	impl := impls[0].(map[string]any)
	if impl["name"] != "A2A" {
		t.Errorf("implements name = %v", impl["name"])
	}
	if impl["href"] != "https://a2a.fgentic.localhost/api/a2a/kagent/docs-qa" {
		t.Errorf("implements href = %v (must list the A2A endpoint)", impl["href"])
	}
	if impl["agentCard"] != "https://fgentic.localhost/ap/agents/agent-docs-qa/agent-card.json" {
		t.Errorf("implements agentCard = %v", impl["agentCard"])
	}
}

// TestDiscoveryChainResolves proves one WebFinger lookup yields resolvable pointers to BOTH the AP
// actor and the A2A card — the end-to-end novel cross-protocol discovery of issue #215.
func TestDiscoveryChainResolves(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})

	wf := do(t, g, http.MethodGet, "/.well-known/webfinger?resource=acct:agent-docs-qa@fgentic.localhost", "")
	var doc jrd
	if err := json.Unmarshal(wf.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal jrd: %v", err)
	}
	var actorHref, cardHref string
	for _, l := range doc.Links {
		switch l.Rel {
		case "self":
			actorHref = l.Href
		case a2aAgentCardRel:
			cardHref = l.Href
		}
	}
	if actorHref == "" || cardHref == "" {
		t.Fatalf("WebFinger must yield both an actor and an A2A card link: %+v", doc.Links)
	}
	// Both hrefs must resolve on this gateway.
	for _, href := range []string{actorHref, cardHref} {
		path := strings.TrimPrefix(href, "https://fgentic.localhost")
		if rec := do(t, g, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("discovery link %q did not resolve (code %d)", href, rec.Code)
		}
	}
}
