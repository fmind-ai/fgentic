package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

// scriptedFixtureJudge returns an entailment judge that answers each claim from the fixture's declared
// ground-truth entailment, keyed by claim text. It runs no model and reaches no cluster; a claim whose
// text is not in the fixture fails the test loudly rather than silently defaulting.
func scriptedFixtureJudge(t *testing.T, fixture FaithfulnessFixture) entailmentJudge {
	t.Helper()
	entailmentByText := make(map[string]bool, len(fixture.Answer.Claims))
	for _, claim := range fixture.Answer.Claims {
		if entailed, declared := fixture.TrueEntailment[claim.ID]; declared {
			entailmentByText[claim.Text] = entailed
		}
	}
	return func(_ context.Context, claimText string, chunkTexts []string) (EntailmentResult, error) {
		if len(chunkTexts) == 0 {
			t.Fatalf("judge invoked with no cited chunk text for claim %q", claimText)
		}
		entailed, found := entailmentByText[claimText]
		if !found {
			t.Fatalf("judge invoked for a claim without declared ground truth: %q", claimText)
		}
		return EntailmentResult{Entailed: entailed, Rationale: "scripted"}, nil
	}
}

func TestFaithfulnessFixtures(t *testing.T) {
	for _, fixture := range FaithfulnessFixtures() {
		t.Run(fixture.Name, func(t *testing.T) {
			if err := fixture.Answer.Validate(); err != nil {
				t.Fatalf("fixture answer must be structurally valid: %v", err)
			}
			result, err := ScoreFaithfulness(t.Context(), scriptedFixtureJudge(t, fixture), fixture.Answer)
			if err != nil {
				t.Fatalf("ScoreFaithfulness: %v", err)
			}
			if result.Verdict != fixture.ExpectedVerdict {
				t.Fatalf("overall verdict = %q, want %q", result.Verdict, fixture.ExpectedVerdict)
			}
			if len(result.Claims) != len(fixture.ExpectedClaims) {
				t.Fatalf("scored %d claims, want %d", len(result.Claims), len(fixture.ExpectedClaims))
			}
			for _, claim := range result.Claims {
				want, ok := fixture.ExpectedClaims[claim.ClaimID]
				if !ok {
					t.Fatalf("unexpected scored claim %q", claim.ClaimID)
				}
				if claim.Verdict != want {
					t.Fatalf("claim %q verdict = %q, want %q", claim.ClaimID, claim.Verdict, want)
				}
				if !claim.Verdict.Valid() {
					t.Fatalf("claim %q verdict is not a valid typed value", claim.ClaimID)
				}
			}
			assertFaithfulnessResultContentFree(t, result, fixture.Answer)
		})
	}
}

// assertFaithfulnessResultContentFree proves the recorded result surfaces only IDs and verdicts: no
// claim prose or chunk text from the answer may appear in the JSON persisted to the report (D7).
func assertFaithfulnessResultContentFree(t *testing.T, result FaithfulnessResult, answer CitedAnswer) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal faithfulness result: %v", err)
	}
	rendered := string(encoded)
	for _, claim := range answer.Claims {
		if claim.Text != "" && strings.Contains(rendered, claim.Text) {
			t.Fatalf("faithfulness result leaked claim prose: %s", rendered)
		}
	}
	for _, chunk := range answer.Chunks {
		if chunk.Text != "" && strings.Contains(rendered, chunk.Text) {
			t.Fatalf("faithfulness result leaked chunk text: %s", rendered)
		}
	}
}

func TestScoreFaithfulnessFailsClosedOnEmptyAnswer(t *testing.T) {
	// An answer that cites nothing (no claims) must never silently pass.
	failingJudge := func(context.Context, string, []string) (EntailmentResult, error) {
		t.Fatal("an empty answer must not call the judge")
		return EntailmentResult{}, nil
	}
	result, err := ScoreFaithfulness(t.Context(), failingJudge, CitedAnswer{})
	if err != nil {
		t.Fatalf("ScoreFaithfulness: %v", err)
	}
	if result.Verdict != VerdictFail {
		t.Fatalf("verdict = %q, want fail-closed for a citation-free answer", result.Verdict)
	}
}

func TestScoreFaithfulnessTransportErrorAborts(t *testing.T) {
	// A judge egress failure must abort, never be read as a verdict.
	answer := CitedAnswer{
		Chunks: []CitedChunk{{ID: "k1", Text: "the chunk"}},
		Claims: []AnswerClaim{{ID: "c1", Text: "a claim", CitedChunks: []string{"k1"}}},
	}
	brokenJudge := func(context.Context, string, []string) (EntailmentResult, error) {
		return EntailmentResult{}, errors.New("judge unreachable")
	}
	if _, err := ScoreFaithfulness(t.Context(), brokenJudge, answer); err == nil {
		t.Fatal("a judge transport failure must abort faithfulness scoring")
	}
}

