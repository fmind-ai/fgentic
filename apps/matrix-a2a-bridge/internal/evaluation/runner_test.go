package evaluation

import (
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

func TestValidateObservedModelUsesCatalogIdentity(t *testing.T) {
	expected := modelcatalog.Model{GenAISystem: "gcp.vertex_ai", Name: "google/gemini-2.5-flash"}
	if err := validateObservedModel(expected, ProviderIdentity{
		System: "gcp.vertex_ai", RequestModel: "google/gemini-2.5-flash",
	}); err != nil {
		t.Fatalf("validateObservedModel: %v", err)
	}
	for name, identity := range map[string]ProviderIdentity{
		"system": {System: "openai", RequestModel: "google/gemini-2.5-flash"},
		"model":  {System: "gcp.vertex_ai", RequestModel: "other", ResponseModel: "other-version"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateObservedModel(expected, identity); err == nil {
				t.Fatal("validateObservedModel unexpectedly accepted mismatched identity")
			}
		})
	}
}
