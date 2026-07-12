// Package agentcardjws implements the A2A v1.0 Signed AgentCard payload contract.
// It keeps raw JSON presence information intact while sharing one RFC 8785
// canonicalization path between card signers and verifiers.
package agentcardjws

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/gowebpki/jcs"
)

// Document is a validated AgentCard JSON document and its canonical unsigned payload.
// The original generic JSON representation is retained so signing preserves unknown fields
// and explicit proto-optional defaults that a typed round trip would discard.
type Document struct {
	raw               []byte
	wire              map[string]any
	payload           []byte
	signatures        []a2a.AgentCardSignature
	hasSignatureField bool
}

// Parse validates an AgentCard JSON document and constructs the RFC 8785 payload covered by
// Signed AgentCard JWS signatures. Both unsigned inputs for signing and signed inputs for
// verification are accepted.
func Parse(raw []byte) (*Document, error) {
	// Decode before invoking the recursive JCS implementation so encoding/json's bounded
	// nesting check rejects adversarially deep input. JCS then catches duplicate object keys
	// before a typed decoder and signature verifier can observe a different document.
	wire, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if _, err := jcs.Transform(raw); err != nil {
		return nil, fmt.Errorf("card is not valid canonicalizable I-JSON")
	}
	if err := validateRequiredCardJSON(wire); err != nil {
		return nil, err
	}

	rawSignatures, hasSignatureField := wire["signatures"]
	var signatures []a2a.AgentCardSignature
	if hasSignatureField {
		signatureJSON, err := json.Marshal(rawSignatures)
		if err != nil {
			return nil, fmt.Errorf("encode card signatures: %w", err)
		}
		if err := json.Unmarshal(signatureJSON, &signatures); err != nil {
			return nil, fmt.Errorf("card signatures do not match the A2A schema")
		}
	}

	payloadDocument, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	delete(payloadDocument, "signatures")
	normalizeCardDefaults(payloadDocument)
	unsignedJSON, err := json.Marshal(payloadDocument)
	if err != nil {
		return nil, fmt.Errorf("encode unsigned card: %w", err)
	}
	payload, err := jcs.Transform(unsignedJSON)
	if err != nil {
		return nil, fmt.Errorf("canonicalize unsigned card: %w", err)
	}

	return &Document{
		raw:               slices.Clone(raw),
		wire:              wire,
		payload:           payload,
		signatures:        signatures,
		hasSignatureField: hasSignatureField,
	}, nil
}

// Card decodes the document into the official A2A v1.0 AgentCard type. Unknown signed fields
// remain covered by Payload even though the SDK intentionally ignores them here.
func (d *Document) Card() (*a2a.AgentCard, error) {
	decoder := json.NewDecoder(bytes.NewReader(d.raw))
	var card a2a.AgentCard
	if err := decoder.Decode(&card); err != nil {
		return nil, fmt.Errorf("card JSON does not match the A2A schema")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("card has trailing JSON data")
	}
	return &card, nil
}

// Payload returns a copy of the canonical unsigned payload covered by the JWS.
func (d *Document) Payload() []byte {
	return slices.Clone(d.payload)
}

// Signatures returns a copy of the signatures and reports whether the JSON property was present.
// Presence is distinct from length so signers can reject even an explicitly empty stale field.
func (d *Document) Signatures() ([]a2a.AgentCardSignature, bool) {
	return slices.Clone(d.signatures), d.hasSignatureField
}

func (d *Document) marshalWithSignatures(signatures []a2a.AgentCardSignature) ([]byte, error) {
	documentJSON, err := json.Marshal(d.wire)
	if err != nil {
		return nil, fmt.Errorf("clone card document: %w", err)
	}
	document, err := decodeObject(documentJSON)
	if err != nil {
		return nil, fmt.Errorf("clone card document: %w", err)
	}
	document["signatures"] = signatures
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode signed card: %w", err)
	}
	return encoded, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("card is not valid JSON")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("card has trailing JSON data")
	}
	if document == nil {
		return nil, fmt.Errorf("card is not valid JSON")
	}
	return document, nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func validateRequiredCardJSON(document map[string]any) error {
	for _, field := range []string{
		"name",
		"description",
		"supportedInterfaces",
		"version",
		"capabilities",
		"defaultInputModes",
		"defaultOutputModes",
		"skills",
	} {
		if _, exists := document[field]; !exists {
			return fmt.Errorf("card is missing a required A2A field")
		}
	}
	provider, ok := document["provider"].(map[string]any)
	if !ok {
		return fmt.Errorf("card provider is missing")
	}
	if _, exists := provider["url"]; !exists {
		return fmt.Errorf("card provider is missing a required A2A field")
	}
	if _, exists := provider["organization"]; !exists {
		return fmt.Errorf("card provider is missing a required A2A field")
	}

	interfaces, ok := document["supportedInterfaces"].([]any)
	if !ok || len(interfaces) == 0 {
		return fmt.Errorf("card has no supported interfaces")
	}
	for _, value := range interfaces {
		agentInterface, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("card contains an invalid interface")
		}
		for _, field := range []string{"url", "protocolBinding", "protocolVersion"} {
			if _, exists := agentInterface[field]; !exists {
				return fmt.Errorf("card interface is missing a required A2A field")
			}
		}
	}

	for _, field := range []string{"defaultInputModes", "defaultOutputModes"} {
		values, ok := document[field].([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("card must advertise default input and output modes")
		}
	}
	skills, ok := document["skills"].([]any)
	if !ok || len(skills) == 0 {
		return fmt.Errorf("card must advertise at least one skill")
	}
	for _, value := range skills {
		skill, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("card contains an invalid skill")
		}
		for _, field := range []string{"id", "name", "description", "tags"} {
			if _, exists := skill[field]; !exists {
				return fmt.Errorf("card skill is missing a required A2A field")
			}
		}
		tags, ok := skill["tags"].([]any)
		if !ok || len(tags) == 0 {
			return fmt.Errorf("card skill must advertise at least one tag")
		}
	}
	return nil
}

