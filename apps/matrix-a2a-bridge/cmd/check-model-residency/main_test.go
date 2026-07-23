package main

import (
	"os"
	"strings"
	"testing"
)

// repoRoot is the path from this package directory (apps/matrix-a2a-bridge/cmd/check-model-residency)
// to the repository root, so the tests evaluate the actually-shipped policy file.
const repoRoot = "../../../.."

func TestShippedPolicyPassesFixtureMatrix(t *testing.T) {
	expression, err := loadAuthorizationExpression(repoRoot + "/" + policyRelativePath)
	if err != nil {
		t.Fatalf("loadAuthorizationExpression: %v", err)
	}
	if err := assertMissingHeaderErrors(expression); err != nil {
		t.Fatalf("assertMissingHeaderErrors: %v", err)
	}
	if err := evaluateMatrix(expression); err != nil {
		t.Fatalf("evaluateMatrix on the shipped policy: %v", err)
	}
}

// TestInvertedComparisonFailsMatrix is the in-code twin of the manual invert-then-restore proof: a
// fail-open policy that admits classified content to a hyperscaler MUST make the gate fail. If this
// test ever passes on an inverted expression, the gate is not guarding the real CEL.
func TestInvertedComparisonFailsMatrix(t *testing.T) {
	expression, err := loadAuthorizationExpression(repoRoot + "/" + policyRelativePath)
	if err != nil {
		t.Fatalf("loadAuthorizationExpression: %v", err)
	}
	if !strings.Contains(expression, "<=") {
		t.Fatalf("expected a <= residency comparison in the shipped expression")
	}
	inverted := strings.Replace(expression, "<=", ">=", 1)
	if err := evaluateMatrix(inverted); err == nil {
		t.Fatal("evaluateMatrix accepted a fail-open (inverted) residency comparison")
	}
}

// TestFoldedPolicyIsSingleExpression guards FIX 2: the workload and residency rules must be one
// &&-joined Require expression, not multiple list elements with ambiguous combining semantics.
func TestFoldedPolicyIsSingleExpression(t *testing.T) {
	expression, err := loadAuthorizationExpression(repoRoot + "/" + policyRelativePath)
	if err != nil {
		t.Fatalf("loadAuthorizationExpression: %v", err)
	}
	if !strings.Contains(expression, "apiKey.workload") ||
		!strings.Contains(expression, `request.headers["x-fgentic-data-classification"]`) {
		t.Fatal("the single expression must fold both the workload and residency rules")
	}
}

func TestLoadRejectsMultipleExpressions(t *testing.T) {
	// A two-element policy must be rejected: it reintroduces the AND-vs-OR ambiguity FIX 2 removed.
	dir := t.TempDir()
	path := dir + "/policy.yaml"
	body := `apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayPolicy
spec:
  traffic:
    authorization:
      action: Require
      policy:
        matchExpressions:
          - 'apiKey.workload == "matrix-a2a-bridge"'
          - 'true'
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadAuthorizationExpression(path); err == nil {
		t.Fatal("loadAuthorizationExpression accepted a two-element policy")
	}
}
