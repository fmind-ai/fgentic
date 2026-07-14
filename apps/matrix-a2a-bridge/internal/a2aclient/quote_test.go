package a2aclient

import (
	"encoding/json"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func quoteCard(quotes ...map[string]any) *a2a.AgentCard {
	list := make([]any, len(quotes))
	for i, quote := range quotes {
		list[i] = quote
	}
	return &a2a.AgentCard{
		Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{
			URI:    SkillQuoteExtensionURI,
			Params: map[string]any{"quotes": list},
		}}},
	}
}

func TestMaxSkillQuotePrice(t *testing.T) {
	malformedList := &a2a.AgentCard{Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{
		URI: SkillQuoteExtensionURI, Params: map[string]any{"quotes": []any{"not-a-map"}},
	}}}}
	tests := []struct {
		name    string
		card    *a2a.AgentCard
		price   uint64
		present bool
	}{
		{name: "nil card", card: nil},
		{name: "no extension", card: &a2a.AgentCard{}},
		{name: "single float price", card: quoteCard(map[string]any{"skillId": "a", "price": float64(12)}), price: 12, present: true},
		{name: "json.Number price", card: quoteCard(map[string]any{"price": json.Number("7")}), price: 7, present: true},
		{name: "max across skills", card: quoteCard(map[string]any{"price": float64(3)}, map[string]any{"price": float64(9)}), price: 9, present: true},
		{name: "empty quotes fails closed", card: quoteCard()},
		{name: "malformed entry fails closed", card: malformedList},
		{name: "negative price fails closed", card: quoteCard(map[string]any{"price": float64(-1)})},
		{name: "non-integer price fails closed", card: quoteCard(map[string]any{"price": float64(1.5)})},
		{name: "missing price fails closed", card: quoteCard(map[string]any{"skillId": "a"})},
	}
	// A card may carry the skill-quote URI more than once; price the global max, not just the first.
	twoBlocks := &a2a.AgentCard{Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{
		{URI: SkillQuoteExtensionURI, Params: map[string]any{"quotes": []any{map[string]any{"price": float64(3)}}}},
		{URI: SkillQuoteExtensionURI, Params: map[string]any{"quotes": []any{map[string]any{"price": float64(99)}}}},
	}}}
	tests = append(
		tests,
		struct {
			name    string
			card    *a2a.AgentCard
			price   uint64
			present bool
		}{name: "global max across duplicate blocks", card: twoBlocks, price: 99, present: true},
	)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			price, present := MaxSkillQuotePrice(tt.card)
			if price != tt.price || present != tt.present {
				t.Fatalf("MaxSkillQuotePrice() = (%d, %v), want (%d, %v)", price, present, tt.price, tt.present)
			}
		})
	}
}

func TestQuoteAdmissionAgainstVerifiedCard(t *testing.T) {
	fixture, client, _ := newRemoteContractFixture(t, nil, "")
	card := cloneCardForTest(t, fixture.baseCard)
	card.Capabilities.Extensions = append(card.Capabilities.Extensions, a2a.AgentExtension{
		URI: SkillQuoteExtensionURI,
		Params: map[string]any{"quotes": []any{
			map[string]any{"skillId": "answer", "unit": "token", "price": float64(50), "maxTokens": float64(4096)},
		}},
	})
	fixture.setCard(signValidAgentCard(t, card, fixture.key, fixture.identity.KeyID), `"card-quote"`)
	target, err := NewRemoteTarget(fixture.server.URL+remoteFixturePath, fixture.identity, 4096, nil)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("ResolveAgentCard: %v", err)
	}

	local, err := NewLocalTarget("/api/a2a/kagent/agent")
	if err != nil {
		t.Fatalf("NewLocalTarget: %v", err)
	}
	cases := []struct {
		name    string
		target  Target
		maxCost uint64
		want    QuoteVerdict
	}{
		{name: "no cost gate", target: target, maxCost: 0, want: QuoteNotApplicable},
		{name: "local target", target: local, maxCost: 100, want: QuoteNotApplicable},
		{name: "within budget", target: target, maxCost: 100, want: QuoteWithinBudget},
		{name: "at budget", target: target, maxCost: 50, want: QuoteWithinBudget},
		{name: "over budget", target: target, maxCost: 40, want: QuoteOverBudget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := client.QuoteAdmission(tc.target, tc.maxCost); got != tc.want {
				t.Fatalf("QuoteAdmission() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestQuoteAdmissionMissingQuoteFailsClosed(t *testing.T) {
	// The base fixture card advertises no skill quote, so a configured cost gate must fail closed.
	_, client, target := newRemoteContractFixture(t, nil, "")
	if got := client.QuoteAdmission(target, 100); got != QuoteMissing {
		t.Fatalf("QuoteAdmission before verification = %d, want QuoteMissing (fail closed)", got)
	}
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("ResolveAgentCard: %v", err)
	}
	if got := client.QuoteAdmission(target, 100); got != QuoteMissing {
		t.Fatalf("QuoteAdmission for quote-less card = %d, want QuoteMissing", got)
	}
}
