package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const (
	agentsSchemaPath       = "../../agents.schema.json"
	agentsExamplePath      = "../../agents.example.yaml"
	chartValuesPath        = "../../chart/values.yaml"
	integrationFixturePath = "../../test/integration/platform.yaml"
)

func TestAgentsSchemaFixtures(t *testing.T) {
	schema := compileAgentsSchema(t)

	example, err := os.ReadFile(agentsExamplePath)
	if err != nil {
		t.Fatalf("read agents example: %v", err)
	}
	chart, err := chartAgentsDocument()
	if err != nil {
		t.Fatalf("build chart agents document: %v", err)
	}
	integration, err := embeddedAgentsDocument(integrationFixturePath, "agent-config")
	if err != nil {
		t.Fatalf("read integration agents document: %v", err)
	}

	for name, document := range map[string][]byte{
		"agents.example.yaml":                      example,
		"chart/templates/configmap.yaml":           chart,
		"test/integration/platform.yaml ConfigMap": integration,
	} {
		t.Run(name, func(t *testing.T) {
			instance := yamlInstance(t, document)
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("validate against agents.schema.json: %v", err)
			}
		})
	}
}

func TestAgentsSchemaRejectsUnknownMajor(t *testing.T) {
	instance := yamlInstance(t, []byte("schemaVersion: 99\nagents:\n  agent-x: {namespace: kagent, name: x}\n"))
	if err := compileAgentsSchema(t).Validate(instance); err == nil {
		t.Fatal("schema accepted unknown schemaVersion 99")
	}
}

func compileAgentsSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := compiler.Compile(agentsSchemaPath)
	if err != nil {
		t.Fatalf("compile agents.schema.json: %v", err)
	}
	return schema
}

func yamlInstance(t *testing.T, document []byte) any {
	t.Helper()
	var value any
	if err := yaml.Unmarshal(document, &value); err != nil {
		t.Fatalf("parse YAML fixture: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("convert YAML fixture to JSON: %v", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("decode JSON fixture: %v", err)
	}
	return instance
}

func chartAgentsDocument() ([]byte, error) {
	data, err := os.ReadFile(chartValuesPath)
	if err != nil {
		return nil, err
	}
	var values struct {
		BridgedOrigins any            `yaml:"bridgedOrigins"`
		Agents         map[string]any `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return yaml.Marshal(map[string]any{
		"schemaVersion":  agentsSchemaVersion,
		"bridgedOrigins": values.BridgedOrigins,
		"agents":         values.Agents,
	})
}

func embeddedAgentsDocument(path, configMapName string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var document struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if document.Kind == "ConfigMap" && document.Metadata.Name == configMapName {
			agents, ok := document.Data["agents.yaml"]
			if !ok {
				return nil, errors.New("ConfigMap does not contain data.agents.yaml")
			}
			return []byte(agents), nil
		}
	}
	return nil, errors.New("agents ConfigMap not found")
}