func TestScoreFaithfulnessMissingChunkSkipsJudge(t *testing.T) {
	answer := CitedAnswer{
		Chunks: []CitedChunk{{ID: "present", Text: "present text"}},
		Claims: []AnswerClaim{{ID: "c1", Text: "cites an absent chunk", CitedChunks: []string{"absent"}}},
	}
	judge := func(context.Context, string, []string) (EntailmentResult, error) {
		t.Fatal("a missing cited chunk must fail closed without a judge call")
		return EntailmentResult{}, nil
	}
	result, err := ScoreFaithfulness(t.Context(), judge, answer)
	if err != nil {
		t.Fatalf("ScoreFaithfulness: %v", err)
	}
	if result.Verdict != VerdictFail || result.Claims[0].Verdict != FaithfulnessUnsupported {
		t.Fatalf("missing cited chunk must be unsupported/fail: %+v", result)
	}
}

func TestScoreFaithfulnessBlankChunkSkipsJudge(t *testing.T) {
	// A cited chunk that is present but blank is no evidence; grounding must not depend on the judge.
	answer := CitedAnswer{
		Chunks: []CitedChunk{{ID: "blank", Text: "   \n\t"}},
		Claims: []AnswerClaim{{ID: "c1", Text: "cites a blank chunk", CitedChunks: []string{"blank"}}},
	}
	judge := func(context.Context, string, []string) (EntailmentResult, error) {
		t.Fatal("a present-but-blank cited chunk must fail closed without a judge call")
		return EntailmentResult{}, nil
	}
	result, err := ScoreFaithfulness(t.Context(), judge, answer)
	if err != nil {
		t.Fatalf("ScoreFaithfulness: %v", err)
	}
	if result.Verdict != VerdictFail || result.Claims[0].Verdict != FaithfulnessUnsupported {
		t.Fatalf("blank cited chunk must be unsupported/fail: %+v", result)
	}
}

func TestParseEntailmentResultAcceptsExactContract(t *testing.T) {
	got, err := ParseEntailmentResult(`{"entailed": true, "rationale": "the chunk states this"}`)
	if err != nil {
		t.Fatalf("ParseEntailmentResult: %v", err)
	}
	if !got.Entailed {
		t.Fatalf("entailed = %v, want true", got.Entailed)
	}
}

