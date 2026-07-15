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
	valid := filepath.Join(directory, "valid.yaml")
	unknown := filepath.Join(directory, "unknown.yaml")
	if err := os.WriteFile(valid, []byte("data:\n  llm_provider: vertex\n  llm_model: google/gemini-2.5-flash\n"), 0o600); err != nil {
		t.Fatalf("write valid settings: %v", err)
	}
	if err := os.WriteFile(unknown, []byte("data:\n  llm_provider: vertex\n  llm_model: unknown\n"), 0o600); err != nil {
		t.Fatalf("write unknown settings: %v", err)
	}
	if err := validatePlatformSettings(valid, catalog); err != nil {
		t.Fatalf("validatePlatformSettings: %v", err)
	}
	if err := validatePlatformSettings(unknown, catalog); err == nil {
		t.Fatal("validatePlatformSettings unexpectedly accepted unknown model")
	}
}

func TestModelAdmissionExpression(t *testing.T) {
	models := []modelcatalog.Model{
		{Name: "public-model", AllowedClassification: modelcatalog.ClassificationPublic},
		{Name: "restricted-model", AllowedClassification: modelcatalog.ClassificationRestricted},
	}
	expression := modelAdmissionExpression(models)
	for _, want := range []string{
		`request.method == "GET"`,
		`"${llm_model}" == "public-model"`,
		`["public"]`,
		`"${llm_model}" == "restricted-model"`,
		`["public", "approved_non_public", "restricted"]`,
	} {
		if !strings.Contains(expression, want) {
			t.Errorf("modelAdmissionExpression() = %q, missing %q", expression, want)
		}
	}
	if got := modelAdmissionExpression(nil); got != `request.method == "GET"` {
		t.Errorf("uncataloged expression = %q", got)
	}
}
