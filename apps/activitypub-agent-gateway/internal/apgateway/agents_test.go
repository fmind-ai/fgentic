package apgateway

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAgents writes an agents.yaml fixture and returns its path.
func writeAgents(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

const validAgents = `schemaVersion: 1
agents:
  agent-docs-qa:
    namespace: kagent
    name: docs-qa
    description: Answers docs questions.
  agent-scribe:
    namespace: kagent
    name: scribe
    description: Summarizes discussion.
`

func TestLoadRegistry(t *testing.T) {
	reg, err := LoadRegistry(writeAgents(t, validAgents), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	ref, ok := reg.Lookup("agent-docs-qa")
	if !ok || ref.Namespace != "kagent" || ref.Name != "docs-qa" {
		t.Fatalf("Lookup docs-qa = %+v, %v", ref, ok)
	}
	if _, ok := reg.Lookup("agent-missing"); ok {
		t.Errorf("unexpected agent")
	}
	ghosts := reg.Ghosts()
	if len(ghosts) != 2 || ghosts[0] != "agent-docs-qa" || ghosts[1] != "agent-scribe" {
		t.Errorf("Ghosts = %v, want sorted", ghosts)
	}
}

func TestLoadRegistryRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field": "schemaVersion: 1\nagents:\n  agent-x:\n    namespace: k\n    name: x\n    description: d\n    bogus: 1\n",
		"bad schema":    "schemaVersion: 2\nagents:\n  agent-x:\n    namespace: k\n    name: x\n    description: d\n",
		"empty agents":  "schemaVersion: 1\nagents: {}\n",
		"no prefix":     "schemaVersion: 1\nagents:\n  docs-qa:\n    namespace: k\n    name: x\n    description: d\n",
		"bare prefix":   "schemaVersion: 1\nagents:\n  agent-:\n    namespace: k\n    name: x\n    description: d\n",
		"bad chars":     "schemaVersion: 1\nagents:\n  \"agent-a/b\":\n    namespace: k\n    name: x\n    description: d\n",
		"no namespace":  "schemaVersion: 1\nagents:\n  agent-x:\n    name: x\n    description: d\n",
		"no name":       "schemaVersion: 1\nagents:\n  agent-x:\n    namespace: k\n    description: d\n",
		"no desc":       "schemaVersion: 1\nagents:\n  agent-x:\n    namespace: k\n    name: x\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadRegistry(writeAgents(t, body), "agent-"); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestLoadRegistryMissingFile(t *testing.T) {
	if _, err := LoadRegistry("/no/such/agents.yaml", "agent-"); err == nil {
		t.Errorf("expected error for missing file")
	}
}