func TestParseEntailmentResultFailsClosed(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"not json":          "the claim is supported",
		"prose around json": `Verdict: {"entailed":true,"rationale":"x"}`,
		"trailing content":  `{"entailed":true,"rationale":"x"} extra`,
		"missing entailed":  `{"rationale": "x"}`,
		"unknown field":     `{"entailed":true,"rationale":"x","score":1}`,
		"empty rationale":   `{"entailed":true,"rationale":"  "}`,
		"wrong type":        `{"entailed":"yes","rationale":"x"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEntailmentResult(raw); err == nil {
				t.Fatalf("expected a fail-closed error for %q", raw)
			}
		})
	}
}

func TestParseEntailmentResultErrorsAreContentFree(t *testing.T) {
	const marker = "EXFIL-marker-91"
	cases := []string{
		`{"entailed":true,"rationale":"x","` + marker + `":0}`,
		`prose ` + marker + ` not json`,
	}
	for _, raw := range cases {
		_, err := ParseEntailmentResult(raw)
		if err == nil {
			t.Fatalf("expected an error for %q", raw)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("entailment parse error leaked model content: %q", err.Error())
		}
	}
}

func TestCitedAnswerValidateRejectsMalformedInput(t *testing.T) {
	cases := map[string]CitedAnswer{
		"blank chunk ID":     {Chunks: []CitedChunk{{ID: " ", Text: "t"}}},
		"duplicate chunk ID": {Chunks: []CitedChunk{{ID: "k", Text: "a"}, {ID: "k", Text: "b"}}},
		"blank claim ID":     {Claims: []AnswerClaim{{ID: "", Text: "t"}}},
		"duplicate claim ID": {Claims: []AnswerClaim{{ID: "c", Text: "a"}, {ID: "c", Text: "b"}}},
		"blank cited chunk":  {Claims: []AnswerClaim{{ID: "c", Text: "t", CitedChunks: []string{" "}}}},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			if err := answer.Validate(); err == nil {
				t.Fatalf("expected validation to reject %s", name)
			}
		})
	}
	if err := (CitedAnswer{}).Validate(); err != nil {
		t.Fatalf("an empty answer must validate (it is scored fail-closed, not rejected): %v", err)
	}
}

func TestFaithfulnessVerdictValid(t *testing.T) {
	for _, verdict := range []FaithfulnessVerdict{FaithfulnessSupported, FaithfulnessUnsupported, FaithfulnessUncited} {
		if !verdict.Valid() {
			t.Fatalf("%q must be valid", verdict)
		}
	}
	if FaithfulnessVerdict("supported-ish").Valid() {
		t.Fatal("an unknown verdict must be invalid")
	}
}

// --- Runner wiring: the check runs where groundedness runs, over the sovereign judge lane ---

func faithfulnessScenario(citations *CitedAnswer) Scenario {
	return Scenario{
		ID: "docs-qa-faithfulness", Agent: AgentDocsQA, Prompt: "cite the security posture",
		Rubric:    Rubric{Kind: RubricContains, Description: "grounded answer", Expected: []string{"untrusted"}},
		Citations: citations,
	}
}

func TestScoreScenarioRunsFaithfulnessOverJudgeLane(t *testing.T) {
	client := &fakeJudgeClient{answer: `{"entailed": true, "rationale": "the chunk states this"}`}
	runner := judgeTestRunner(client)
	config, model := sovereignJudgeConfig()
	config.ScenarioTimeout = 1
	config.PollInterval = 1

	citations := &CitedAnswer{
		Chunks: []CitedChunk{{ID: "docs/security.md#1", Text: "Room content is untrusted input."}},
		Claims: []AnswerClaim{{ID: "c1", Text: "Room content is untrusted.", CitedChunks: []string{"docs/security.md#1"}}},
	}
	_, _, faithfulness, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(model), faithfulnessScenario(citations), "untrusted")
	if err != nil {
		t.Fatalf("scoreScenario: %v", err)
	}
	if faithfulness == nil || faithfulness.Verdict != VerdictPass || faithfulness.Claims[0].Verdict != FaithfulnessSupported {
		t.Fatalf("faithfulness = %+v, want a supported pass", faithfulness)
	}
	// The entailment call must go to the sovereign judge Agent through the local A2A route.
	if len(client.targets) != 1 || client.targets[0].String() != "/api/a2a/kagent/sovereign-judge" {
		t.Fatalf("entailment target = %+v, want the sovereign-judge agent", client.targets)
	}
}

func TestScoreScenarioFaithfulnessFailsClosedOnUnparseableJudge(t *testing.T) {
	client := &fakeJudgeClient{answer: "the claim looks supported to me"}
	runner := judgeTestRunner(client)
	config, model := sovereignJudgeConfig()
	config.ScenarioTimeout = 1
	config.PollInterval = 1

	citations := &CitedAnswer{
		Chunks: []CitedChunk{{ID: "k1", Text: "some corpus text"}},
		Claims: []AnswerClaim{{ID: "c1", Text: "a confident claim", CitedChunks: []string{"k1"}}},
	}
	_, _, faithfulness, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(model), faithfulnessScenario(citations), "untrusted")
	if err != nil {
		t.Fatalf("an unparseable judgment must fail the claim, not the run: %v", err)
	}
	if faithfulness == nil || faithfulness.Verdict != VerdictFail || faithfulness.Claims[0].Verdict != FaithfulnessUnsupported {
		t.Fatalf("unparseable judgment must be unsupported/fail: %+v", faithfulness)
	}
}

func TestScoreScenarioSkipsFaithfulnessForExternalProvider(t *testing.T) {
	// A metered external provider blocks the judge lane: no corpus text may leave the cluster, so the
	// faithfulness check does not run and makes no judge call.
	client := &fakeJudgeClient{answer: `{"entailed": true, "rationale": "x"}`}
	runner := judgeTestRunner(client)
	config, _ := sovereignJudgeConfig()
	config.ScenarioTimeout = 1
	config.PollInterval = 1
	external := modelcatalog.Model{Profile: "vertex", Name: "gemini", Residency: modelcatalog.ResidencyGlobal}

	citations := &CitedAnswer{
		Chunks: []CitedChunk{{ID: "k1", Text: "corpus text"}},
		Claims: []AnswerClaim{{ID: "c1", Text: "a claim", CitedChunks: []string{"k1"}}},
	}
	_, _, faithfulness, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(external), faithfulnessScenario(citations), "untrusted")
	if err != nil {
		t.Fatalf("scoreScenario: %v", err)
	}
	if faithfulness != nil {
		t.Fatalf("faithfulness must not run for an external provider: %+v", faithfulness)
	}
	if len(client.prompts) != 0 {
		t.Fatalf("blocked faithfulness lane must make no judge call, saw %d", len(client.prompts))
	}
}
