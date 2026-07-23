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

func TestParseClassificationRejectsUnknown(t *testing.T) {
	if _, err := ParseClassification("confidential"); err == nil {
		t.Fatal("ParseClassification accepted an unknown value")
	}
}

func TestClassificationOrMostRestrictiveFailsClosed(t *testing.T) {
	tests := map[string]Classification{
		"public":              ClassificationPublic,
		"regulated":           ClassificationRegulated,
		"":                    ClassificationRegulated, // missing signal -> most restrictive
		"confidential":        ClassificationRegulated, // unknown signal -> most restrictive
		"REGULATED":           ClassificationRegulated, // wrong case is not the closed enum
		"approved_non_public": ClassificationApprovedNonPublic,
	}
	for value, want := range tests {
		if got := ClassificationOrMostRestrictive(value); got != want {
			t.Errorf("ClassificationOrMostRestrictive(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestClassificationRankIsTotalAndFailClosed(t *testing.T) {
	if ClassificationPublic.Rank() >= ClassificationApprovedNonPublic.Rank() ||
		ClassificationApprovedNonPublic.Rank() >= ClassificationRestricted.Rank() ||
		ClassificationRestricted.Rank() >= ClassificationRegulated.Rank() {
		t.Fatal("classification rank is not strictly increasing in sensitivity")
	}
	// An out-of-enum value must rank as most sensitive so drift denies rather than leaks.
	if Classification("confidential").Rank() != ClassificationRegulated.Rank() {
		t.Fatal("unknown classification did not rank as most restrictive")
	}
}

func TestModelAdmitsIsFailClosedResidencyDecision(t *testing.T) {
	sovereign := Model{Profile: "vllm", Residency: ResidencySelfHosted, AllowedClassification: ClassificationRegulated}
	hyperscaler := Model{Profile: "vertex", Residency: ResidencyGlobal, AllowedClassification: ClassificationPublic}

	// Sovereign backend serves classified content; hyperscaler is denied before egress.
	if !sovereign.Admits(ClassificationRegulated) {
		t.Error("sovereign backend must admit regulated content")
	}
	if hyperscaler.Admits(ClassificationRegulated) {
		t.Error("hyperscaler must be denied for regulated content")
	}
	// Default public flow is unchanged on the hyperscaler.
	if !hyperscaler.Admits(ClassificationPublic) {
		t.Error("hyperscaler must still admit public content")
	}
	// A missing/unknown signal collapses to regulated and is denied on a public-only backend.
	if hyperscaler.Admits(ClassificationOrMostRestrictive("")) {
		t.Error("missing classification must fail closed to denied on a public-only backend")
	}
	if !sovereign.Admits(ClassificationOrMostRestrictive("garbage")) {
		t.Error("unknown classification must still be servable by a regulated sovereign backend")
	}
}
