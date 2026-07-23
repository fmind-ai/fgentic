package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

func TestValidatePlatformSettings(t *testing.T) {
	catalog, err := modelcatalog.Decode(strings.NewReader(`
schemaVersion: 1
models:
  - profile: vertex
    genAiSystem: gcp.vertex_ai
    model: google/gemini-2.5-flash
    residency: global
    allowedClassification: public
    capabilities: [chat]
`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	directory := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	valid := write("valid.yaml", "data:\n  llm_provider: vertex\n  llm_model: google/gemini-2.5-flash\n  model_allowed_classification: public\n")
	unknown := write("unknown.yaml", "data:\n  llm_provider: vertex\n  llm_model: unknown\n  model_allowed_classification: public\n")
	missingCeiling := write("missing-ceiling.yaml", "data:\n  llm_provider: vertex\n  llm_model: google/gemini-2.5-flash\n")
	wrongCeiling := write("wrong-ceiling.yaml", "data:\n  llm_provider: vertex\n  llm_model: google/gemini-2.5-flash\n  model_allowed_classification: regulated\n")
	badCeiling := write("bad-ceiling.yaml", "data:\n  llm_provider: vertex\n  llm_model: google/gemini-2.5-flash\n  model_allowed_classification: confidential\n")

	if err := validatePlatformSettings(valid, catalog); err != nil {
		t.Fatalf("validatePlatformSettings: %v", err)
	}
	for name, path := range map[string]string{
		"unknown model":   unknown,
		"missing ceiling": missingCeiling,
		"widened ceiling": wrongCeiling, // regulated > catalog public ceiling must be rejected
		"unknown ceiling": badCeiling,
	} {
		if err := validatePlatformSettings(path, catalog); err == nil {
			t.Fatalf("validatePlatformSettings unexpectedly accepted %s", name)
		}
	}
}