// normalizeCardDefaults applies the protobuf presence rules required by A2A v1.0 before JCS.
// Values explicitly present for proto optional fields and all REQUIRED fields are retained;
// defaults for ordinary scalar, repeated, and map fields are removed.
func normalizeCardDefaults(document map[string]any) {
	cleanJSONObject(document)
}

func cleanJSONObject(document map[string]any) {
	deleteEmptyField(document, "securitySchemes")
	deleteEmptyField(document, "securityRequirements")
	if securitySchemes, ok := document["securitySchemes"].(map[string]any); ok {
		normalizeSecuritySchemes(securitySchemes)
	}
	if securityRequirements, ok := document["securityRequirements"].([]any); ok {
		normalizeSecurityRequirements(securityRequirements)
	}

	capabilities, _ := document["capabilities"].(map[string]any)
	if capabilities != nil {
		deleteEmptyField(capabilities, "extensions")
		if extensions, ok := capabilities["extensions"].([]any); ok {
			for _, value := range extensions {
				extension, _ := value.(map[string]any)
				if extension == nil {
					continue
				}
				deleteDefaultScalar(extension, "uri")
				deleteDefaultScalar(extension, "description")
				deleteDefaultScalar(extension, "required")
				// Params is a protobuf Struct: a nonempty subtree is extension-owned data,
				// so nested false/empty values must never be interpreted as proto defaults.
				deleteEmptyField(extension, "params")
			}
		}
	}

	if interfaces, ok := document["supportedInterfaces"].([]any); ok {
		for _, value := range interfaces {
			agentInterface, _ := value.(map[string]any)
			if agentInterface != nil {
				deleteDefaultScalar(agentInterface, "tenant")
			}
		}
	}

	if skills, ok := document["skills"].([]any); ok {
		for _, value := range skills {
			skill, _ := value.(map[string]any)
			if skill == nil {
				continue
			}
			deleteEmptyField(skill, "examples")
			deleteEmptyField(skill, "inputModes")
			deleteEmptyField(skill, "outputModes")
			deleteEmptyField(skill, "securityRequirements")
			if securityRequirements, ok := skill["securityRequirements"].([]any); ok {
				normalizeSecurityRequirements(securityRequirements)
			}
		}
	}
}

func normalizeSecurityRequirements(requirements []any) {
	for _, value := range requirements {
		requirement, _ := value.(map[string]any)
		if requirement == nil {
			continue
		}
		if schemes, ok := requirement["schemes"].(map[string]any); ok {
			for _, scopesValue := range schemes {
				if scopes, ok := scopesValue.(map[string]any); ok {
					deleteEmptyField(scopes, "list")
				}
			}
		}
		deleteEmptyField(requirement, "schemes")
	}
}

func normalizeSecuritySchemes(schemes map[string]any) {
	for _, value := range schemes {
		wrapper, _ := value.(map[string]any)
		if wrapper == nil {
			continue
		}
		for schemeType, schemeValue := range wrapper {
			scheme, _ := schemeValue.(map[string]any)
			if scheme == nil {
				continue
			}
			switch schemeType {
			case "apiKeySecurityScheme":
				deleteDefaultScalar(scheme, "description")
			case "httpAuthSecurityScheme":
				deleteDefaultScalar(scheme, "description")
				deleteDefaultScalar(scheme, "bearerFormat")
			case "openIdConnectSecurityScheme", "mtlsSecurityScheme":
				deleteDefaultScalar(scheme, "description")
			case "oauth2SecurityScheme":
				normalizeOAuth2SecurityScheme(scheme)
			}
		}
	}
}

func normalizeOAuth2SecurityScheme(scheme map[string]any) {
	deleteDefaultScalar(scheme, "description")
	deleteDefaultScalar(scheme, "oauth2MetadataUrl")
	flows, _ := scheme["flows"].(map[string]any)
	for flowType, flowValue := range flows {
		flow, _ := flowValue.(map[string]any)
		if flow == nil {
			continue
		}
		deleteDefaultScalar(flow, "refreshUrl")
		switch flowType {
		case "authorizationCode":
			deleteDefaultScalar(flow, "pkceRequired")
		case "implicit":
			deleteDefaultScalar(flow, "authorizationUrl")
			deleteEmptyField(flow, "scopes")
		case "password":
			deleteDefaultScalar(flow, "tokenUrl")
			deleteEmptyField(flow, "scopes")
		}
	}
}

func deleteEmptyField(object map[string]any, key string) {
	value, exists := object[key]
	if !exists {
		return
	}
	switch typed := value.(type) {
	case nil:
		delete(object, key)
	case []any:
		if len(typed) == 0 {
			delete(object, key)
		}
	case map[string]any:
		if len(typed) == 0 {
			delete(object, key)
		}
	}
}

func deleteDefaultScalar(object map[string]any, key string) {
	value, exists := object[key]
	if !exists {
		return
	}
	if value == "" || value == false || value == nil {
		delete(object, key)
	}
}
