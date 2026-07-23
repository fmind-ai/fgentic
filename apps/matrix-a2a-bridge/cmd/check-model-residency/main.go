// Command check-model-residency is the authoritative offline gate for #339. It extracts the EXACT
// CEL authorization expression shipped in infra/agentgateway/a2a-authorization.yaml, compiles it
// with cel-go, and evaluates it against a fixture request matrix with the residency ceiling
// substituted as Flux would. Because it evaluates the real expression (not a Go twin or substring
// match), a fail-open regression — e.g. inverting the residency comparison so classified content is
// admitted to a hyperscaler model — makes the gate FAIL. modelcatalog.Admits is retained only as a
// cross-check that the shipped CEL and the governed decision model agree.
//
// cel-go approximates agentgateway's Rust CEL for these simple expressions (int compare, map index,
// ternary, &&, string startsWith/endsWith). On a MISSING header key both engines error on the lookup
// (verified against the real agentgateway v1.3.1 via test:model-residency --runtime — it returns 403,
// not the "ranks as regulated" admit an earlier model assumed), and Require denies on a CEL error, so
// an absent header is fail-closed DENIED under every ceiling. Only a PRESENT-but-unknown value falls
// through the ladder to the regulated rank. The fully-populated ladder fixtures (including an unknown
// class value and both missing-header ceilings) carry the inversion-detection guarantee.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

const policyRelativePath = "infra/agentgateway/a2a-authorization.yaml"

// ceilingPlaceholder is the Flux post-build substitution token the gateway policy carries for the
// per-cluster residency ceiling. The harness substitutes it exactly as a reconcile would.
const ceilingPlaceholder = "${model_allowed_classification}"

const bridgeWorkload = "matrix-a2a-bridge"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("model residency CEL gate failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("check-model-residency", flag.ContinueOnError)
	repoRoot := flags.String("repo-root", "", "repository root (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *repoRoot == "" {
		return fmt.Errorf("--repo-root is required")
	}

	expression, err := loadAuthorizationExpression(filepath.Join(*repoRoot, policyRelativePath))
	if err != nil {
		return err
	}
	// The single Require expression must fold the workload rule and the residency rule together, so
	// the CEL evaluation below covers the whole admission decision — not the residency rule alone.
	if !strings.Contains(expression, `apiKey.workload`) {
		return fmt.Errorf("authorization expression is missing the workload rule")
	}
	if !strings.Contains(expression, `request.headers["x-fgentic-data-classification"]`) {
		return fmt.Errorf("authorization expression is missing the residency rule")
	}

	if err := assertMissingHeaderErrors(expression); err != nil {
		return err
	}
	if err := evaluateMatrix(expression); err != nil {
		return err
	}
	fmt.Printf("model residency CEL gate passed: %d fixtures against the shipped expression\n", len(matrix()))
	return nil
}

