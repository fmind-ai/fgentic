package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

const validRemoteAgentsYAML = `agents:
  agent-remote:
    url: https://partner.example/a2a
    timeout: 12s
    tokenBudget: 8192
    cardIdentity:
      name: Partner Helper
      organization: Partner Corp
      keyID: partner-2026
      publicKey:
        kty: EC
        crv: P-256
        x: axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY
        y: T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU
`

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
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    description: Diagnoses cluster health during startup outages.
    avatarURL: mxc://fgentic.fmind.ai/k8s-avatar
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
	if ref.Target().IsRemote() {
		t.Error("local Target().IsRemote() = true")
	}
	if ref.Timeout() != 0 {
		t.Errorf("local Timeout() = %s, want zero", ref.Timeout())
	}
	if ref.MappingID() == "" {
		t.Error("MappingID() is empty")
	}
	if ref.Description != "Diagnoses cluster health during startup outages." {
		t.Errorf("Description = %q", ref.Description)
	}
	if got := ref.Avatar().String(); got != "mxc://fgentic.fmind.ai/k8s-avatar" {
		t.Errorf("Avatar() = %q", got)
	}
	if _, ok := am.Lookup("agent-unknown"); ok {
		t.Error("unexpected lookup hit for agent-unknown")
	}
	if len(am.Names()) != 2 {
		t.Errorf("Names() = %v", am.Names())
	}
}

func TestLoadAgentsRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "empty map", content: "agents: {}\n", want: "defines no agents"},
		{
			name:    "missing agent field",
			content: "agents:\n  agent-x: {namespace: kagent}\n",
			want:    "both namespace and name are required",
		},
		{
			name:    "null agent",
			content: "agents:\n  agent-x: null\n",
			want:    "target configuration must not be null",
		},
		{name: "malformed YAML", content: "agents: [\n", want: "parse agents file"},
		{
			name: "non-Matrix avatar",
			content: `agents:
  agent-x:
    namespace: kagent
    name: x
    avatarURL: https://example.com/avatar.png
`,
			want: "avatarURL must be an mxc:// URI",
		},
		{
			name:    "invalid ghost localpart",
			content: "agents:\n  Agent-X: {namespace: kagent, name: x}\n",
			want:    "ghost must be a valid Matrix user localpart",
		},
		{
			name: "both target forms",
			content: strings.Replace(
				validRemoteAgentsYAML,
				"    url: https://partner.example/a2a",
				"    namespace: kagent\n    name: helper\n    url: https://partner.example/a2a",
				1,
			),
			want: "exactly one target form is required",
		},
		{
			name: "explicit empty url is still a second target form",
			content: `agents:
  agent-x:
    namespace: kagent
    name: x
    url: ""
`,
			want: "exactly one target form is required",
		},
		{
			name: "local target with remote policy",
			content: `agents:
  agent-x:
    namespace: kagent
    name: x
    timeout: 2s
`,
			want: "only valid for a url target",
		},
		{
			name:    "remote target without timeout",
			content: strings.Replace(validRemoteAgentsYAML, "    timeout: 12s\n", "", 1),
			want:    "timeout must be positive",
		},
		{
			name:    "remote target with zero timeout",
			content: strings.Replace(validRemoteAgentsYAML, "timeout: 12s", "timeout: 0s", 1),
			want:    "timeout must be positive",
		},
		{
			name:    "remote target without token budget",
			content: strings.Replace(validRemoteAgentsYAML, "    tokenBudget: 8192\n", "", 1),
			want:    "tokenBudget must be positive",
		},
		{
			name:    "remote target with zero token budget",
			content: strings.Replace(validRemoteAgentsYAML, "tokenBudget: 8192", "tokenBudget: 0", 1),
			want:    "tokenBudget must be positive",
		},
		{
			name:    "remote target without card identity",
			content: strings.Split(validRemoteAgentsYAML, "    cardIdentity:")[0],
			want:    "cardIdentity is required",
		},
		{
			name:    "external cleartext URL",
			content: strings.Replace(validRemoteAgentsYAML, "https://partner.example", "http://partner.example", 1),
			want:    "must use HTTPS",
		},
		{
			name:    "noncanonical trailing slash URL",
			content: strings.Replace(validRemoteAgentsYAML, "https://partner.example/a2a", "https://partner.example/a2a/", 1),
			want:    "must be canonical without a trailing slash",
		},
		{
			name:    "identity with surrounding whitespace",
			content: strings.Replace(validRemoteAgentsYAML, "name: Partner Helper", "name: ' Partner Helper'", 1),
			want:    "without surrounding whitespace",
		},
		{
			name:    "non-P256 key",
			content: strings.Replace(validRemoteAgentsYAML, "crv: P-256", "crv: P-384", 1),
			want:    "must be an EC P-256 key",
		},
		{
			name:    "invalid P256 point",
			content: strings.Replace(validRemoteAgentsYAML, "axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 1),
			want:    "coordinates are not on P-256",
		},
		{
			name:    "unknown field",
			content: strings.Replace(validRemoteAgentsYAML, "    timeout: 12s", "    requestTimeout: 12s", 1),
			want:    "field requestTimeout not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadAgents(writeTemp(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadAgents() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadAgentsRemoteTarget(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote not found")
	}
	if !ref.Target().IsRemote() {
		t.Error("Target().IsRemote() = false")
	}
	if got := ref.Target().String(); got != "https://partner.example/a2a" {
		t.Errorf("Target().String() = %q", got)
	}
	if got := ref.Target().TokenBudget(); got != 8192 {
		t.Errorf("Target().TokenBudget() = %d", got)
	}
	if got := ref.Timeout(); got != 12*time.Second {
		t.Errorf("Timeout() = %s", got)
	}
	if ref.Name != "Partner Helper" {
		t.Errorf("fallback Name = %q", ref.Name)
	}
	if ref.MappingID() == "" {
		t.Error("MappingID() is empty")
	}
}

