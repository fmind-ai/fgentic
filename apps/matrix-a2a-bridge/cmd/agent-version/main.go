package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/bridge"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Agent version failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("agent-version", flag.ContinueOnError)
	configPath := flags.String("config", "", "rendered agents.yaml path (required)")
	ghost := flags.String("ghost", "", "ghost localpart without @ (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *ghost == "" {
		return fmt.Errorf("--config and --ghost are required")
	}
	agents, err := bridge.LoadAgents(*configPath)
	if err != nil {
		return err
	}
	ref, ok := agents.Lookup(*ghost)
	if !ok {
		return fmt.Errorf("ghost %q is absent from agents config", *ghost)
	}
	fmt.Println(ref.AgentVersion())
	return nil
}