// loadAuthorizationExpression returns the single shipped CEL Require expression from the policy.
func loadAuthorizationExpression(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read authorization policy: %w", err)
	}
	// The file is a single AgentgatewayPolicy document.
	var policy struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Traffic struct {
				Authorization struct {
					Action string `yaml:"action"`
					Policy struct {
						MatchExpressions []string `yaml:"matchExpressions"`
					} `yaml:"policy"`
				} `yaml:"authorization"`
			} `yaml:"traffic"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return "", fmt.Errorf("decode authorization policy: %w", err)
	}
	if policy.Spec.Traffic.Authorization.Action != "Require" {
		return "", fmt.Errorf("authorization action = %q, want fail-closed Require", policy.Spec.Traffic.Authorization.Action)
	}
	exprs := policy.Spec.Traffic.Authorization.Policy.MatchExpressions
	// One &&-joined expression, matching repo precedent; this removes any AND-vs-OR ambiguity in how
	// agentgateway combines multiple list elements and makes this CEL evaluation cover the full policy.
	if len(exprs) != 1 {
		return "", fmt.Errorf("expected exactly one matchExpression (workload && residency), got %d", len(exprs))
	}
	return exprs[0], nil
}

// compile substitutes the ceiling and compiles the shipped expression with cel-go.
func compile(expression, ceiling string) (cel.Program, error) {
	env, err := cel.NewEnv(
		cel.Variable("apiKey", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("build CEL env: %w", err)
	}
	substituted := strings.ReplaceAll(expression, ceilingPlaceholder, ceiling)
	ast, issues := env.Compile(substituted)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile CEL: %w", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("build CEL program: %w", err)
	}
	return program, nil
}

// evalDecision evaluates the shipped expression for one request. Any evaluation error is mapped to
// DENY — agentgateway's Require semantics also deny on a CEL error — so the gate stays fail-closed.
func evalDecision(program cel.Program, activation map[string]any) (allow bool, evalErr error) {
	out, _, err := program.Eval(activation)
	if err != nil {
		return false, err
	}
	value, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL result is not a bool: %v", out.Value())
	}
	return value, nil
}

// requestActivation builds the CEL activation. A nil classHeader models a missing header key.
func requestActivation(workload, path, method string, classHeader *string) map[string]any {
	headers := map[string]any{}
	if classHeader != nil {
		headers["x-fgentic-data-classification"] = *classHeader
	}
	return map[string]any{
		"apiKey": map[string]any{"workload": workload},
		"request": map[string]any{
			"path":    path,
			"method":  method,
			"headers": headers,
		},
	}
}

const kagentPath = "/api/a2a/kagent/docs-qa"

type fixture struct {
	name     string
	workload string
	path     string
	method   string
	class    *string // nil => header absent
	ceiling  string
	want     bool // true => ALLOW
	// crossCheck compares the CEL decision to modelcatalog.Admits for pure residency fixtures
	// (valid workload, POST, present header, known ceiling).
	crossCheck bool
}

func matrix() []fixture {
	pub, anp, res, reg := "public", "approved_non_public", "restricted", "regulated"
	garbage, unset := "top-secret", "__unsubstituted__"
	return []fixture{
		// Residency ladder under a public (hyperscaler) ceiling: only public is served.
		{"public under public ceiling", bridgeWorkload, kagentPath, "POST", &pub, pub, true, true},
		{"approved_non_public denied under public", bridgeWorkload, kagentPath, "POST", &anp, pub, false, true},
		{"restricted denied under public", bridgeWorkload, kagentPath, "POST", &res, pub, false, true},
		{"regulated denied under public (inversion catcher)", bridgeWorkload, kagentPath, "POST", &reg, pub, false, true},
		{"unknown class denied under public (inversion catcher)", bridgeWorkload, kagentPath, "POST", &garbage, pub, false, false},
		// Residency ladder under a regulated (sovereign) ceiling: every class up to regulated served.
		{"regulated served under regulated ceiling", bridgeWorkload, kagentPath, "POST", &reg, reg, true, true},
		{"public served under regulated ceiling", bridgeWorkload, kagentPath, "POST", &pub, reg, true, true},
		{"restricted served under regulated ceiling", bridgeWorkload, kagentPath, "POST", &res, reg, true, true},
		// Intermediate ceiling.
		{"regulated denied under restricted ceiling", bridgeWorkload, kagentPath, "POST", &reg, res, false, true},
		{"restricted served under restricted ceiling", bridgeWorkload, kagentPath, "POST", &res, res, true, true},
		// Fail-closed ceiling: an unknown/unsubstituted ceiling collapses to public.
		{"regulated denied under unknown ceiling", bridgeWorkload, kagentPath, "POST", &reg, unset, false, false},
		{"public served under unknown ceiling", bridgeWorkload, kagentPath, "POST", &pub, unset, true, false},
		// Absent header key: the shipped CEL errors on the missing lookup (verified against the real
		// agentgateway v1.3.1 via test:model-residency --runtime), which Require denies under EVERY
		// ceiling — strictly more fail-closed than a "ranks as regulated" model. A header-less request
		// has bypassed the bridge's classification, so denying it is correct (D11). (nil class => absent.)
		{"missing header denied under regulated ceiling", bridgeWorkload, kagentPath, "POST", nil, reg, false, false},
		{"missing header denied under public ceiling", bridgeWorkload, kagentPath, "POST", nil, pub, false, false},
		// Combined-policy gating proves the fold covers the workload rule too.
		{"wrong workload denied", "intruder", kagentPath, "POST", &pub, pub, false, false},
		{"wrong namespace path denied", bridgeWorkload, "/api/a2a/other/docs-qa", "POST", &pub, pub, false, false},
		{"unsupported method denied", bridgeWorkload, kagentPath, "DELETE", &pub, pub, false, false},
		{"agent-card GET allowed", bridgeWorkload, kagentPath + "/.well-known/agent-card.json", "GET", &pub, pub, true, false},
		{"non-card GET denied", bridgeWorkload, kagentPath, "GET", &pub, pub, false, false},
	}
}

// evaluateMatrix compiles + evaluates the shipped expression for every fixture and cross-checks the
// governed decision model. It returns an error on the first mismatch, so the gate fails loudly.
func evaluateMatrix(expression string) error {
	for _, f := range matrix() {
		program, err := compile(expression, f.ceiling)
		if err != nil {
			return fmt.Errorf("fixture %q: %w", f.name, err)
		}
		allow, evalErr := evalDecision(program, requestActivation(f.workload, f.path, f.method, f.class))
		if evalErr != nil {
			// Fail-closed: a CEL error denies. Only acceptable when the fixture expects DENY.
			allow = false
		}
		if allow != f.want {
			return fmt.Errorf("fixture %q: CEL decision allow=%v, want allow=%v", f.name, allow, f.want)
		}
		if f.crossCheck {
			ceiling, cerr := modelcatalog.ParseClassification(f.ceiling)
			if cerr != nil {
				return fmt.Errorf("fixture %q: cross-check ceiling: %w", f.name, cerr)
			}
			room := modelcatalog.ClassificationOrMostRestrictive(deref(f.class))
			model := modelcatalog.Model{AllowedClassification: ceiling}
			if model.Admits(room) != f.want {
				return fmt.Errorf("fixture %q: modelcatalog.Admits disagrees with the shipped CEL", f.name)
			}
		}
		fmt.Printf("  %-52s allow=%-5v OK\n", f.name, allow)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// assertMissingHeaderErrors makes the one cel-go/agentgateway divergence explicit: cel-go raises a
// no-such-key error for a missing header, whereas agentgateway resolves it to null. Both are
// fail-closed (deny) under a public ceiling; documenting it here makes a semantic drift visible.
func assertMissingHeaderErrors(expression string) error {
	program, err := compile(expression, "public")
	if err != nil {
		return err
	}
	_, evalErr := evalDecision(program, requestActivation(bridgeWorkload, kagentPath, "POST", nil))
	if evalErr == nil {
		return errors.New("expected cel-go to error on a missing header key (divergence assertion)")
	}
	// A missing header is fail-closed in both engines under a public ceiling: cel-go errors (-> deny
	// here), agentgateway returns null -> ranks regulated -> denied under public.
	fmt.Printf("  %-52s deny (both engines error on the absent key) OK\n", "missing header under public ceiling")
	return nil
}
