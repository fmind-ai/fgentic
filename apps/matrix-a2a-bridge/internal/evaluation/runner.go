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
	if err := ValidateScenarios(scenarios); err != nil {
		return ProfileRun{}, err
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
		scenarioCtx, cancel := context.WithTimeout(a2aclient.WithUser(ctx, config.UserID), config.ScenarioTimeout)
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
		score, err := ScoreAnswer(answer, scenario.Rubric)
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
			Usage: usage, EstimatedCost: cost, Score: score,
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
	if observed.RequestModel != expected.Name && observed.ResponseModel != expected.Name {
		return fmt.Errorf("observed model %q/%q, catalog requires %q", observed.RequestModel, observed.ResponseModel, expected.Name)
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
	target, err := a2aclient.NewLocalTarget("/api/a2a/kagent/" + string(scenario.Agent))
	if err != nil {
		return "", fmt.Errorf("build local A2A target: %w", err)
	}
	result, err := r.a2a.Call(ctx, target, scenario.Prompt, "", nil)
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
