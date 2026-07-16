package evaluation

import (
	"strings"
	"testing"
)

const goldenTestAgents = `apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: helper
spec:
  declarative:
    systemMessage: '{{include "zoo/common"}} helper'
`

const goldenTestPrompts = `apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-zoo-prompts
data:
  common: shared boundary
`

func TestVerifyAgentGoldenSuites(t *testing.T) {
	suite := goldenTestSuite(t)
	results, err := VerifyAgentGoldenSuites(
		[]AgentGoldenSuite{suite},
		strings.NewReader(goldenTestAgents),
		strings.NewReader(goldenTestPrompts),
		GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
	)
	if err != nil {
		t.Fatalf("VerifyAgentGoldenSuites: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
}

func TestVerifyAgentGoldenSuitesRejectsAnswerRegression(t *testing.T) {
	suite := goldenTestSuite(t)
	suite.Scenarios[0].Rubric.Expected = []string{"changed answer"}
	_, err := VerifyAgentGoldenSuites(
		[]AgentGoldenSuite{suite},
		strings.NewReader(goldenTestAgents),
		strings.NewReader(goldenTestPrompts),
		GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
	)
	if err == nil || !strings.Contains(err.Error(), "--- expected\n+++ actual\n-changed answer\n+deterministic answer") {
		t.Fatalf("VerifyAgentGoldenSuites error = %v, want clear answer diff", err)
	}
}

func TestVerifyAgentGoldenSuiteUsesCompleteGateAssertionsWithoutRequiringOtherFixtures(t *testing.T) {
	agents := goldenTestAgents + `---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: another
spec:
  declarative:
    systemMessage: '{{include "zoo/common"}} another'
`
	results, err := VerifyAgentGoldenSuite(
		goldenTestSuite(t),
		strings.NewReader(agents),
		strings.NewReader(goldenTestPrompts),
		GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
	)
	if err != nil {
		t.Fatalf("VerifyAgentGoldenSuite: %v", err)
	}
	if len(results) != 1 || results[0].Agent != "helper" {
		t.Fatalf("results = %#v, want one helper result", results)
	}
}

func TestVerifyAgentGoldenSuitesRejectsAgentContractDrift(t *testing.T) {
	suite := goldenTestSuite(t)
	drifted := strings.Replace(goldenTestAgents, "helper'", "helper changed'", 1)
	_, err := VerifyAgentGoldenSuites(
		[]AgentGoldenSuite{suite},
		strings.NewReader(drifted),
		strings.NewReader(goldenTestPrompts),
		GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
	)
	if err == nil || !strings.Contains(err.Error(), "contract sha256") {
		t.Fatalf("VerifyAgentGoldenSuites error = %v, want contract drift", err)
	}
}

func TestVerifyAgentGoldenSuitesRejectsMissingFixture(t *testing.T) {
	agents := goldenTestAgents + `---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: orphan
spec:
  declarative:
    systemMessage: '{{include "zoo/common"}} orphan'
`
	_, err := VerifyAgentGoldenSuites(
		[]AgentGoldenSuite{goldenTestSuite(t)},
		strings.NewReader(agents),
		strings.NewReader(goldenTestPrompts),
		GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
	)
	if err == nil || !strings.Contains(err.Error(), "fixture count") {
		t.Fatalf("VerifyAgentGoldenSuites error = %v, want fixture count mismatch", err)
	}
}

func TestVerifyAgentGoldenSuitesRejectsOptionalJudge(t *testing.T) {
	suite := goldenTestSuite(t)
	suite.Scenarios[0].Rubric = Rubric{Kind: RubricOptionalLLMJudge, Description: "review"}
	_, err := VerifyAgentGoldenSuites(
		[]AgentGoldenSuite{suite},
		strings.NewReader(goldenTestAgents),
		strings.NewReader(goldenTestPrompts),
		GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
	)
	if err == nil || !strings.Contains(err.Error(), "deterministic rubric") {
		t.Fatalf("VerifyAgentGoldenSuites error = %v, want deterministic rubric error", err)
	}
}

func TestVerifyAgentGoldenSuitesRejectsEmptyDeterministicAssertions(t *testing.T) {
	tests := []struct {
		name   string
		rubric Rubric
		want   string
	}{
		{name: "exact", rubric: Rubric{Kind: RubricExact, Expected: []string{" "}}, want: "non-blank expected"},
		{name: "contains", rubric: Rubric{Kind: RubricContains, Expected: []string{"answer", ""}}, want: "non-blank expected"},
		{name: "regex", rubric: Rubric{Kind: RubricRegex, Pattern: ""}, want: "non-blank pattern"},
		{name: "forbidden", rubric: Rubric{Kind: RubricExact, Expected: []string{"deterministic answer"}, Forbidden: []string{" "}}, want: "forbidden values"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suite := goldenTestSuite(t)
			suite.Scenarios[0].Rubric = test.rubric
			_, err := VerifyAgentGoldenSuites(
				[]AgentGoldenSuite{suite},
				strings.NewReader(goldenTestAgents),
				strings.NewReader(goldenTestPrompts),
				GoldenAnswers{Answers: []GoldenAnswer{{ScenarioID: "helper-smoke", Answer: "deterministic answer"}}},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyAgentGoldenSuites error = %v, want %q", err, test.want)
			}
		})
	}
}

func goldenTestSuite(t *testing.T) AgentGoldenSuite {
	t.Helper()
	digest, err := AgentContractDigest(
		"helper",
		strings.NewReader(goldenTestAgents),
		strings.NewReader(goldenTestPrompts),
	)
	if err != nil {
		t.Fatal(err)
	}
	return AgentGoldenSuite{
		SchemaVersion:       AgentGoldenSchemaVersion,
		Agent:               "helper",
		AgentContractSHA256: digest,
		Scenarios: []Scenario{{
			ID:     "helper-smoke",
			Agent:  "helper",
			Prompt: "confirm",
			Rubric: Rubric{Kind: RubricExact, Expected: []string{"deterministic answer"}},
		}},
	}
}
