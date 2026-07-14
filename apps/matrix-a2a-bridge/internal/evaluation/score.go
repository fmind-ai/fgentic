package evaluation

import (
	"fmt"
	"regexp"
	"strings"
)

// Verdict is a deterministic pass/fail or an explicit unscored outcome.
type Verdict string

const (
	// VerdictPass means every deterministic assertion passed.
	VerdictPass Verdict = "pass"
	// VerdictFail means at least one deterministic assertion failed.
	VerdictFail Verdict = "fail"
	// VerdictUnscored marks optional qualitative review that was not invoked.
	VerdictUnscored Verdict = "unscored"
)

// Score records a verdict, optional numeric point, and auditable reason.
type Score struct {
	Verdict Verdict  `json:"verdict"`
	Points  *float64 `json:"points,omitempty"`
	Reason  string   `json:"reason"`
}

// ScoreAnswer applies only the declared local rubric and never calls a model.
func ScoreAnswer(answer string, rubric Rubric) (Score, error) {
	pass := 1.0
	fail := 0.0
	normalized := strings.TrimSpace(answer)
	lower := strings.ToLower(normalized)

	for _, forbidden := range rubric.Forbidden {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			return Score{Verdict: VerdictFail, Points: &fail, Reason: fmt.Sprintf("contains forbidden text %q", forbidden)}, nil
		}
	}

	switch rubric.Kind {
	case RubricExact:
		if normalized == rubric.Expected[0] {
			return Score{Verdict: VerdictPass, Points: &pass, Reason: "exact match"}, nil
		}
		return Score{Verdict: VerdictFail, Points: &fail, Reason: fmt.Sprintf("want exact %q", rubric.Expected[0])}, nil
	case RubricContains:
		for _, expected := range rubric.Expected {
			if !strings.Contains(lower, strings.ToLower(expected)) {
				return Score{Verdict: VerdictFail, Points: &fail, Reason: fmt.Sprintf("missing required text %q", expected)}, nil
			}
		}
		return Score{Verdict: VerdictPass, Points: &pass, Reason: "contains every required value"}, nil
	case RubricRegex:
		pattern, err := regexp.Compile(rubric.Pattern)
		if err != nil {
			return Score{}, fmt.Errorf("compile rubric regex: %w", err)
		}
		if pattern.MatchString(normalized) {
			return Score{Verdict: VerdictPass, Points: &pass, Reason: "regular expression matched"}, nil
		}
		return Score{Verdict: VerdictFail, Points: &fail, Reason: fmt.Sprintf("did not match %q", rubric.Pattern)}, nil
	case RubricOptionalLLMJudge:
		return Score{Verdict: VerdictUnscored, Reason: rubric.Description}, nil
	default:
		return Score{}, fmt.Errorf("unknown rubric kind %q", rubric.Kind)
	}
}
