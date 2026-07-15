package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentschema"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "agent mapping validation failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("validate-agents", flag.ContinueOnError)
	schemaPath := flags.String("schema", "agents.schema.json", "agents JSON schema path")
	configPath := flags.String("config", "", "agents YAML document path (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("--config is required")
	}
	config, err := os.Open(*configPath)
	if err != nil {
		return fmt.Errorf("open agents config: %w", err)
	}
	validationErr := agentschema.Validate(*schemaPath, config)
	closeErr := config.Close()
	if validationErr != nil {
		return validationErr
	}
	if closeErr != nil {
		return fmt.Errorf("close agents config: %w", closeErr)
	}
	return nil
}
