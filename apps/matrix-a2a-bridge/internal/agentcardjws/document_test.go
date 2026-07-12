package agentcardjws

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentPresenceNormalizationAndWirePreservation(t *testing.T) {
	wire := decodeTestObject(t, testCardJSON(t))
	wire["documentationUrl"] = ""
	wire["iconUrl"] = ""
	wire["futureSignedField"] = map[string]any{"enabled": false, "values": []any{}}
	wire["securityRequirements"] = nil
	capabilities := wire["capabilities"].(map[string]any)
	capabilities["streaming"] = false
	interfaces := wire["supportedInterfaces"].([]any)
	interfaces[0].(map[string]any)["tenant"] = ""
	wire["securitySchemes"] = map[string]any{
		"oauth": map[string]any{
			"oauth2SecurityScheme": map[string]any{
				"description":       "",
				"oauth2MetadataUrl": "",
				"flows": map[string]any{
					"authorizationCode": map[string]any{
						"authorizationUrl": "https://identity.example/authorize",
						"tokenUrl":         "https://identity.example/token",
						"refreshUrl":       "",
						"scopes":           map[string]any{},
						"pkceRequired":     false,
					},
				},
			},
		},
	}
	raw := encodeTestObject(t, wire)
	document, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, present := document.Signatures(); present {
		t.Fatal("unsigned document reported a signatures field")
	}
	if _, err := document.Card(); err != nil {
		t.Fatalf("Card: %v", err)
	}

	payload := decodeTestObject(t, document.Payload())
	if payload["documentationUrl"] != "" || payload["iconUrl"] != "" {
		t.Fatalf("proto-optional defaults were removed: %#v", payload)
	}
	payloadCapabilities := payload["capabilities"].(map[string]any)
	if streaming, ok := payloadCapabilities["streaming"].(bool); !ok || streaming {
		t.Fatalf("proto-optional streaming = %#v", payloadCapabilities["streaming"])
	}
	payloadInterface := payload["supportedInterfaces"].([]any)[0].(map[string]any)
	if _, exists := payloadInterface["tenant"]; exists {
		t.Fatal("ordinary scalar default tenant remained in canonical payload")
	}
	if _, exists := payload["securityRequirements"]; exists {
		t.Fatal("unset repeated securityRequirements remained in canonical payload")
	}
	if _, exists := payload["futureSignedField"]; !exists {
		t.Fatal("unknown signed field was removed from canonical payload")
	}
	oauth := payload["securitySchemes"].(map[string]any)["oauth"].(map[string]any)["oauth2SecurityScheme"].(map[string]any)
	if _, exists := oauth["description"]; exists {
		t.Fatal("ordinary OAuth description default remained in canonical payload")
	}
	flow := oauth["flows"].(map[string]any)["authorizationCode"].(map[string]any)
	if _, exists := flow["refreshUrl"]; exists {
		t.Fatal("ordinary OAuth refreshUrl default remained in canonical payload")
	}
	if scopes, ok := flow["scopes"].(map[string]any); !ok || len(scopes) != 0 {
		t.Fatalf("required OAuth scopes = %#v", flow["scopes"])
	}

	bundle, err := Sign(raw, testPrivateKey(t), "fixture-key")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	signedWire := decodeTestObject(t, bundle.AgentCard)
	signedInterface := signedWire["supportedInterfaces"].([]any)[0].(map[string]any)
	if tenant, exists := signedInterface["tenant"]; !exists || tenant != "" {
		t.Fatalf("wire tenant = %#v, present %v", tenant, exists)
	}
	if value, exists := signedWire["securityRequirements"]; !exists || value != nil {
		t.Fatalf("wire securityRequirements = %#v, present %v", value, exists)
	}
	if _, exists := signedWire["futureSignedField"]; !exists {
		t.Fatal("unknown field was removed from signed wire document")
	}
	signedDocument, err := Parse(bundle.AgentCard)
	if err != nil {
		t.Fatalf("Parse signed card: %v", err)
	}
	if !bytes.Equal(document.Payload(), signedDocument.Payload()) {
		t.Fatal("signing changed the canonical unsigned payload")
	}
}

func TestParseRejectsAmbiguousOrIncompleteJSON(t *testing.T) {
	base := testCardJSON(t)
	missing := decodeTestObject(t, base)
	delete(missing, "description")
	badSignatures := decodeTestObject(t, base)
	badSignatures["signatures"] = "not-an-array"
	duplicate := bytes.Replace(base, []byte(`"name":`), []byte(`"name":"duplicate","name":`), 1)

	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "malformed", raw: []byte(`{"name":`), want: "card is not valid JSON"},
		{name: "trailing", raw: append(append([]byte{}, base...), []byte(` {}`)...), want: "card has trailing JSON data"},
		{name: "duplicate", raw: duplicate, want: "card is not valid canonicalizable I-JSON"},
		{name: "missing required", raw: encodeTestObject(t, missing), want: "card is missing a required A2A field"},
		{name: "null", raw: []byte(`null`), want: "card is not valid JSON"},
		{name: "bad signatures", raw: encodeTestObject(t, badSignatures), want: "card signatures do not match the A2A schema"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestCardRejectsSchemaMismatchAfterRawValidation(t *testing.T) {
	document := decodeTestObject(t, testCardJSON(t))
	document["name"] = json.Number("42")
	parsed, err := Parse(encodeTestObject(t, document))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := parsed.Card(); err == nil || !strings.Contains(err.Error(), "A2A schema") {
		t.Fatalf("Card() error = %v", err)
	}
}
