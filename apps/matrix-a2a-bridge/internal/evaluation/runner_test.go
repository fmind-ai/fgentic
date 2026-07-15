package evaluation

import (
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
)

func TestValidateObservedModelUsesCatalogIdentity(t *testing.T) {
	expected := modelcatalog.Model{GenAISystem: "gcp.vertex_ai", Name: "google/gemini-2.5-flash"}
	for name, identity := range map[string]ProviderIdentity{
		"request": {System: "gcp.vertex_ai", RequestModel: "google/gemini-2.5-flash"},
		"response": {
			System: "gcp.vertex_ai", RequestModel: "unknown", ResponseModel: "google/gemini-2.5-flash",
		},
		"both": {
			System: "gcp.vertex_ai", RequestModel: "google/gemini-2.5-flash", ResponseModel: "google/gemini-2.5-flash",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateObservedModel(expected, identity); err != nil {
				t.Fatalf("validateObservedModel: %v", err)
			}
		})
	}
	for name, identity := range map[string]ProviderIdentity{
		"system":         {System: "openai", RequestModel: "google/gemini-2.5-flash"},
		"model":          {System: "gcp.vertex_ai", RequestModel: "other", ResponseModel: "other-version"},
		"response model": {System: "gcp.vertex_ai", RequestModel: "google/gemini-2.5-flash", ResponseModel: "other"},
		"request model":  {System: "gcp.vertex_ai", RequestModel: "other", ResponseModel: "google/gemini-2.5-flash"},
		"missing model":  {System: "gcp.vertex_ai", RequestModel: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateObservedModel(expected, identity); err == nil {
				t.Fatal("validateObservedModel unexpectedly accepted mismatched identity")
			}
		})
	}
}
