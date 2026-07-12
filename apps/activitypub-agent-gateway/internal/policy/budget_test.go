package policy

import "testing"

const budgetPolicy = `{
  "version": 1,
  "allowed_domains": ["mastodon.example", "gts.example"],
  "budgets": {
    "reservation_tokens": 1000,
    "default_tokens_per_minute": 0,
    "domains": {"mastodon.example": 6000},
    "actors": {"https://mastodon.example/users/bob": 2000}
  }
}`

func TestBudgetResolution(t *testing.T) {
	p, err := Parse([]byte(budgetPolicy))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// An explicit actor override wins; its pool is the actor cap, the domain pool bounds the domain.
	b, ok := p.Budget("https://mastodon.example/users/bob")
	if !ok {
		t.Fatalf("bob must have a budget")
	}
	if b.Reservation != 1000 || b.ActorPool != 2000 || b.DomainPool != 6000 {
		t.Errorf("bob budget = %+v", b)
	}

	// An unlisted actor inherits the domain pool as its individual cap.
	b, ok = p.Budget("https://mastodon.example/users/alice")
	if !ok || b.ActorPool != 6000 || b.DomainPool != 6000 {
		t.Errorf("alice budget = %+v ok=%v", b, ok)
	}

	// gts.example is allowlisted but has no budget entry and no default → deny-by-default.
	if _, ok := p.Budget("https://gts.example/users/x"); ok {
		t.Errorf("an allowlisted-but-unbudgeted domain must resolve to no budget")
	}
	// A malformed actor URI resolves to no budget.
	if _, ok := p.Budget("not a url"); ok {
		t.Errorf("malformed actor must have no budget")
	}
}

func TestBudgetDefaultPool(t *testing.T) {
	p, err := Parse([]byte(`{"version":1,"budgets":{"reservation_tokens":500,"default_tokens_per_minute":1500}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, ok := p.Budget("https://any.example/users/z")
	if !ok || b.Reservation != 500 || b.ActorPool != 1500 || b.DomainPool != 1500 {
		t.Errorf("default budget = %+v ok=%v", b, ok)
	}
}

func TestNoBudgetSectionResolvesFalse(t *testing.T) {
	p, err := Parse([]byte(validPolicy))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := p.Budget("https://mastodon.example/users/bob"); ok {
		t.Errorf("a policy with no budgets section must resolve to no budget (deny when enforced)")
	}
}

func TestBudgetParseRejects(t *testing.T) {
	cases := map[string]string{
		"zero reservation":          `{"version":1,"budgets":{"reservation_tokens":0}}`,
		"default below reserve":     `{"version":1,"budgets":{"reservation_tokens":1000,"default_tokens_per_minute":500}}`,
		"domain pool below reserve": `{"version":1,"budgets":{"reservation_tokens":1000,"domains":{"m.example":500}}}`,
		"actor pool below reserve":  `{"version":1,"budgets":{"reservation_tokens":1000,"actors":{"https://m.example/u":500}}}`,
		"bad domain key":            `{"version":1,"budgets":{"reservation_tokens":100,"domains":{"https://m.example":6000}}}`,
		"bad actor key":             `{"version":1,"budgets":{"reservation_tokens":100,"actors":{"m.example":6000}}}`,
		"unknown budget field":      `{"version":1,"budgets":{"reservation_tokens":100,"bogus":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Errorf("expected parse error for %s", name)
			}
		})
	}
}

func TestBudgetDigestUnaffected(t *testing.T) {
	// Budgets are content-free config; adding them must not change the allowlist digest shape.
	p, err := Parse([]byte(budgetPolicy))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Digest() != "v1/d2/a0" {
		t.Errorf("Digest = %q", p.Digest())
	}
}
