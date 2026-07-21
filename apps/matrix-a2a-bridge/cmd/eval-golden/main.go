package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	agentName := flags.String("agent", "", "verify only this lowercase Kubernetes DNS-label Agent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *answerPath == "" {
		return fmt.Errorf("--actual-answer is required")
	}

	if *agentName != "" && !agentNamePattern.MatchString(*agentName) {
		return fmt.Errorf("--agent must be a lowercase Kubernetes DNS label of at most 63 characters")
	}

	suites, err := loadSuites(*evalsPath, *agentName)
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

	var results []evaluation.GoldenResult
	if *agentName == "" {
		results, err = evaluation.VerifyAgentGoldenSuites(
			suites,
			strings.NewReader(string(agentsBytes)),
			strings.NewReader(string(promptsBytes)),
			answers,
		)
	} else {
		results, err = evaluation.VerifyAgentGoldenSuite(
			suites[0],
			strings.NewReader(string(agentsBytes)),
			strings.NewReader(string(promptsBytes)),
			answers,
		)
	}
	if err != nil {
		return err
	}
	for _, result := range results {
		fmt.Printf("PASS %s agent=%s contract=%s\n", result.ScenarioID, result.Agent, result.AgentContractSHA256)
	}
	fmt.Printf("Golden Agent gate passed: %d scenarios, deterministic loopback model, zero external network requests\n", len(results))
	return nil
}

var agentNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func loadSuites(root, agentName string) ([]evaluation.AgentGoldenSuite, error) {
	if agentName != "" {
		suite, err := loadSuite(filepath.Join(root, agentName, "golden.json"), agentName)
		if err != nil {
			return nil, err
		}
		return []evaluation.AgentGoldenSuite{suite}, nil
	}

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
		suite, loadErr := loadSuite(fixturePath, entry.Name())
		if loadErr != nil {
			return nil, loadErr
		}
		suites = append(suites, suite)
	}
	if len(suites) == 0 {
		return nil, fmt.Errorf("no <agent>/golden.json fixtures under %s", root)
	}
	return suites, nil
}

func loadSuite(fixturePath, directoryName string) (evaluation.AgentGoldenSuite, error) {
	fixture, err := os.Open(fixturePath)
	if err != nil {
		return evaluation.AgentGoldenSuite{}, fmt.Errorf("open golden fixture %s: %w", fixturePath, err)
	}
	suite, decodeErr := evaluation.DecodeAgentGoldenSuite(fixture)
	closeErr := fixture.Close()
	if decodeErr != nil {
		return evaluation.AgentGoldenSuite{}, fmt.Errorf("%s: %w", fixturePath, decodeErr)
	}
	if closeErr != nil {
		return evaluation.AgentGoldenSuite{}, fmt.Errorf("close golden fixture %s: %w", fixturePath, closeErr)
	}
	if string(suite.Agent) != directoryName {
		return evaluation.AgentGoldenSuite{}, fmt.Errorf("%s names Agent %q; directory requires %q", fixturePath, suite.Agent, directoryName)
	}
	return suite, nil
}
