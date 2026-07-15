package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	if err := validateProfileAdmission(*repoRoot, catalog); err != nil {
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

const modelAdmissionAnnotation = "fgentic.dev/model-admission"

func validateProfileAdmission(repoRoot string, catalog *modelcatalog.Catalog) error {
	modelsByProfile := make(map[string][]modelcatalog.Model)
	for _, model := range catalog.Models {
		modelsByProfile[model.Profile] = append(modelsByProfile[model.Profile], model)
	}
	backends, err := filepath.Glob(filepath.Join(
		repoRoot, "infra", "agentgateway", "providers", "profiles", "*", "llm-backend.yaml",
	))
	if err != nil {
		return fmt.Errorf("glob provider backends: %w", err)
	}
	if len(backends) == 0 {
		return fmt.Errorf("no provider backends found")
	}
	for _, path := range backends {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read provider backend %s: %w", path, err)
		}
		var backend struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(data, &backend); err != nil {
			return fmt.Errorf("decode provider backend %s: %w", path, err)
		}
		profile := filepath.Base(filepath.Dir(path))
		got := backend.Metadata.Annotations[modelAdmissionAnnotation]
		want := modelAdmissionExpression(modelsByProfile[profile])
		if got != want {
			return fmt.Errorf(
				"provider profile %s model admission drifted from the governed catalog: got %q, want %q",
				profile,
				got,
				want,
			)
		}
	}
	return nil
}

func modelAdmissionExpression(models []modelcatalog.Model) string {
	if len(models) == 0 {
		return `request.method == "GET"`
	}
	clauses := make([]string, 0, len(models))
	for _, model := range models {
		allowed := make([]string, 0, 4)
		for _, classification := range []modelcatalog.Classification{
			modelcatalog.ClassificationPublic,
			modelcatalog.ClassificationApprovedNonPublic,
			modelcatalog.ClassificationRestricted,
			modelcatalog.ClassificationRegulated,
		} {
			if model.Admits(classification) {
				allowed = append(allowed, strconv.Quote(string(classification)))
			}
		}
		clauses = append(clauses, fmt.Sprintf(
			`"${llm_model}" == %s && request.headers["x-fgentic-data-classification"] in [%s]`,
			strconv.Quote(model.Name),
			strings.Join(allowed, ", "),
		))
	}
	return `request.method == "GET" || (request.method == "POST" && ` +
		`"x-fgentic-data-classification" in request.headers && (` +
		strings.Join(clauses, " || ") + `))`
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
