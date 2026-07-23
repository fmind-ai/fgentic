package evaluation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

// A2AClient is the existing bridge client surface needed by the harness.
type A2AClient interface {
	Call(context.Context, a2aclient.Target, string, string, []a2aclient.InboundFile) (a2aclient.Result, error)
	PollTask(context.Context, a2aclient.Target, string) (a2aclient.Result, error)
}

// RunConfig defines one approved profile run and its local safety bounds.
type RunConfig struct {
	Profile         string
	Model           modelcatalog.Model
	UserID          string
	ScenarioTimeout time.Duration
	PollInterval    time.Duration
	QuietWindow     time.Duration
	// Judge optionally enables the sovereign LLM-as-judge scoring lane for RubricOptionalLLMJudge
	// scenarios. It runs only when the run's model is self-hosted (JudgeConfig.ApprovedFor).
	Judge JudgeConfig
}

// Runner executes fixed scenarios over A2A and attributes direct metric deltas.
type Runner struct {
	a2a     A2AClient
	metrics MetricsReader
	pricing *PricingCatalog
	log     *slog.Logger
}

// NewRunner composes the existing A2A client, metric reader, and optional catalog.
func NewRunner(a2a A2AClient, metrics MetricsReader, pricing *PricingCatalog, log *slog.Logger) *Runner {
	return &Runner{a2a: a2a, metrics: metrics, pricing: pricing, log: log}
}

// Run executes a complete fixed suite and fails before producing a partial report.
func (r *Runner) Run(ctx context.Context, config RunConfig, scenarios []Scenario) (ProfileRun, error) {
	if config.Profile == "" || config.Model.Name == "" || config.UserID == "" {
		return ProfileRun{}, fmt.Errorf("profile, governed model, and user ID are required")
	}
	if config.Model.Profile != config.Profile {
		return ProfileRun{}, fmt.Errorf("catalog model profile %q does not match requested profile %q", config.Model.Profile, config.Profile)
	}
	if !config.Model.Supports(modelcatalog.CapabilityChat) {
		return ProfileRun{}, fmt.Errorf("catalog model %s/%s does not declare chat capability", config.Profile, config.Model.Name)
	}
	if config.ScenarioTimeout <= 0 || config.PollInterval <= 0 || config.QuietWindow <= 0 {
		return ProfileRun{}, fmt.Errorf("scenario timeout, poll interval, and quiet window must be positive")
	}
	if err := config.Judge.Validate(); err != nil {
		return ProfileRun{}, fmt.Errorf("judge lane: %w", err)
	}
	if err := ValidateScenarios(scenarios); err != nil {
		return ProfileRun{}, err
	}
	judgeApproved := config.Judge.ApprovedFor(config.Model)
	if config.Judge.Enabled && !judgeApproved {
		r.log.Info("judge lane blocked by approved-profile guard",
			"profile", config.Profile, "residency", string(config.Model.Residency))
	}

	run := ProfileRun{
		Profile: config.Profile, RequestedModel: config.Model.Name,
		StartedAt: time.Now().UTC(), Results: make([]ScenarioResult, 0, len(scenarios)),
	}
	for index, scenario := range scenarios {
		r.log.Info("running evaluation scenario", "position", index+1, "total", len(scenarios), "id", scenario.ID)
		before, err := r.stableSnapshot(ctx, config.QuietWindow)
		if err != nil {
			return ProfileRun{}, fmt.Errorf("scenario %s preflight: %w", scenario.ID, err)
		}

		started := time.Now()
		// The fixed evaluation suite contains only repository-published prompts and bypasses Matrix
		// mappings, so bind its reviewed public class explicitly instead of relying on the local
		// transport's fail-closed regulated default.
		policyCtx := a2aclient.WithDataClassification(
			a2aclient.WithUser(ctx, config.UserID),
			modelcatalog.ClassificationPublic,
		)
		scenarioCtx, cancel := context.WithTimeout(policyCtx, config.ScenarioTimeout)
		answer, err := r.callScenario(scenarioCtx, scenario, config.PollInterval)
		cancel()
		if err != nil {
			return ProfileRun{}, fmt.Errorf("scenario %s: %w", scenario.ID, err)
		}
		latency := time.Since(started)

		after, err := r.stableSnapshot(ctx, config.QuietWindow)
		if err != nil {
			return ProfileRun{}, fmt.Errorf("scenario %s postflight: %w", scenario.ID, err)
		}
		usage, err := MetricsDelta(before, after)
		if err != nil {
			return ProfileRun{}, fmt.Errorf("scenario %s metrics: %w", scenario.ID, err)
		}
		if err := validateObservedModel(config.Model, usage.Identity); err != nil {
			return ProfileRun{}, fmt.Errorf("scenario %s: %w", scenario.ID, err)
		}
		score, judgeScores, faithfulness, err := r.scoreScenario(policyCtx, config, judgeApproved, scenario, answer)
		if err != nil {
			return ProfileRun{}, fmt.Errorf("score scenario %s: %w", scenario.ID, err)
		}
		cost, err := r.pricing.Estimate(usage)
		if err != nil {
			return ProfileRun{}, fmt.Errorf("price scenario %s: %w", scenario.ID, err)
		}
		run.Results = append(run.Results, ScenarioResult{
			ScenarioID: scenario.ID, Agent: scenario.Agent, Prompt: scenario.Prompt,
			Rubric: scenario.Rubric, Answer: answer, LatencyMilliseconds: latency.Milliseconds(),
			Usage: usage, EstimatedCost: cost, Score: score, JudgeScores: judgeScores,
			Faithfulness: faithfulness,
		})
	}
	run.CompletedAt = time.Now().UTC()
	summary, err := BuildSummary(run.Results)
	if err != nil {
		return ProfileRun{}, err
	}
	run.Summary = summary
	return run, nil
}

