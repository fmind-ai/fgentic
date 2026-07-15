package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/evaluation"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "golden agent evaluation failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("eval-golden", flag.ContinueOnError)
	suitePath := flags.String("suite", "internal/evaluation/testdata/golden-agent-responses.json", "golden fixture path")
	agentsPath := flags.String("agents", "../../infra/kagent/agent-zoo.yaml", "shipped Agent manifest path")
	promptsPath := flags.String("prompts", "../../infra/kagent/agent-zoo-prompts.yaml", "Agent prompt ConfigMap path")
	answerPath := flags.String("actual-answer", "", "answer returned by the extracted deterministic demo stub (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *answerPath == "" {
		return fmt.Errorf("--actual-answer is required")
	}

	suiteBytes, err := os.ReadFile(*suitePath)
	if err != nil {
		return fmt.Errorf("read golden suite: %w", err)
	}
	suite, err := evaluation.DecodeGoldenSuite(strings.NewReader(string(suiteBytes)))
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

	results, err := evaluation.VerifyGoldenSuite(
		suite,
		evaluation.Scenarios(),
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
	fmt.Printf("Golden agent gate passed: %d shipped agents, deterministic demo response, zero external network requests\n", len(results))
	return nil
}
