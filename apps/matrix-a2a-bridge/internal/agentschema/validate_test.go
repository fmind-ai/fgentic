package agentschema

import (
	"strings"
	"testing"
)

const testSchemaPath = "../../agents.schema.json"

func TestValidate(t *testing.T) {
	document := `schemaVersion: 1
agents:
  agent-demo-helper:
    namespace: kagent
    name: demo-helper
    description: Development scaffold.
    stage: dev
    allowedSenders: ["@alice:fgentic.fmind.ai"]
`
	if err := Validate(testSchemaPath, strings.NewReader(document)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsUnsafeScaffold(t *testing.T) {
	document := `schemaVersion: 1
agents:
  agent-demo-helper:
    namespace: kagent
    name: demo-helper
    stage: staging
    credential: plaintext
`
	err := Validate(testSchemaPath, strings.NewReader(document))
	if err == nil {
		t.Fatal("Validate accepted unknown fields and an invalid stage")
	}
}

func TestValidateRejectsMultipleDocuments(t *testing.T) {
	document := "agents:\n  agent-a: {namespace: kagent, name: a}\n---\nagents:\n  agent-b: {namespace: kagent, name: b}\n"
	err := Validate(testSchemaPath, strings.NewReader(document))
	if err == nil || !strings.Contains(err.Error(), "multiple documents") {
		t.Fatalf("Validate error = %v, want multiple documents", err)
	}
}
