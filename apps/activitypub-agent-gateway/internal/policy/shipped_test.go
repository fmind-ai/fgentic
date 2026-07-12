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
	if _, err := Parse(raw); err != nil {
		t.Fatalf("shipped policy.json must parse: %v", err)
	}
}
