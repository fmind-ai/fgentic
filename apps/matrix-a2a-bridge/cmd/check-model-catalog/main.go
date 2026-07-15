package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

const (
	catalogRelativePath = "infra/agentgateway/providers/model-catalog.yaml"
	schemaRelativePath  = "infra/agentgateway/providers/model-catalog.schema.json"
)

type platformSettings struct {
	Data struct {
		LLMProvider string `yaml:"llm_provider"`
		LLMModel    string `yaml:"llm_model"`
	} `yaml:"data"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("model catalog validation failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("check-model-catalog", flag.ContinueOnError)
	repoRoot := flags.String("repo-root", "", "repository root (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *repoRoot == "" {
		return fmt.Errorf("--repo-root is required")
	}

	catalog, err := loadCatalog(filepath.Join(*repoRoot, catalogRelativePath))
	if err != nil {
		return err
	}
	if err := validateSchemaDocument(
		filepath.Join(*repoRoot, schemaRelativePath),
		filepath.Join(*repoRoot, catalogRelativePath),
	); err != nil {
		return err
	}
	if err := validateProfileDirectories(*repoRoot, catalog); err != nil {
		return err
	}
	settings, err := filepath.Glob(filepath.Join(*repoRoot, "clusters", "*", "platform-settings.yaml"))
	if err != nil {
		return fmt.Errorf("glob platform settings: %w", err)
	}
	if len(settings) == 0 {
		return fmt.Errorf("no tracked platform-settings.yaml files found")
	}
	for _, path := range settings {
		if err := validatePlatformSettings(path, catalog); err != nil {
			return err
		}
	}
	fmt.Printf("model catalog valid: %d models, %d active overlays\n", len(catalog.Models), len(settings))
	return nil
}

func loadCatalog(path string) (*modelcatalog.Catalog, error) {
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

func validateSchemaDocument(schemaPath, catalogPath string) error {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("compile model catalog schema: %w", err)
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		return fmt.Errorf("read model catalog for schema validation: %w", err)
	}
	var yamlValue any
	if err := yaml.Unmarshal(data, &yamlValue); err != nil {
		return fmt.Errorf("decode model catalog for schema validation: %w", err)
	}
	encoded, err := json.Marshal(yamlValue)
	if err != nil {
		return fmt.Errorf("convert model catalog to JSON: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("decode model catalog JSON instance: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("validate model catalog schema: %w", err)
	}
	return nil
}

func validateProfileDirectories(repoRoot string, catalog *modelcatalog.Catalog) error {
	seen := make(map[string]struct{}, len(catalog.Models))
	for _, model := range catalog.Models {
		if _, checked := seen[model.Profile]; checked {
			continue
		}
		path := filepath.Join(repoRoot, "infra", "agentgateway", "providers", "profiles", model.Profile)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("catalog profile %s has no provider directory: %w", model.Profile, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("catalog profile path %s is not a directory", path)
		}
		seen[model.Profile] = struct{}{}
	}
	return nil
}

func validatePlatformSettings(path string, catalog *modelcatalog.Catalog) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var settings platformSettings
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	provider := settings.Data.LLMProvider
	model := settings.Data.LLMModel
	if provider == "" || model == "" {
		return fmt.Errorf("%s must declare data.llm_provider and data.llm_model", path)
	}
	if _, err := catalog.ResolveProfile(provider, model); err != nil {
		return fmt.Errorf("validate %s: %w", path, err)
	}
	return nil
}