func TestRemoteURLTransportPolicy(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "public HTTPS", url: "https://partner.example/a2a", want: true},
		{name: "IPv4 loopback", url: "http://127.0.0.1:8080/a2a", want: true},
		{name: "IPv6 loopback", url: "http://[::1]:8080/a2a", want: true},
		{name: "localhost subdomain", url: "http://fixture.localhost:8080/a2a", want: true},
		{name: "single-label service", url: "http://a2a-stub:8080/a2a", want: true},
		{name: "service namespace", url: "http://a2a-stub.default.svc:8080/a2a", want: true},
		{name: "cluster-local service", url: "http://a2a-stub.default.svc.cluster.local:8080/a2a", want: true},
		{name: "public cleartext", url: "http://partner.example/a2a"},
		{name: "localhost lookalike", url: "http://localhost.evil.example/a2a"},
		{name: "svc lookalike", url: "http://a2a-stub.default.svc.evil/a2a"},
		{name: "missing authority", url: "https:///a2a"},
		{name: "credentials", url: "https://user:secret@partner.example/a2a"},
		{name: "query", url: "https://partner.example/a2a?tenant=other"},
		{name: "non-HTTP scheme", url: "ftp://a2a-stub/a2a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRemoteURL(tt.url)
			if (err == nil) != tt.want {
				t.Fatalf("validateRemoteURL(%q) error = %v, want valid=%v", tt.url, err, tt.want)
			}
		})
	}
}

func TestAgentRefSameTargetBindsTrustAndOperationalPolicy(t *testing.T) {
	load := func(t *testing.T, content string) *AgentRef {
		t.Helper()
		agents, err := LoadAgents(writeTemp(t, content))
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		ref, ok := agents.Lookup("agent-remote")
		if !ok {
			t.Fatal("agent-remote not found")
		}
		return ref
	}

	baseline := load(t, validRemoteAgentsYAML)
	sameTarget := load(t, strings.Replace(
		validRemoteAgentsYAML,
		"    url: https://partner.example/a2a/",
		"    url: https://partner.example/a2a/\n    description: Updated profile fallback",
		1,
	))
	changedSigner := load(t, strings.Replace(validRemoteAgentsYAML, "partner-2026", "partner-2027", 1))
	changedBudget := load(t, strings.Replace(validRemoteAgentsYAML, "tokenBudget: 8192", "tokenBudget: 4096", 1))
	changedTimeout := load(t, strings.Replace(validRemoteAgentsYAML, "timeout: 12s", "timeout: 13s", 1))

	if !baseline.SameTarget(sameTarget) {
		t.Error("identical target is not equal")
	}
	for name, changed := range map[string]*AgentRef{
		"signer": changedSigner, "budget": changedBudget, "timeout": changedTimeout,
	} {
		t.Run(name, func(t *testing.T) {
			if baseline.SameTarget(changed) {
				t.Errorf("SameTarget() = true after %s change", name)
			}
			if baseline.MappingID() == changed.MappingID() {
				t.Errorf("MappingID() unchanged after %s change", name)
			}
		})
	}
}

func TestLoadAgentsRejectsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := LoadAgents(path)
	if err == nil || !strings.Contains(err.Error(), "read agents file") {
		t.Fatalf("LoadAgents() error = %v, want read error", err)
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
		name   string
		ghost  string
		sender id.UserID
		want   bool
	}{
		{name: "own server allowed by default", ghost: "agent-open", sender: id.NewUserID("alice", own), want: true},
		{name: "federated denied by default", ghost: "agent-open", sender: id.NewUserID("alice", "partner.example"), want: false},
		{name: "allowed federated server", ghost: "agent-fed", sender: id.NewUserID("alice", "partner.example"), want: true},
		{name: "unlisted federated server", ghost: "agent-fed", sender: id.NewUserID("alice", "evil.example"), want: false},
		{name: "sender glob matches", ghost: "agent-locked", sender: id.NewUserID("ops-bob", "partner.example"), want: true},
		{name: "sender glob mismatch", ghost: "agent-locked", sender: id.NewUserID("alice", "partner.example"), want: false},
		{name: "sender glob restricts own server", ghost: "agent-locked", sender: id.NewUserID("ops-bob", own), want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, ok := am.Lookup(c.ghost)
			if !ok {
				t.Fatalf("%s not found", c.ghost)
			}
			if got := ref.AllowsSender(am.IdentifySender(c.sender), own); got != c.want {
				t.Errorf("AllowsSender(%s -> %s) = %v, want %v", c.sender, c.ghost, got, c.want)
			}
		})
	}
}

