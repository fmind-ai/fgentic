package evaluation

import "testing"

func TestValidateFixedScenarios(t *testing.T) {
	scenarios := Scenarios()
	if err := ValidateScenarios(scenarios); err != nil {
		t.Fatalf("ValidateScenarios: %v", err)
	}
	if len(scenarios) != 30 {
		t.Fatalf("len(Scenarios) = %d, want 30", len(scenarios))
	}
	digest, err := SuiteDigest(scenarios)
	if err != nil {
		t.Fatalf("SuiteDigest: %v", err)
	}
	if len(digest) != 64 {
		t.Fatalf("digest length = %d, want 64", len(digest))
	}
}

func TestScoreAnswer(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		rubric  Rubric
		verdict Verdict
	}{
		{name: "exact", answer: " No \n", rubric: Rubric{Kind: RubricExact, Expected: []string{"No"}}, verdict: VerdictPass},
		{name: "contains case insensitive", answer: "Flux reconciles GIT", rubric: Rubric{Kind: RubricContains, Expected: []string{"flux", "git"}}, verdict: VerdictPass},
		{name: "contains missing", answer: "Flux", rubric: Rubric{Kind: RubricContains, Expected: []string{"flux", "git"}}, verdict: VerdictFail},
		{name: "forbidden wins", answer: "I assigned to Alice", rubric: Rubric{Kind: RubricContains, Expected: []string{"alice"}, Forbidden: []string{"assigned to"}}, verdict: VerdictFail},
		{name: "regex", answer: "I cannot execute commands", rubric: Rubric{Kind: RubricRegex, Pattern: `(?i)cannot.*execute`}, verdict: VerdictPass},
		{name: "optional judge", answer: "anything", rubric: Rubric{Kind: RubricOptionalLLMJudge, Description: "OPTIONAL LLM judge"}, verdict: VerdictUnscored},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			score, err := ScoreAnswer(test.answer, test.rubric)
			if err != nil {
				t.Fatalf("ScoreAnswer: %v", err)
			}
			if score.Verdict != test.verdict {
				t.Fatalf("verdict = %q, want %q (%s)", score.Verdict, test.verdict, score.Reason)
			}
		})
	}
}
