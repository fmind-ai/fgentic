package evaluation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

// fakeJudgeClient is a deterministic A2AClient stub: it records the targets/prompts it saw and
// returns a canned terminal answer (or a transport error) so the judge lane can be exercised with no
// cluster or model.
type fakeJudgeClient struct {
	answer  string
	err     error
	targets []a2aclient.Target
	prompts []string
}

func (f *fakeJudgeClient) Call(_ context.Context, target a2aclient.Target, prompt, _ string, _ []a2aclient.InboundFile) (a2aclient.Result, error) {
	f.targets = append(f.targets, target)
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return a2aclient.Result{}, f.err
	}
	return a2aclient.Result{Text: f.answer, Terminal: true}, nil
}

func (f *fakeJudgeClient) PollTask(context.Context, a2aclient.Target, string) (a2aclient.Result, error) {
	return a2aclient.Result{}, errors.New("poll not expected")
}

func judgeTestRunner(client A2AClient) *Runner {
	return NewRunner(client, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func judgeScenario() Scenario {
	return Scenario{
		ID: "judge-1", Agent: "k8s", Prompt: "inspect the pod",
		Rubric: Rubric{Kind: RubricOptionalLLMJudge, Description: "rate diagnostic relevance"},
	}
}

func sovereignJudgeConfig() (RunConfig, modelcatalog.Model) {
	model := modelcatalog.Model{Profile: "vllm", Name: "qwen", Residency: modelcatalog.ResidencySelfHosted}
	return RunConfig{
		Profile: "vllm", Model: model, UserID: "@eval:fgentic",
		ScenarioTimeout: 0, PollInterval: 0, // unused by scoreScenario's single Call
		Judge: JudgeConfig{
			Enabled: true, Agent: "sovereign-judge",
			Thresholds: JudgeThresholds{MinGroundedness: 0.7, MinTaskSuccess: 0.6},
		},
	}, model
}

func TestJudgeLaneScoresApprovedSovereignScenario(t *testing.T) {
	client := &fakeJudgeClient{answer: `{"groundedness": 0.9, "task_success": 0.8, "rationale": "cited pod evidence"}`}
	runner := judgeTestRunner(client)
	config, model := sovereignJudgeConfig()
	config.ScenarioTimeout = 1 // positive so WithTimeout is valid

	score, scores, _, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(model), judgeScenario(), "the pod is CrashLooping; restart it")
	if err != nil {
		t.Fatalf("scoreScenario: %v", err)
	}
	if score.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want pass (%s)", score.Verdict, score.Reason)
	}
	if scores == nil || scores.Groundedness != 0.9 || scores.TaskSuccess != 0.8 {
		t.Fatalf("judge scores = %+v, want 0.9/0.8", scores)
	}
	if len(client.targets) != 1 || client.targets[0].String() != "/api/a2a/kagent/sovereign-judge" {
		t.Fatalf("judge target = %+v, want the sovereign-judge agent", client.targets)
	}
}

func TestJudgeLaneFailsClosedOnMalformedOutput(t *testing.T) {
	client := &fakeJudgeClient{answer: "the answer looks pretty good to me"}
	runner := judgeTestRunner(client)
	config, model := sovereignJudgeConfig()
	config.ScenarioTimeout = 1

	score, scores, _, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(model), judgeScenario(), "some answer")
	if err != nil {
		t.Fatalf("malformed judge output must fail the scenario, not the run: %v", err)
	}
	if score.Verdict != VerdictFail {
		t.Fatalf("verdict = %q, want fail-closed", score.Verdict)
	}
	if scores != nil {
		t.Fatalf("no judge scores should be recorded for malformed output: %+v", scores)
	}
}

func TestJudgeLaneTransportErrorAbortsRun(t *testing.T) {
	client := &fakeJudgeClient{err: errors.New("judge unreachable")}
	runner := judgeTestRunner(client)
	config, model := sovereignJudgeConfig()
	config.ScenarioTimeout = 1

	_, _, _, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(model), judgeScenario(), "answer")
	if err == nil {
		t.Fatal("a judge transport failure must abort the run")
	}
}

func TestJudgeLaneBlockedForExternalProvider(t *testing.T) {
	client := &fakeJudgeClient{answer: `{"groundedness":1,"task_success":1,"rationale":"x"}`}
	runner := judgeTestRunner(client)
	config, _ := sovereignJudgeConfig()
	// A metered external provider: residency is not self-hosted, so the guard blocks the judge lane.
	external := modelcatalog.Model{Profile: "vertex", Name: "gemini", Residency: modelcatalog.ResidencyGlobal}
	config.Model = external

	score, scores, _, err := runner.scoreScenario(t.Context(), config, config.Judge.ApprovedFor(external), judgeScenario(), "answer")
	if err != nil {
		t.Fatalf("scoreScenario: %v", err)
	}
	if score.Verdict != VerdictUnscored {
		t.Fatalf("verdict = %q, want unscored (judge lane blocked)", score.Verdict)
	}
	if scores != nil || len(client.prompts) != 0 {
		t.Fatalf("blocked judge lane must make no judge call: scores=%+v calls=%d", scores, len(client.prompts))
	}
}

func TestJudgeConfigApprovedForResidency(t *testing.T) {
	cfg := JudgeConfig{Enabled: true, Agent: "j", Thresholds: JudgeThresholds{MinGroundedness: 0.5, MinTaskSuccess: 0.5}}
	if !cfg.ApprovedFor(modelcatalog.Model{Residency: modelcatalog.ResidencySelfHosted}) {
		t.Fatal("self-hosted model must be approved")
	}
	for _, res := range []modelcatalog.Residency{modelcatalog.ResidencyEU, modelcatalog.ResidencyGlobal} {
		if cfg.ApprovedFor(modelcatalog.Model{Residency: res}) {
			t.Fatalf("residency %q must not be approved for the sovereign judge lane", res)
		}
	}
	if (JudgeConfig{Enabled: false}).ApprovedFor(modelcatalog.Model{Residency: modelcatalog.ResidencySelfHosted}) {
		t.Fatal("a disabled judge lane is never approved")
	}
}

func TestJudgeConfigValidate(t *testing.T) {
	if err := (JudgeConfig{Enabled: false}).Validate(); err != nil {
		t.Fatalf("disabled config must validate: %v", err)
	}
	if err := (JudgeConfig{Enabled: true}).Validate(); err == nil {
		t.Fatal("enabled config with no agent must fail")
	}
	if err := (JudgeConfig{Enabled: true, Agent: "j", Thresholds: JudgeThresholds{MinGroundedness: 2}}).Validate(); err == nil {
		t.Fatal("out-of-range threshold must fail validation")
	}
}