func validateObservedModel(expected modelcatalog.Model, observed ProviderIdentity) error {
	if observed.System != expected.GenAISystem {
		return fmt.Errorf("observed gen_ai_system %q, catalog requires %q", observed.System, expected.GenAISystem)
	}
	observedModel := false
	for label, model := range map[string]string{
		"gen_ai_request_model":  observed.RequestModel,
		"gen_ai_response_model": observed.ResponseModel,
	} {
		if model == "" || model == "unknown" {
			continue
		}
		observedModel = true
		if model != expected.Name {
			return fmt.Errorf("observed %s %q, catalog requires %q", label, model, expected.Name)
		}
	}
	if !observedModel {
		return fmt.Errorf("observed no model identity, catalog requires %q", expected.Name)
	}
	return nil
}

func (r *Runner) stableSnapshot(ctx context.Context, quietWindow time.Duration) (MetricsSnapshot, error) {
	first, err := r.metrics.Snapshot(ctx)
	if err != nil {
		return MetricsSnapshot{}, err
	}
	timer := time.NewTimer(quietWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return MetricsSnapshot{}, ctx.Err()
	case <-timer.C:
	}
	second, err := r.metrics.Snapshot(ctx)
	if err != nil {
		return MetricsSnapshot{}, err
	}
	if err := MetricsStable(first, second); err != nil {
		return MetricsSnapshot{}, err
	}
	return second, nil
}

func (r *Runner) callScenario(ctx context.Context, scenario Scenario, pollInterval time.Duration) (string, error) {
	return r.callAgent(ctx, scenario.Agent, scenario.Prompt, pollInterval)
}

