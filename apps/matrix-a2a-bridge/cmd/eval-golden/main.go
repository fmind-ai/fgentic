package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/evaluation"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "golden Agent evaluation failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("eval-golden", flag.ContinueOnError)
	evalsPath := flags.String("evals", "../../evals", "directory containing <agent>/golden.json fixtures")
	agentsPath := flags.String("agents", "../../infra/kagent/agent-zoo.yaml", "rendered Agent manifest path")
	promptsPath := flags.String("prompts", "../../infra/kagent/agent-zoo-prompts.yaml", "Agent prompt ConfigMap path")
	answerPath := flags.String("actual-answer", "", "answers returned by the deterministic demo stub (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *answerPath == "" {
		return fmt.Errorf("--actual-answer is required")
	}

	suites, err := loadSuites(*evalsPath)
	if err != nil {
		return err
	}
	agentsBytes, err := os.ReadFile(*agentsPath)
	if err != nil {
		return fmt.Errorf("read Agent manifests: %w", err)
	}
	promptsBytes, err := os.ReadFile(*promptsPath)
	if err != nil {
		return fmt.Errorf("read Agent prompts: %w", err)
	}
	answerFile, err := os.Open(*answerPath)
	if err != nil {
		return fmt.Errorf("open deterministic demo answers: %w", err)
	}
	defer func() { _ = answerFile.Close() }()
	answers, err := evaluation.DecodeGoldenAnswers(answerFile)
	if err != nil {
		return err
	}

	results, err := evaluation.VerifyAgentGoldenSuites(
		suites,
		strings.NewReader(string(agentsBytes)),
		strings.NewReader(string(promptsBytes)),
		answers,
	)
	if err != nil {
		return err
	}
	for _, result := range results {
		fmt.Printf("PASS %s agent=%s contract=%s\n", result.ScenarioID, result.Agent, result.AgentContractSHA256)
	}
	fmt.Printf("Golden Agent gate passed: %d scenarios, deterministic loopback model, zero external network requests\n", len(results))
	return nil
}

func loadSuites(root string) ([]evaluation.AgentGoldenSuite, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read golden fixture directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	suites := make([]evaluation.AgentGoldenSuite, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("unexpected file in evals directory: %s", entry.Name())
		}
		fixturePath := filepath.Join(root, entry.Name(), "golden.json")
		fixture, openErr := os.Open(fixturePath)
		if openErr != nil {
			return nil, fmt.Errorf("open golden fixture %s: %w", fixturePath, openErr)
		}
		suite, decodeErr := evaluation.DecodeAgentGoldenSuite(fixture)
		closeErr := fixture.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("%s: %w", fixturePath, decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close golden fixture %s: %w", fixturePath, closeErr)
		}
		if string(suite.Agent) != entry.Name() {
			return nil, fmt.Errorf("%s names Agent %q; directory requires %q", fixturePath, suite.Agent, entry.Name())
		}
		suites = append(suites, suite)
	}
	if len(suites) == 0 {
		return nil, fmt.Errorf("no <agent>/golden.json fixtures under %s", root)
	}
	return suites, nil
}
