package a2aclient

import (
	"encoding/json"
	"math"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// SkillQuoteExtensionURI identifies Fgentic's data-only A2A extension carrying per-skill price
// quotes inside the already-signed AgentCard. It is an fgentic-specific extension, NOT a standard
// A2A field: because it rides the signed card, the quote is tamper-evident for free, letting the
// bridge refuse a delegation whose advertised price exceeds the configured budget for that remote
// (docs/federation.md). Prices are bilateral credit-units, never currency.
const SkillQuoteExtensionURI = "https://fgentic.fmind.ai/a2a/extensions/skill-quote/v1"

// QuoteVerdict is the outcome of comparing a remote's verified per-skill quote against a mapping's
// configured maxCost budget.
type QuoteVerdict int

const (
	// QuoteNotApplicable means no cost gate applies: maxCost is unset or the target is local.
	QuoteNotApplicable QuoteVerdict = iota
	// QuoteWithinBudget means every advertised quote price is within maxCost.
	QuoteWithinBudget
	// QuoteOverBudget means at least one advertised quote price exceeds maxCost — fail closed.
	QuoteOverBudget
	// QuoteMissing means maxCost is set but the verified card advertises no usable quote — fail closed.
	QuoteMissing
)

// QuoteAdmission reports whether target's verified card advertises per-skill quotes within maxCost.
// maxCost of zero (unset) or a local target yields QuoteNotApplicable. A ready remote whose card is
// unreadable or advertises no usable quote yields QuoteMissing so the caller fails closed. The
// worst-case (highest) advertised price is compared, since the bridge cannot pin which skill a free
// -form delegation will exercise.
func (c *Client) QuoteAdmission(target Target, maxCost uint64) QuoteVerdict {
	if maxCost == 0 || !target.IsRemote() {
		return QuoteNotApplicable
	}
	c.mu.RLock()
	cached := c.cache[target.ID()]
	c.mu.RUnlock()
	if !cached.ready || cached.card == nil {
		return QuoteMissing // no verified card to price against: fail closed
	}
	price, present := MaxSkillQuotePrice(cached.card)
	if !present {
		return QuoteMissing
	}
	if price > maxCost {
		return QuoteOverBudget
	}
	return QuoteWithinBudget
}

// MaxSkillQuotePrice returns the highest per-skill quote price advertised by the card's skill-quote
// extension and whether any usable quote was present. Absent or malformed quote data yields
// (0, false) so a caller that requires a quote fails closed rather than defaulting to free.
func MaxSkillQuotePrice(card *a2a.AgentCard) (uint64, bool) {
	if card == nil {
		return 0, false
	}
	// Inspect every skill-quote block, not just the first: a card may carry the URI more than once
	// (the trust layer does not dedupe extensions), and a caller must not be under-priced by a cheap
	// leading block hiding an expensive later one. Any malformed block fails the whole quote closed.
	var maxPrice uint64
	present := false
	for _, extension := range card.Capabilities.Extensions {
		if extension.URI != SkillQuoteExtensionURI {
			continue
		}
		quotes, ok := extension.Params["quotes"].([]any)
		if !ok || len(quotes) == 0 {
			return 0, false
		}
		for _, raw := range quotes {
			quote, ok := raw.(map[string]any)
			if !ok {
				return 0, false // a malformed entry makes the whole quote untrustworthy
			}
			price, ok := quotePrice(quote["price"])
			if !ok {
				return 0, false
			}
			present = true
			if price > maxPrice {
				maxPrice = price
			}
		}
	}
	return maxPrice, present
}

// quotePrice reads a non-negative integer credit-unit price from opaque JSON, accepting the float64
// or json.Number shapes an AgentCard's extension params may decode to.
func quotePrice(raw any) (uint64, bool) {
	switch value := raw.(type) {
	case float64:
		if value < 0 || value != math.Trunc(value) || value > maxExactJSONInteger {
			return 0, false
		}
		return uint64(value), true
	case json.Number:
		integer, err := value.Int64()
		if err != nil || integer < 0 {
			return 0, false
		}
		return uint64(integer), true
	default:
		return 0, false
	}
}