func TestAllowsSenderGlobIsAnchored(t *testing.T) {
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
	// Treat each sender's homeserver as local so the sender glob, rather than the federated
	// server allowlist, is the only policy under test.
	tests := []struct {
		name      string
		sender    id.UserID
		ownServer string
		want      bool
	}{
		{
			name:      "exact match",
			sender:    id.NewUserID("exact", "server"),
			ownServer: "server",
			want:      true,
		},
		{
			name:      "homeserver suffix is not a prefix match",
			sender:    id.NewUserID("exact", "server.example"),
			ownServer: "server.example",
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ref.AllowsSender(am.IdentifySender(tt.sender), tt.ownServer); got != tt.want {
				t.Errorf("AllowsSender(%s) = %v, want %v", tt.sender, got, tt.want)
			}
		})
	}
}

func TestBridgedSenderRequiresExplicitAgentAllowlist(t *testing.T) {
	path := writeTemp(t, `bridgedOrigins:
  slack: ["@slack_*:fgentic.fmind.ai"]
agents:
  agent-open: {namespace: kagent, name: open-agent}
  agent-slack:
    namespace: kagent
    name: slack-agent
    allowedSenders: ["@slack_*:fgentic.fmind.ai"]
  agent-exact:
    namespace: kagent
    name: exact-agent
    allowedSenders: ["@slack_U123:fgentic.fmind.ai"]
  agent-federated:
    namespace: kagent
    name: federated-agent
    allowedServers: [partner.example]
`)
	am, err := LoadAgents(path)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}

	tests := []struct {
		name   string
		ghost  string
		sender id.UserID
		want   bool
	}{
		{
			name:   "bridged sender denied by open local policy",
			ghost:  "agent-open",
			sender: "@slack_U123:fgentic.fmind.ai",
		},
		{
			name:   "bridged sender allowed by explicit namespace",
			ghost:  "agent-slack",
			sender: "@slack_U123:fgentic.fmind.ai",
			want:   true,
		},
		{
			name:   "bridged sender allowed by explicit exact MXID",
			ghost:  "agent-exact",
			sender: "@slack_U123:fgentic.fmind.ai",
			want:   true,
		},
		{
			name:   "bridged sender exact MXID mismatch",
			ghost:  "agent-exact",
			sender: "@slack_U999:fgentic.fmind.ai",
		},
		{
			name:   "ordinary local sender remains allowed by default",
			ghost:  "agent-open",
			sender: "@alice:fgentic.fmind.ai",
			want:   true,
		},
		{
			name:   "foreign lookalike retains federated policy",
			ghost:  "agent-federated",
			sender: "@slack_U123:partner.example",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := am.Lookup(tt.ghost)
			if !ok {
				t.Fatalf("agent %q not found", tt.ghost)
			}
			if got := ref.AllowsSender(am.IdentifySender(tt.sender), ownServer); got != tt.want {
				t.Errorf("AllowsSender(%s -> %s) = %v, want %v", tt.sender, tt.ghost, got, tt.want)
			}
		})
	}
}

func TestAgentMapReplaceUpdatesBridgedOrigins(t *testing.T) {
	load := func(t *testing.T, network, pattern string) *AgentMap {
		t.Helper()
		agents, err := LoadAgents(writeTemp(t, "bridgedOrigins:\n  "+network+": [\""+pattern+"\"]\nagents:\n  agent-x: {namespace: kagent, name: x}\n"))
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		return agents
	}

	agents := load(t, "slack", "@slack_*:fgentic.fmind.ai")
	if got := agents.IdentifySender("@slack_U123:fgentic.fmind.ai").origin.network; got != "slack" {
		t.Fatalf("initial origin network = %q, want slack", got)
	}

	agents.Replace(load(t, "discord", "@discord_*:fgentic.fmind.ai"))
	if got := agents.IdentifySender("@slack_U123:fgentic.fmind.ai").origin.network; got != matrixOriginNetwork {
		t.Errorf("replaced Slack origin network = %q, want %q", got, matrixOriginNetwork)
	}
	if got := agents.IdentifySender("@discord_456:fgentic.fmind.ai").origin.network; got != "discord" {
		t.Errorf("replaced Discord origin network = %q, want discord", got)
	}
}
