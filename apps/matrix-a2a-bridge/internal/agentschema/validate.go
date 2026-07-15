// Package agentschema validates the bridge's versioned agent-routing document.
package agentschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// Validate compiles the authoritative JSON schema and validates one YAML document against it.
func Validate(schemaPath string, input io.Reader) error {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("compile agents schema: %w", err)
	}

	decoder := yaml.NewDecoder(input)
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode agents YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode agents YAML: multiple documents")
		}
		return fmt.Errorf("decode agents YAML trailer: %w", err)
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("convert agents YAML to JSON: %w", err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		return fmt.Errorf("normalize agents document: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("validate agents document: %w", err)
	}
	return nil
}
