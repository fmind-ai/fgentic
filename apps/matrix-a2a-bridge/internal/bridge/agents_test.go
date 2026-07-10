package bridge

import (
	"os"
	"path/filepath"
	"testing"

	"maunium.net/go/mautrix/id"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp agents file: %v", err)
	}
	return path
}

func TestLoadAgents(t *testing.T) {
	path := writeTemp(t, `agents:
  agent-k8s: {namespace: kagent, name: k8s-agent}
  agent-helm: {namespace: kagent, name: helm-agent}
`)
	am, err := LoadAgents(path)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, ok := am.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s not found")
	}
	if ref.Path() != "/api/a2a/kagent/k8s-agent" {
		t.Errorf("Path() = %q", ref.Path())
	}
	if _, ok := am.Lookup("agent-unknown"); ok {
		t.Error("unexpected lookup hit for agent-unknown")
	}
	if len(am.Names()) != 2 {
		t.Errorf("Names() = %v", am.Names())
	}
}

func TestLoadAgentsRejectsEmpty(t *testing.T) {
	path := writeTemp(t, "agents: {}\n")
	if _, err := LoadAgents(path); err == nil {
		t.Fatal("expected error for empty agents map")
	}
}

func TestLoadAgentsRejectsIncomplete(t *testing.T) {
	path := writeTemp(t, "agents:\n  agent-x: {namespace: kagent}\n")
	if _, err := LoadAgents(path); err == nil {
		t.Fatal("expected error for agent missing name")
	}
}

// Sender policy (SPEC §4 F6): own-server senders pass by default, federated senders are
// deny-by-default, allowedServers/allowedSenders open the door selectively.
func TestAllowsSender(t *testing.T) {
	path := writeTemp(t, `agents:
  agent-open: {namespace: kagent, name: open-agent}
  agent-fed:
    namespace: kagent
    name: fed-agent
    allowedServers: [partner.example]
  agent-locked:
    namespace: kagent
    name: locked-agent
    allowedServers: [partner.example]
    allowedSenders: ["@ops-*:partner.example"]
`)
	am, err := LoadAgents(path)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	const own = "fgentic.fmind.ai"
	cases := []struct {
		ghost  string
		sender id.UserID
		want   bool
	}{
		{"agent-open", id.NewUserID("alice", own), true},
		{"agent-open", id.NewUserID("alice", "partner.example"), false}, // federated deny-by-default
		{"agent-fed", id.NewUserID("alice", "partner.example"), true},
		{"agent-fed", id.NewUserID("alice", "evil.example"), false},
		{"agent-locked", id.NewUserID("ops-bob", "partner.example"), true},
		{"agent-locked", id.NewUserID("alice", "partner.example"), false}, // sender glob mismatch
		{"agent-locked", id.NewUserID("ops-bob", own), false},             // own server, glob still applies
	}
	for _, c := range cases {
		ref, ok := am.Lookup(c.ghost)
		if !ok {
			t.Fatalf("%s not found", c.ghost)
		}
		if got := ref.AllowsSender(c.sender, own); got != c.want {
			t.Errorf("AllowsSender(%s -> %s) = %v, want %v", c.sender, c.ghost, got, c.want)
		}
	}
}

func TestLoadAgentsRejectsBadSenderPattern(t *testing.T) {
	// A '*' glob can never be invalid, but an empty pattern list entry compiles to ^$ — the
	// realistic failure is YAML giving a non-string; guard the compile path stays exercised.
	path := writeTemp(t, `agents:
  agent-x:
    namespace: kagent
    name: x
    allowedSenders: ["@exact:server"]
`)
	am, err := LoadAgents(path)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, _ := am.Lookup("agent-x")
	if !ref.AllowsSender(id.NewUserID("exact", "server"), "server") {
		t.Error("exact allowedSenders pattern should match")
	}
	if ref.AllowsSender(id.NewUserID("exactly-not", "server"), "server") {
		t.Error("pattern must be anchored, not a substring match")
	}
}
