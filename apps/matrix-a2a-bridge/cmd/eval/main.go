package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/evaluation"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		slog.Error("model evaluation failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	defaultOutput := filepath.Join(repoRoot, ".agents", "tmp", "model-eval")
	defaultCatalog := filepath.Join(repoRoot, "infra", "agentgateway", "providers", "model-catalog.yaml")
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	profile := flags.String("profile", "", "operator label for the deployed model profile (required)")
	model := flags.String("model", "", "expected agentgateway request or response model (required)")
	modelCatalogPath := flags.String("model-catalog", defaultCatalog, "governed model catalog path")
	a2aURL := flags.String("a2a-url", "http://127.0.0.1:18080", "agentgateway A2A base URL")
	metricsURL := flags.String("metrics-url", "http://127.0.0.1:15020/metrics", "agentgateway Prometheus exposition URL")
	userID := flags.String("user", "@model-eval:fgentic.localhost", "A2A evaluation identity")
	pricingPath := flags.String("pricing-catalog", "", "optional operator-reviewed pricing catalog JSON")
	jsonPath := flags.String("json-output", filepath.Join(defaultOutput, "report.json"), "machine report path")
	markdownPath := flags.String("markdown-output", filepath.Join(defaultOutput, "comparison.md"), "comparison report path")
	scenarioTimeout := flags.Duration("scenario-timeout", 2*time.Minute, "deadline for each A2A scenario")
	pollInterval := flags.Duration("poll-interval", time.Second, "GetTask polling interval")
	quietWindow := flags.Duration("quiet-window", 2*time.Second, "metric stability window before and after each scenario")
	judge := flags.Bool("judge", false, "enable the sovereign LLM-as-judge scoring lane (runs only on a self-hosted model)")
	judgeAgent := flags.String("judge-agent", "sovereign-judge", "kagent Agent backing the self-hosted judge model")
	judgeMinGroundedness := flags.Float64("judge-min-groundedness", 0.7, "minimum judge groundedness score to pass, in [0,1]")
	judgeMinTaskSuccess := flags.Float64("judge-min-task-success", 0.6, "minimum judge task-success score to pass, in [0,1]")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *profile == "" || *model == "" {
		return fmt.Errorf("--profile and --model are required")
	}
	catalog, err := openModelCatalog(*modelCatalogPath)
	if err != nil {
		return err
	}
	governedModel, err := resolveGovernedModel(catalog, *profile, *model)
	if err != nil {
		return err
	}

	var pricing *evaluation.PricingCatalog
	if *pricingPath != "" {
		if governedModel.CostRef != evaluation.PricingSchemaVersion {
			return fmt.Errorf("catalog model %s/%s does not reference pricing schema %s", *profile, *model, evaluation.PricingSchemaVersion)
		}
		file, err := os.Open(*pricingPath)
		if err != nil {
			return fmt.Errorf("open pricing catalog: %w", err)
		}
		pricing, err = evaluation.DecodePricingCatalog(file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("close pricing catalog: %w", closeErr)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	metrics, err := evaluation.NewPrometheusReader(*metricsURL, 10*time.Second)
	if err != nil {
		return err
	}
	client := a2aclient.New(*a2aURL, os.Getenv("A2A_API_KEY"), logger)
	runner := evaluation.NewRunner(client, metrics, pricing, logger)
	scenarios := evaluation.Scenarios()
	digest, err := evaluation.SuiteDigest(scenarios)
	if err != nil {
		return err
	}
	runResult, err := runner.Run(ctx, evaluation.RunConfig{
		Profile: *profile, Model: governedModel, UserID: *userID,
		ScenarioTimeout: *scenarioTimeout, PollInterval: *pollInterval, QuietWindow: *quietWindow,
		Judge: evaluation.JudgeConfig{
			Enabled: *judge, Agent: evaluation.Agent(*judgeAgent),
			Thresholds: evaluation.JudgeThresholds{
				MinGroundedness: *judgeMinGroundedness, MinTaskSuccess: *judgeMinTaskSuccess,
			},
		},
	}, scenarios)
	if err != nil {
		return err
	}

	report := evaluation.NewReport(time.Now(), digest)
	loaded, err := evaluation.LoadReport(*jsonPath)
	if err == nil {
		if loaded.SuiteDigest != digest {
			return fmt.Errorf("existing report uses scenario digest %q; move it aside before evaluating suite %q", loaded.SuiteDigest, digest)
		}
		report = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := evaluation.MergeRun(&report, runResult); err != nil {
		return err
	}
	if err := evaluation.WriteReports(report, *jsonPath, *markdownPath); err != nil {
		return err
	}
	logger.Info("model evaluation report written", "json", *jsonPath, "markdown", *markdownPath)
	return nil
}

func openModelCatalog(path string) (*modelcatalog.Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open model catalog: %w", err)
	}
	catalog, err := modelcatalog.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close model catalog: %w", closeErr)
	}
	return catalog, nil
}

func resolveGovernedModel(catalog *modelcatalog.Catalog, profile, model string) (modelcatalog.Model, error) {
	governedModel, err := catalog.ResolveProfile(profile, model)
	if err != nil {
		return modelcatalog.Model{}, err
	}
	if !governedModel.Supports(modelcatalog.CapabilityChat) {
		return modelcatalog.Model{}, fmt.Errorf("catalog model %s/%s does not declare chat capability", profile, model)
	}
	return governedModel, nil
}

func findRepoRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(directory, ".agents")); err == nil && info.IsDir() {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("repository root with .agents directory not found")
		}
		directory = parent
	}
}
