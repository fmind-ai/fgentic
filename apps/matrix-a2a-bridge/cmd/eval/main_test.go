package main

import (
	"strings"
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

func TestResolveGovernedModelRequiresExactChatEntry(t *testing.T) {
	catalog, err := modelcatalog.Decode(strings.NewReader(`
schemaVersion: 1
models:
  - profile: vertex
    genAiSystem: gcp.vertex_ai
    model: chat-model
    residency: global
    allowedClassification: public
    capabilities: [chat]
  - profile: vllm
    genAiSystem: openai
    model: embedding-model
    residency: self-hosted
    allowedClassification: regulated
    capabilities: [embeddings]
`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	model, err := resolveGovernedModel(catalog, "vertex", "chat-model")
	if err != nil {
		t.Fatalf("resolveGovernedModel: %v", err)
	}
	if model.GenAISystem != "gcp.vertex_ai" {
		t.Fatalf("GenAISystem = %q", model.GenAISystem)
	}
	if _, err := resolveGovernedModel(catalog, "vertex", "missing"); err == nil {
		t.Fatal("resolveGovernedModel unexpectedly accepted missing identity")
	}
	if _, err := resolveGovernedModel(catalog, "vllm", "embedding-model"); err == nil {
		t.Fatal("resolveGovernedModel unexpectedly accepted model without chat capability")
	}
}
