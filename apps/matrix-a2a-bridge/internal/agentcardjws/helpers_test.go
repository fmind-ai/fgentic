package agentcardjws

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func testPrivateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

func testCardJSON(t *testing.T) []byte {
	t.Helper()
	card := &a2a.AgentCard{
		Name:        "Signed card fixture",
		Description: "Exercises the shared AgentCard JWS contract",
		Provider: &a2a.AgentProvider{
			Org: "Fgentic tests",
			URL: "https://fgentic.example",
		},
		Version: "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface("https://agents.example/a2a/docs", a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       a2a.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID:          "docs",
			Name:        "Answer documentation questions",
			Description: "Answers one bounded documentation prompt",
			Tags:        []string{"documentation"},
		}},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("Marshal AgentCard: %v", err)
	}
	return raw
}

func decodeTestObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("Decode object: %v", err)
	}
	return object
}

func encodeTestObject(t *testing.T, object map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("Marshal object: %v", err)
	}
	return raw
}