// scoreScenario applies the judge lane to a RubricOptionalLLMJudge scenario when the lane is enabled
// and approved for the run's self-hosted model; otherwise it applies the deterministic local rubric.
// A judge transport failure aborts the run (like a scenario call failure); malformed judge output
// fails the scenario fail-closed rather than silently passing it.
func (r *Runner) scoreScenario(
	ctx context.Context,
	config RunConfig,
	judgeApproved bool,
	scenario Scenario,
	answer string,
) (Score, *JudgeScores, *FaithfulnessResult, error) {
	// Citation faithfulness runs in the same guarded lane as groundedness: only when the sovereign judge
	// lane is approved (self-hosted model), so corpus text never leaves the cluster. It is independent
	// of the rubric kind — any corpus-cited answer is checked, not just optional-judge scenarios.
	var faithfulness *FaithfulnessResult
	if scenario.Citations != nil && judgeApproved {
		result, err := r.scoreFaithfulness(ctx, config, *scenario.Citations)
		if err != nil {
			return Score{}, nil, nil, fmt.Errorf("faithfulness: %w", err)
		}
		faithfulness = &result
	}
	if scenario.Rubric.Kind != RubricOptionalLLMJudge || !judgeApproved {
		score, err := ScoreAnswer(answer, scenario.Rubric)
		return score, nil, faithfulness, err
	}
	judgeCtx, cancel := context.WithTimeout(ctx, config.ScenarioTimeout)
	defer cancel()
	raw, err := r.callAgent(judgeCtx, config.Judge.Agent, judgePrompt(scenario.Rubric.Description, scenario.Prompt, answer), config.PollInterval)
	if err != nil {
		return Score{}, nil, nil, fmt.Errorf("judge call: %w", err)
	}
	result, parseErr := ParseJudgeResult(raw)
	if parseErr != nil {
		// Fail closed with a payload-free reason; the parse detail is logged, never recorded.
		r.log.Warn("judge output failed strict validation", "scenario", scenario.ID, "error", parseErr)
		fail := 0.0
		return Score{Verdict: VerdictFail, Points: &fail, Reason: "judge output failed strict validation"}, nil, faithfulness, nil
	}
	scores := result.Scores()
	return result.Score(config.Judge.Thresholds), &scores, faithfulness, nil
}

// scoreFaithfulness runs the citation-faithfulness check for a scenario's corpus-cited answer over the
// sovereign judge lane, mirroring the groundedness lane's egress exactly: each claim and its cited
// chunk text reach only the local judge Agent through agentgateway (r.callAgent), never an external
// provider. It is invoked only from the judge-approved path, so no corpus text can leave the cluster.
// A judge transport failure aborts the run; a malformed judgment fails that claim closed (unsupported).
func (r *Runner) scoreFaithfulness(ctx context.Context, config RunConfig, answer CitedAnswer) (FaithfulnessResult, error) {
	if err := answer.Validate(); err != nil {
		return FaithfulnessResult{}, err
	}
	judge := func(judgeCtx context.Context, claimText string, chunkTexts []string) (EntailmentResult, error) {
		callCtx, cancel := context.WithTimeout(judgeCtx, config.ScenarioTimeout)
		defer cancel()
		raw, err := r.callAgent(callCtx, config.Judge.Agent, entailmentPrompt(claimText, chunkTexts), config.PollInterval)
		if err != nil {
			return EntailmentResult{}, fmt.Errorf("entailment judge call: %w", err)
		}
		result, parseErr := ParseEntailmentResult(raw)
		if parseErr != nil {
			// Fail closed: a malformed judgment is scored "unsupported", never "supported". The parse
			// detail is logged content-free and never becomes a verdict or recorded evidence.
			r.log.Warn("entailment judge output failed strict validation", "error", parseErr)
			return EntailmentResult{Entailed: false, Rationale: "unparseable"}, nil
		}
		return result, nil
	}
	return ScoreFaithfulness(ctx, judge, answer)
}

// callAgent submits one prompt to a local kagent agent over A2A and returns its terminal text answer,
// polling a long task without holding a worker. It is shared by the scenario and judge lanes.
func (r *Runner) callAgent(ctx context.Context, agent Agent, prompt string, pollInterval time.Duration) (string, error) {
	target, err := a2aclient.NewLocalTarget("/api/a2a/kagent/" + string(agent))
	if err != nil {
		return "", fmt.Errorf("build local A2A target: %w", err)
	}
	result, err := r.a2a.Call(ctx, target, prompt, "", nil)
	if err != nil {
		return "", err
	}
	for !result.Terminal {
		if result.TaskID == "" {
			return "", fmt.Errorf("agent returned a non-terminal result without a task ID")
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
		result, err = r.a2a.PollTask(ctx, target, result.TaskID)
		if err != nil {
			return "", err
		}
	}
	if result.Failed {
		return "", fmt.Errorf("agent task failed")
	}
	if result.Text == "" {
		return "", fmt.Errorf("agent returned an empty answer")
	}
	return result.Text, nil
}
