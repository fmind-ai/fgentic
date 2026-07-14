package bridge

import (
	"strings"
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestIdentifySenderUsesOnlyConfiguredFullMXIDNamespaces(t *testing.T) {
	rules, err := compileBridgedOrigins(map[string][]string{
		"discord": {"@discord_*:fgentic.fmind.ai"},
		"slack":   {"@slack_*:fgentic.fmind.ai"},
	})
	if err != nil {
		t.Fatalf("compileBridgedOrigins: %v", err)
	}
	am := &AgentMap{bridgedOrigins: rules}

	tests := []struct {
		name        string
		mxid        id.UserID
		wantKind    senderOriginKind
		wantNetwork string
	}{
		{
			name:        "Slack namespace",
			mxid:        "@slack_U123:fgentic.fmind.ai",
			wantKind:    senderOriginBridge,
			wantNetwork: "slack",
		},
		{
			name:        "future bridge namespace",
			mxid:        "@discord_456:fgentic.fmind.ai",
			wantKind:    senderOriginBridge,
			wantNetwork: "discord",
		},
		{
			name:        "ordinary local Matrix user",
			mxid:        "@alice:fgentic.fmind.ai",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "foreign Slack lookalike",
			mxid:        "@slack_U123:partner.example",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "localpart prefix lookalike",
			mxid:        "@xslack_U123:fgentic.fmind.ai",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "homeserver suffix lookalike",
			mxid:        "@slack_U123:fgentic.fmind.ai.evil.example",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "bare localpart is never identity evidence",
			mxid:        "slack_U123",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "display name is never identity evidence",
			mxid:        "Slack User",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "malformed MXID is not classified",
			mxid:        "@slack_U123",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
		{
			name:        "malformed localpart is not classified",
			mxid:        "@slack_U@123:fgentic.fmind.ai",
			wantKind:    senderOriginMatrix,
			wantNetwork: matrixOriginNetwork,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := am.IdentifySender(tt.mxid)
			if got.origin.kind != tt.wantKind || got.origin.network != tt.wantNetwork {
				t.Errorf(
					"IdentifySender(%q) origin = (%q, %q), want (%q, %q)",
					tt.mxid,
					got.origin.kind,
					got.origin.network,
					tt.wantKind,
					tt.wantNetwork,
				)
			}
		})
	}
}

func TestCompileBridgedOriginsRejectsUnsafeOrAmbiguousNamespaces(t *testing.T) {
	tests := []struct {
		name   string
		config map[string][]string
		want   string
	}{
		{name: "reserved network", config: map[string][]string{"matrix": {"@matrix_*:server"}}, want: "must not be"},
		{name: "invalid network", config: map[string][]string{"Slack": {"@slack_*:server"}}, want: "must match"},
		{name: "empty namespace list", config: map[string][]string{"slack": {}}, want: "defines no MXID namespaces"},
		{name: "bare localpart", config: map[string][]string{"slack": {"slack_*"}}, want: "full MXID glob"},
		{name: "exact user is not a namespace", config: map[string][]string{"slack": {"@slack_U123:server"}}, want: "exactly one '*'"},
		{name: "catch all localpart", config: map[string][]string{"slack": {"@*:server"}}, want: "invalid literal localpart prefix"},
		{name: "middle wildcard", config: map[string][]string{"slack": {"@slack_*_user:server"}}, want: "must end its localpart"},
		{name: "multiple wildcards", config: map[string][]string{"slack": {"@slack_**:server"}}, want: "exactly one '*'"},
		{name: "invalid local prefix", config: map[string][]string{"slack": {"@slack user*:server"}}, want: "invalid literal localpart prefix"},
		{name: "wildcard server", config: map[string][]string{"slack": {"@slack_*:*.example"}}, want: "invalid homeserver"},
		{name: "invalid server", config: map[string][]string{"slack": {"@slack_*:bad server"}}, want: "invalid homeserver"},
		{
			name: "duplicate namespace",
			config: map[string][]string{
				"slack": {"@slack_*:server", "@slack_*:server"},
			},
			want: "overlap",
		},
		{
			name: "nested namespace across networks",
			config: map[string][]string{
				"slack":    {"@bridge_*:server"},
				"telegram": {"@bridge_telegram_*:server"},
			},
			want: "overlap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileBridgedOrigins(tt.config)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("compileBridgedOrigins() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCompileBridgedOriginsAllowsDisjointNamespaces(t *testing.T) {
	_, err := compileBridgedOrigins(map[string][]string{
		"discord": {"@discord_*:server"},
		"slack":   {"@slack_*:server", "@slack_*:partner.example"},
	})
	if err != nil {
		t.Fatalf("compileBridgedOrigins: %v", err)
	}
}

func TestSenderIdentityRateLimitKeyAttributesBridgeOrigin(t *testing.T) {
	tests := []struct {
		name   string
		sender senderIdentity
		agent  string
		want   string
	}{
		{
			name:   "native key remains backward compatible",
			sender: matrixSender("@alice:fgentic.fmind.ai"),
			agent:  "agent-k8s",
			want:   "@alice:fgentic.fmind.ai|agent-k8s",
		},
		{
			name: "bridged key includes bounded origin and full Matrix sender",
			sender: senderIdentity{
				mxid: "@slack_U123:fgentic.fmind.ai",
				origin: senderOrigin{
					kind:    senderOriginBridge,
					network: "slack",
				},
			},
			agent: "agent-k8s",
			want:  "bridge:slack|@slack_U123:fgentic.fmind.ai|agent-k8s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sender.rateLimitKey(tt.agent); got != tt.want {
				t.Errorf("rateLimitKey(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}
}
