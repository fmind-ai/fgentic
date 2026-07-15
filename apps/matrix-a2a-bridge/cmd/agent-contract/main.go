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
		_, _ = fmt.Fprintf(os.Stderr, "Agent contract digest failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("agent-contract", flag.ContinueOnError)
	agentName := flags.String("agent", "", "rendered Agent name (required)")
	manifestPath := flags.String("manifest", "", "rendered manifest containing Agents and agent-zoo-prompts (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *agentName == "" || *manifestPath == "" {
		return fmt.Errorf("--agent and --manifest are required")
	}
	manifest, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("read rendered manifest: %w", err)
	}
	digest, err := evaluation.AgentContractDigest(
		*agentName,
		strings.NewReader(string(manifest)),
		strings.NewReader(string(manifest)),
	)
	if err != nil {
		return err
	}
	fmt.Println(digest)
	return nil
}
