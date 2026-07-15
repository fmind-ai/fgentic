package evaluation

import (
	"strings"
	"testing"
)

const goldenTestAgents = `apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: platform-helper
spec:
  declarative:
    systemMessage: '{{include "zoo/common"}} platform'
---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: docs-qa
spec:
  declarative:
    systemMessage: '{{include "zoo/common"}} docs'
---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: scribe
spec:
  declarative:
    systemMessage: '{{include "zoo/common"}} scribe'
`

const goldenTestPrompts = `apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-zoo-prompts
data:
  common: shared boundary
`

func TestVerifyGoldenSuite(t *testing.T) {
	suite := goldenTestSuite(t)
	results, err := VerifyGoldenSuite(
		suite,
		Scenarios(),
		strings.NewReader(goldenTestAgents),
		strings.NewReader(goldenTestPrompts),
		goldenTestAnswers(suite),
	)
	if err != nil {
		t.Fatalf("VerifyGoldenSuite: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
}

func TestVerifyGoldenSuiteRejectsAnswerRegression(t *testing.T) {
	suite := goldenTestSuite(t)
	suite.Cases[0].ExpectedAnswer = "changed answer"
	_, err := VerifyGoldenSuite(
		suite,
		Scenarios(),
		strings.NewReader(goldenTestAgents),
		strings.NewReader(goldenTestPrompts),
		goldenTestAnswers(suite),
	)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("VerifyGoldenSuite error = %v, want answer regression", err)
	}
}

func TestVerifyGoldenSuiteRejectsAgentContractDrift(t *testing.T) {
	suite := goldenTestSuite(t)
	drifted := strings.Replace(goldenTestAgents, "platform'", "platform changed'", 1)
	_, err := VerifyGoldenSuite(
		suite,
		Scenarios(),
		strings.NewReader(drifted),
		strings.NewReader(goldenTestPrompts),
		goldenTestAnswers(suite),
	)
	if err == nil || !strings.Contains(err.Error(), "contract sha256") {
		t.Fatalf("VerifyGoldenSuite error = %v, want contract drift", err)
	}
}

func goldenTestAnswers(suite GoldenSuite) GoldenAnswers {
	answers := make([]GoldenAnswer, 0, len(suite.Cases))
	for _, golden := range suite.Cases {
		answers = append(answers, GoldenAnswer{ScenarioID: golden.ScenarioID, Answer: "deterministic answer"})
	}
	return GoldenAnswers{Answers: answers}
}

func goldenTestSuite(t *testing.T) GoldenSuite {
	t.Helper()
	agents, err := decodeDocuments(strings.NewReader(goldenTestAgents), "Agent")
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := namedConfigMapData(strings.NewReader(goldenTestPrompts), "agent-zoo-prompts")
	if err != nil {
		t.Fatal(err)
	}
	caseIDs := map[Agent]string{
		AgentPlatformHelper: "platform-helper-01-secret-boundary",
		AgentDocsQA:         "docs-qa-03-room-encryption",
		AgentScribe:         "scribe-04-missing-thread",
	}
	scenarios := Scenarios()
	cases := make([]GoldenCase, 0, len(caseIDs))
	for _, agent := range []Agent{AgentPlatformHelper, AgentDocsQA, AgentScribe} {
		digest, digestErr := contractDigest(agents[string(agent)], prompts)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		for _, scenario := range scenarios {
			if scenario.ID == caseIDs[agent] {
				cases = append(cases, GoldenCase{
					ScenarioID: scenario.ID, Agent: agent, Prompt: scenario.Prompt,
					ExpectedAnswer: "deterministic answer", AgentContractSHA256: digest,
				})
				break
			}
		}
	}
	return GoldenSuite{SchemaVersion: GoldenSchemaVersion, Cases: cases}
}
