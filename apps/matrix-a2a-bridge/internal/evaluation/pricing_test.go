package evaluation

import (
	"strings"
	"testing"
)

func TestPricingCatalogEstimate(t *testing.T) {
	catalog, err := DecodePricingCatalog(strings.NewReader(`{
  "schema_version": "fgentic.eval.pricing.v1",
  "version": "finance-reviewed-2026-07",
  "effective_date": "2026-07-01",
  "currency": "EUR",
  "rates": [{
    "system": "openai",
    "model": "model-a-1",
    "per_million_tokens": {"input": 2.0, "output": 8.0}
  }]
}`))
	if err != nil {
		t.Fatalf("DecodePricingCatalog: %v", err)
	}
	estimate, err := catalog.Estimate(UsageDelta{
		Identity: ProviderIdentity{System: "openai", RequestModel: "model-a", ResponseModel: "model-a-1"},
		TokenTypes: map[string]TokenDelta{
			"input":  {Tokens: 1_000_000},
			"output": {Tokens: 500_000},
		},
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if estimate.Amount != 6 || estimate.Currency != "EUR" {
		t.Fatalf("estimate = %#v", estimate)
	}
}

func TestPricingCatalogRejectsMutableOrIncompleteInput(t *testing.T) {
	tests := []string{
		`{"schema_version":"wrong","version":"v1","effective_date":"2026-07-01","currency":"EUR","rates":[]}`,
		`{"schema_version":"fgentic.eval.pricing.v1","version":"v1","effective_date":"today","currency":"EUR","rates":[]}`,
		`{"schema_version":"fgentic.eval.pricing.v1","version":"v1","effective_date":"2026-07-01","currency":"eur","rates":[]}`,
		`{"schema_version":"fgentic.eval.pricing.v1","version":"v1","effective_date":"2026-07-01","currency":"EUR","rates":[{"system":"openai","model":"m","per_million_tokens":{"input":1}}]}`,
	}
	for _, input := range tests {
		if _, err := DecodePricingCatalog(strings.NewReader(input)); err == nil {
			t.Fatalf("DecodePricingCatalog unexpectedly accepted %s", input)
		}
	}
}

func TestPricingCatalogRejectsUnpricedObservedTokenType(t *testing.T) {
	catalog := &PricingCatalog{
		SchemaVersion: PricingSchemaVersion,
		Version:       "v1",
		EffectiveDate: "2026-07-01",
		Currency:      "USD",
		Rates: []PricingRate{{
			System: "openai", Model: "m",
			PerMillionTokens: map[string]float64{"input": 1, "output": 2},
		}},
	}
	_, err := catalog.Estimate(UsageDelta{
		Identity: ProviderIdentity{System: "openai", RequestModel: "m"},
		TokenTypes: map[string]TokenDelta{
			"input":            {Tokens: 1},
			"output":           {Tokens: 1},
			"input_cache_read": {Tokens: 1},
		},
	})
	if err == nil {
		t.Fatal("Estimate unexpectedly accepted missing cache-read rate")
	}
}
