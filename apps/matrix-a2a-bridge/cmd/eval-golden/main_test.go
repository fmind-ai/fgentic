package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSuitesSelectsOnlyRequestedAgent(t *testing.T) {
	root := t.TempDir()
	writeGoldenFixture(t, root, "chosen", "chosen")
	writeGoldenFixture(t, root, "other", "wrong-name")

	suites, err := loadSuites(root, "chosen")
	if err != nil {
		t.Fatalf("loadSuites: %v", err)
	}
	if len(suites) != 1 || suites[0].Agent != "chosen" {
		t.Fatalf("suites = %#v, want only chosen", suites)
	}
}

func TestLoadSuitesRejectsSelectedDirectoryMismatch(t *testing.T) {
	root := t.TempDir()
	writeGoldenFixture(t, root, "chosen", "different")

	_, err := loadSuites(root, "chosen")
	if err == nil {
		t.Fatal("loadSuites unexpectedly accepted a mismatched Agent directory")
	}
}

func writeGoldenFixture(t *testing.T, root, directoryName, agentName string) {
	t.Helper()
	directory := filepath.Join(root, directoryName)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	fixture := fmt.Sprintf(`{
  "schema_version": "fgentic.agent.eval.v1",
  "agent": %q,
  "agent_contract_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "scenarios": [
    {
      "id": %q,
      "agent": %q,
      "prompt": "test",
      "rubric": {"kind": "exact", "expected": ["answer"]}
    }
  ]
}
`, agentName, agentName+"-smoke", agentName)
	if err := os.WriteFile(filepath.Join(directory, "golden.json"), []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
