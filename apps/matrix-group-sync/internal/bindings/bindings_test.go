package bindings

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bindings.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	path := write(t, `schemaVersion: 1
bindings:
  - group: /fgentic/agent-access/platform
    roomAlias: "#agent-platform:fgentic.localhost"
    agents: [agent-k8s, agent-helm]
  - group: /fgentic/agent-access/docs
    roomAlias: "#agent-docs:fgentic.localhost"
    agents: [agent-docs-qa]
`)
	set, err := Load(path, "agent-")
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Groups(); len(got) != 2 || got[0] != "/fgentic/agent-access/docs" {
		t.Fatalf("expected sorted groups, got %v", got)
	}
	if len(set.All()) != 2 {
		t.Fatalf("expected 2 bindings")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"bad schema": `schemaVersion: 2
bindings:
  - group: /a
    roomAlias: "#a:s"
    agents: [agent-x]`,
		"empty": `schemaVersion: 1
bindings: []`,
		"relative group": `schemaVersion: 1
bindings:
  - group: fgentic/platform
    roomAlias: "#a:s"
    agents: [agent-x]`,
		"empty segment": `schemaVersion: 1
bindings:
  - group: /fgentic//platform
    roomAlias: "#a:s"
    agents: [agent-x]`,
		"alias without server": `schemaVersion: 1
bindings:
  - group: /a
    roomAlias: "#a"
    agents: [agent-x]`,
		"no agents": `schemaVersion: 1
bindings:
  - group: /a
    roomAlias: "#a:s"
    agents: []`,
		"bad agent prefix": `schemaVersion: 1
bindings:
  - group: /a
    roomAlias: "#a:s"
    agents: [k8s]`,
		"duplicate group": `schemaVersion: 1
bindings:
  - group: /a
    roomAlias: "#a:s"
    agents: [agent-x]
  - group: /a
    roomAlias: "#b:s"
    agents: [agent-y]`,
		"duplicate room": `schemaVersion: 1
bindings:
  - group: /a
    roomAlias: "#a:s"
    agents: [agent-x]
  - group: /b
    roomAlias: "#a:s"
    agents: [agent-y]`,
		"unknown field": `schemaVersion: 1
bindings:
  - group: /a
    roomAlias: "#a:s"
    agents: [agent-x]
    extra: nope`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, content), "agent-"); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), "agent-"); err == nil {
		t.Fatal("missing file must error")
	}
}
