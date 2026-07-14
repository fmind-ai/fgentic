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

func TestAgentsSchemaStage(t *testing.T) {
	schema := compileAgentsSchema(t)
	for _, doc := range [][]byte{
		[]byte("schemaVersion: 1\nagents:\n  agent-local: {namespace: kagent, name: k8s, stage: dev}\n"),
		[]byte("schemaVersion: 1\nagents:\n  agent-local: {namespace: kagent, name: k8s, stage: prod}\n"),
	} {
		if err := schema.Validate(yamlInstance(t, doc)); err != nil {
			t.Fatalf("valid stage rejected: %v", err)
		}
	}
	invalid := []byte("schemaVersion: 1\nagents:\n  agent-local: {namespace: kagent, name: k8s, stage: staging}\n")
	if err := schema.Validate(yamlInstance(t, invalid)); err == nil {
		t.Fatal("schema accepted invalid stage enum")
	}
}

func TestAgentsSchemaExtensions(t *testing.T) {
	schema := compileAgentsSchema(t)
	key := "publicKey: {kty: EC, crv: P-256, " +
		"x: axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY, y: T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU}"
	remote := func(extensions string) []byte {
		return []byte("schemaVersion: 1\nagents:\n  agent-remote:\n    url: https://partner.example/a2a\n" +
			"    timeout: 12s\n    tokenBudget: 8192\n" + extensions +
			"    cardIdentity:\n      name: Partner\n      organization: Partner Corp\n      keyID: k1\n      " + key + "\n")
	}
	valid := remote("    extensions: [https://fgentic.fmind.ai/a2a/extensions/skill-quote/v1]\n    maxCost: 25\n    allowMedia: true\n    mtls:\n      clientCertFile: /etc/mtls/client.crt\n      clientKeyFile: /etc/mtls/client.key\n")
	if err := schema.Validate(yamlInstance(t, valid)); err != nil {
		t.Fatalf("valid remote+extensions+maxCost+allowMedia+mtls rejected: %v", err)
	}
	rejects := map[string][]byte{
		"non-https extension": remote("    extensions: [http://partner.example/ext]\n"),
		"extensions on local target": []byte("schemaVersion: 1\nagents:\n  agent-k8s:\n    namespace: kagent\n" +
			"    name: k8s\n    extensions: [https://fgentic.fmind.ai/a2a/extensions/skill-quote/v1]\n"),
		"zero maxCost": remote("    maxCost: 0\n"),
		"maxCost on local target": []byte("schemaVersion: 1\nagents:\n  agent-k8s:\n    namespace: kagent\n" +
			"    name: k8s\n    maxCost: 25\n"),
		"allowMedia on local target": []byte("schemaVersion: 1\nagents:\n  agent-k8s:\n    namespace: kagent\n" +
			"    name: k8s\n    allowMedia: true\n"),
		"mtls on local target": []byte("schemaVersion: 1\nagents:\n  agent-k8s:\n    namespace: kagent\n" +
			"    name: k8s\n    mtls:\n      clientCertFile: /c\n      clientKeyFile: /k\n"),
		"mtls missing clientKeyFile": remote("    mtls:\n      clientCertFile: /c\n"),
	}
	for name, document := range rejects {
		t.Run(name, func(t *testing.T) {
			if err := schema.Validate(yamlInstance(t, document)); err == nil {
				t.Fatal("schema accepted invalid extensions config")
			}
		})
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
