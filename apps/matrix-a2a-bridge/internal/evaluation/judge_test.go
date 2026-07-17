package evaluation

import (
	"strings"
	"testing"
)

func TestParseJudgeResultAcceptsExactContract(t *testing.T) {
	raw := `{"groundedness": 0.9, "task_success": 0.75, "rationale": "cites evidence and answers"}`
	got, err := ParseJudgeResult(raw)
	if err != nil {
		t.Fatalf("ParseJudgeResult: %v", err)
	}
	if got.Groundedness != 0.9 || got.TaskSuccess != 0.75 {
		t.Fatalf("scores = %+v, want 0.9/0.75", got)
	}
	if got.Scores().Groundedness != 0.9 || got.Scores().TaskSuccess != 0.75 {
		t.Fatalf("Scores() = %+v", got.Scores())
	}
}

func TestParseJudgeResultFailsClosed(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"not json":          "the answer looks good",
		"prose around json": `Here is my verdict: {"groundedness":1,"task_success":1,"rationale":"x"}`,
		"trailing content":  `{"groundedness":1,"task_success":1,"rationale":"x"} and more`,
		"missing field":     `{"groundedness": 0.5, "rationale": "x"}`,
		"unknown field":     `{"groundedness":1,"task_success":1,"rationale":"x","verdict":"pass"}`,
		"score above one":   `{"groundedness": 1.5, "task_success": 1, "rationale": "x"}`,
		"score below zero":  `{"groundedness": -0.1, "task_success": 1, "rationale": "x"}`,
		"empty rationale":   `{"groundedness": 1, "task_success": 1, "rationale": "  "}`,
		"wrong type":        `{"groundedness": "high", "task_success": 1, "rationale": "x"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJudgeResult(raw); err == nil {
				t.Fatalf("expected a fail-closed error for %q", raw)
			}
		})
	}
}

func TestParseJudgeResultErrorsAreContentFree(t *testing.T) {
	// A judge answer influenced by scenario content must not smuggle model-authored text into the
	// error the caller logs. Inject a distinctive marker as a field name and a score value and assert
	// neither reaches the returned error message (#355 payload-free invariant).
	const marker = "EXFIL-marker-73"
	cases := []string{
		`{"groundedness":1,"task_success":1,"rationale":"x","` + marker + `":0}`, // unknown field name
		`{"groundedness": 4273, "task_success": 1, "rationale": "x"}`,            // out-of-range value
		`prose ` + marker + ` not json`,                                          // offending characters
	}
	for _, raw := range cases {
		_, err := ParseJudgeResult(raw)
		if err == nil {
			t.Fatalf("expected an error for %q", raw)
		}
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "4273") {
			t.Fatalf("judge parse error leaked model content: %q", err.Error())
		}
	}
}

func TestJudgeResultScoreAgainstThresholds(t *testing.T) {
	thresholds := JudgeThresholds{MinGroundedness: 0.7, MinTaskSuccess: 0.6}
	if err := thresholds.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	pass := JudgeResult{Groundedness: 0.8, TaskSuccess: 0.7, Rationale: "ok"}.Score(thresholds)
	if pass.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want pass", pass.Verdict)
	}

	// Below either threshold fails.
	lowGround := JudgeResult{Groundedness: 0.6, TaskSuccess: 0.9, Rationale: "ok"}.Score(thresholds)
	lowTask := JudgeResult{Groundedness: 0.9, TaskSuccess: 0.5, Rationale: "ok"}.Score(thresholds)
	if lowGround.Verdict != VerdictFail || lowTask.Verdict != VerdictFail {
		t.Fatalf("expected both below-threshold results to fail: %q/%q", lowGround.Verdict, lowTask.Verdict)
	}
}

func TestJudgeScoreReasonIsPayloadFree(t *testing.T) {
	// The recorded reason must carry only bounded numbers, never the judge rationale or agent text.
	result := JudgeResult{Groundedness: 0.9, TaskSuccess: 0.8, Rationale: "SENSITIVE agent output echoed"}
	score := result.Score(JudgeThresholds{MinGroundedness: 0.5, MinTaskSuccess: 0.5})
	if strings.Contains(score.Reason, "SENSITIVE") {
		t.Fatalf("judge reason leaked the rationale: %q", score.Reason)
	}
}

func TestJudgeThresholdsValidateRejectsOutOfRange(t *testing.T) {
	if err := (JudgeThresholds{MinGroundedness: 1.2, MinTaskSuccess: 0.5}).Validate(); err == nil {
		t.Fatal("expected an out-of-range threshold to be rejected")
	}
}
