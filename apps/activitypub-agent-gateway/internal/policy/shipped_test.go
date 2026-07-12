package policy

import (
	"os"
	"testing"
)

// TestShippedPolicyIsValid guards the mutable policy.json the Component projects: it must always
// parse (fail-closed defaults are fine — an empty allowlist denies all).
func TestShippedPolicyIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../component/policy.json")
	if err != nil {
		t.Fatalf("read shipped policy: %v", err)
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("shipped policy.json must parse: %v", err)
	}
	// The shipped default is deny-by-default for budgets too: no default pool, so any actor an
	// operator later allowlists still has no budget until one is explicitly configured.
	if _, ok := p.Budget("https://mastodon.example/users/bob"); ok {
		t.Errorf("shipped policy must resolve to no budget by default")
	}
}
