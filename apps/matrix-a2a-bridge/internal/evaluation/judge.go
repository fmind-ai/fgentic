package evaluation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

// JudgeConfig enables and bounds the sovereign LLM-as-judge scoring lane. When Enabled, a scenario
// whose rubric is RubricOptionalLLMJudge is scored by submitting the agent answer plus the rubric to
// the Agent judge over the existing A2A/agentgateway route, but only if the run's model is
// self-hosted (see ApprovedFor) — never against a metered external provider.
type JudgeConfig struct {
	Enabled    bool
	Agent      Agent
	Thresholds JudgeThresholds
}

// Validate rejects an enabled-but-incomplete judge configuration fail-fast.
func (c JudgeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Agent == "" {
		return fmt.Errorf("judge lane is enabled but names no judge Agent")
	}
	return c.Thresholds.Validate()
}

// ApprovedFor reports whether the judge lane may run against a model. It is the approved-profile
// guard: the judge only runs when the selected model is served in-cluster (ResidencySelfHosted), so
// eval prompts and agent outputs never leave the cluster and no metered external provider is invoked.
func (c JudgeConfig) ApprovedFor(model modelcatalog.Model) bool {
	return c.Enabled && model.Residency == modelcatalog.ResidencySelfHosted
}

// JudgeResult is the validated, trusted-type rendering of one judge model verdict. It is parsed at
// the boundary from the judge's raw output: the two scores are bounded to [0,1] and the rationale is
// used only to justify the parse (it is never persisted, keeping recorded evidence payload-free).
type JudgeResult struct {
	// Groundedness rates how well the agent answer is supported by the evidence it cites, in [0,1].
	Groundedness float64
	// TaskSuccess rates how well the agent answer accomplished the scenario task, in [0,1].
	TaskSuccess float64
	// Rationale is the judge's short justification. It is validated (must be present) but deliberately
	// dropped from all recorded output so no free-form model text becomes eval evidence.
	Rationale string
}

// judgeWireResult is the exact on-the-wire contract the judge model must emit. Strict decoding with
// DisallowUnknownFields means any extra prose, missing field, or wrong shape fails closed rather than
// silently scoring a scenario as passed.
type judgeWireResult struct {
	Groundedness *float64 `json:"groundedness"`
	TaskSuccess  *float64 `json:"task_success"`
	Rationale    string   `json:"rationale"`
}

// ParseJudgeResult parses one judge model response into a trusted JudgeResult at the boundary. It
// fails closed on any deviation: non-JSON output, extra or missing fields, a non-string rationale, or
// a score outside [0,1]. A malformed judge answer must never be treated as a passing score.
func ParseJudgeResult(raw string) (JudgeResult, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return JudgeResult{}, fmt.Errorf("judge returned an empty response")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	decoder.DisallowUnknownFields()
	var wire judgeWireResult
	if err := decoder.Decode(&wire); err != nil {
		// The raw json error can embed judge-authored content (an unknown field name, an offending
		// character), so it is deliberately dropped: every error this function returns must be
		// content-free so a caller can log it without leaking model output (#355 payload-free invariant).
		return JudgeResult{}, fmt.Errorf("judge result is not the expected JSON contract")
	}
	if decoder.More() {
		return JudgeResult{}, fmt.Errorf("judge result carries trailing content after the JSON object")
	}
	if wire.Groundedness == nil || wire.TaskSuccess == nil {
		return JudgeResult{}, fmt.Errorf("judge result is missing a required score field")
	}
	if err := validateScore("groundedness", *wire.Groundedness); err != nil {
		return JudgeResult{}, err
	}
	if err := validateScore("task_success", *wire.TaskSuccess); err != nil {
		return JudgeResult{}, err
	}
	if strings.TrimSpace(wire.Rationale) == "" {
		return JudgeResult{}, fmt.Errorf("judge result is missing a rationale")
	}
	return JudgeResult{
		Groundedness: *wire.Groundedness,
		TaskSuccess:  *wire.TaskSuccess,
		Rationale:    wire.Rationale,
	}, nil
}

func validateScore(field string, value float64) error {
	if value < 0 || value > 1 {
		// The field name is a fixed identifier; the offending value is omitted to keep the error
		// content-free (it is a judge-emitted number).
		return fmt.Errorf("judge %s score is outside [0,1]", field)
	}
	return nil
}

// JudgeThresholds are the minimum groundedness and task-success scores a scenario must reach to pass
// the judge lane. Both bounds live in [0,1]; a scenario scoring below either fails.
type JudgeThresholds struct {
	MinGroundedness float64
	MinTaskSuccess  float64
}

// Validate rejects an out-of-range threshold fail-fast so a misconfigured lane cannot pass anything.
func (t JudgeThresholds) Validate() error {
	if err := validateScore("minimum groundedness", t.MinGroundedness); err != nil {
		return err
	}
	if err := validateScore("minimum task success", t.MinTaskSuccess); err != nil {
		return err
	}
	return nil
}

// JudgeScores is the content-free score pair recorded alongside a judged scenario result. It holds
// only the two bounded numbers — never the judge prompt, rationale, or agent output.
type JudgeScores struct {
	Groundedness float64 `json:"groundedness"`
	TaskSuccess  float64 `json:"task_success"`
}

// Scores projects the content-free score pair for recording.
func (r JudgeResult) Scores() JudgeScores {
	return JudgeScores{Groundedness: r.Groundedness, TaskSuccess: r.TaskSuccess}
}

// Score turns a judge result into a deterministic pass/fail verdict against the thresholds. The
// reason carries only the bounded numeric scores, keeping the recorded evidence payload-free.
func (r JudgeResult) Score(thresholds JudgeThresholds) Score {
	pass := 1.0
	fail := 0.0
	if r.Groundedness < thresholds.MinGroundedness || r.TaskSuccess < thresholds.MinTaskSuccess {
		return Score{
			Verdict: VerdictFail, Points: &fail,
			Reason: fmt.Sprintf(
				"judge groundedness %.2f/%.2f, task_success %.2f/%.2f",
				r.Groundedness, thresholds.MinGroundedness, r.TaskSuccess, thresholds.MinTaskSuccess,
			),
		}
	}
	return Score{
		Verdict: VerdictPass, Points: &pass,
		Reason: fmt.Sprintf(
			"judge groundedness %.2f, task_success %.2f",
			r.Groundedness, r.TaskSuccess,
		),
	}
}

// judgePrompt renders the fixed judge instruction from a scenario rubric and the agent answer. The
// judge is asked to return only the strict JSON contract ParseJudgeResult accepts. This is a fixed
// rubric-driven template, not model-authored, so the lane wiring — not the judge — owns the contract.
func judgePrompt(rubricDescription, prompt, answer string) string {
	var b strings.Builder
	b.WriteString("You are a strict, sovereign evaluation judge. Score the ASSISTANT answer against ")
	b.WriteString("the RUBRIC and the original TASK. Respond with ONLY a single JSON object and no ")
	b.WriteString("other text, exactly of the form ")
	b.WriteString(`{"groundedness": <0..1>, "task_success": <0..1>, "rationale": "<one sentence>"}. `)
	b.WriteString("groundedness rates how well the answer is supported by evidence it cites; ")
	b.WriteString("task_success rates how fully it accomplished the task.\n\n")
	b.WriteString("RUBRIC: ")
	b.WriteString(rubricDescription)
	b.WriteString("\n\nTASK: ")
	b.WriteString(prompt)
	b.WriteString("\n\nASSISTANT ANSWER: ")
	b.WriteString(answer)
	return b.String()
}
