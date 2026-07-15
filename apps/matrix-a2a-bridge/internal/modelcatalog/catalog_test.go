package modelcatalog

import (
	"strings"
	"testing"
)

const validCatalog = `
schemaVersion: 1
models:
  - profile: vertex
    genAiSystem: gcp.vertex_ai
    model: google/gemini-2.5-flash
    residency: global
    allowedClassification: public
    capabilities: [chat]
    costRef: fgentic.eval.pricing.v1
`

func TestDecodeAndResolve(t *testing.T) {
	catalog, err := Decode(strings.NewReader(validCatalog))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	model, err := catalog.ResolveProfile("vertex", "google/gemini-2.5-flash")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if model.GenAISystem != "gcp.vertex_ai" || !model.Supports(CapabilityChat) || model.Supports(CapabilityEmbeddings) {
		t.Fatalf("resolved model = %#v", model)
	}
}

func TestDecodeRejectsUnknownMissingAndDuplicatePolicy(t *testing.T) {
	tests := map[string]string{
		"unknown provider model": strings.Replace(validCatalog, "profile: vertex", "profile: unknown", 1),
		"missing classification": strings.Replace(validCatalog, "    allowedClassification: public\n", "", 1),
		"unknown classification": strings.Replace(validCatalog, "allowedClassification: public", "allowedClassification: confidential", 1),
		"duplicate identity": strings.Replace(validCatalog, "    costRef: fgentic.eval.pricing.v1", `    costRef: fgentic.eval.pricing.v1
  - profile: duplicate
    genAiSystem: gcp.vertex_ai
    model: google/gemini-2.5-flash
    residency: global
    allowedClassification: public
    capabilities: [chat]`, 1),
		"unknown field": strings.Replace(validCatalog, "    residency: global", "    residency: global\n    mutablePrice: 1", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			catalog, err := Decode(strings.NewReader(input))
			if name == "unknown provider model" {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				_, err = catalog.ResolveProfile("vertex", "google/gemini-2.5-flash")
			}
			if err == nil {
				t.Fatal("unexpectedly accepted invalid catalog")
			}
		})
	}
}

func TestDecodeRejectsMultipleDocuments(t *testing.T) {
	if _, err := Decode(strings.NewReader(validCatalog + "---\n{}\n")); err == nil {
		t.Fatal("Decode unexpectedly accepted multiple documents")
	}
}

func TestModelAdmitsClassificationCeiling(t *testing.T) {
	model := Model{AllowedClassification: ClassificationRestricted}
	for _, classification := range []Classification{
		ClassificationPublic,
		ClassificationApprovedNonPublic,
		ClassificationRestricted,
	} {
		if !model.Admits(classification) {
			t.Errorf("restricted model rejected %q", classification)
		}
	}
	if model.Admits(ClassificationRegulated) {
		t.Error("restricted model admitted regulated data")
	}
	if model.Admits(Classification("unknown")) {
		t.Error("restricted model admitted an unknown classification")
	}
}

func TestParseClassificationRejectsUnknown(t *testing.T) {
	if _, err := ParseClassification("confidential"); err == nil {
		t.Fatal("ParseClassification accepted an unknown value")
	}
}
