// Package config holds the ActivityPub agent gateway's typed, environment-parsed configuration.
// Every operational value is parsed at the boundary and validated up front (fail fast); nothing
// is hardcoded. The gateway is the AP twin of the Matrix<->A2A bridge: it never holds a model
// credential and reaches kagent only through agentgateway (docs/fediverse.md §2).
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

const (
	// LogFormatJSON selects slog's structured JSON handler.
	LogFormatJSON = "json"
	// LogFormatText selects slog's human-readable text handler.
	LogFormatText = "text"
)

// Config is the fully-resolved gateway configuration.
type Config struct {
	// ServerName is the fediverse domain in an agent handle (acct:agent-<name>@<ServerName>) and
	// the default public host for actor IDs. Federation-ready: WebFinger matches the FULL handle,
	// never the localpart alone (docs/fediverse.md §6).
	ServerName string `env:"SERVER_NAME" envDefault:"fgentic.fmind.ai"`
	// PublicScheme/PublicHost form the externally-reachable base of every actor, inbox, and outbox
	// URL. PublicHost defaults to ServerName; a dedicated a2a.<domain> listener may override it.
	PublicScheme string `env:"PUBLIC_SCHEME" envDefault:"https"`
	PublicHost   string `env:"PUBLIC_HOST"`

	// A2APublicBaseURL is the externally-reachable base of the agent's A2A endpoint, advertised in
	// the published AgentCard and the actor's FEP-844e `implements` for cross-protocol discovery
	// (issue #215). It defaults to the gated federation A2A host (<scheme>://a2a.<PublicHost>); actual
	// reachability of that route stays governed by the federation profile (docs/fediverse.md §3).
	A2APublicBaseURL string `env:"A2A_PUBLIC_BASE_URL"`

	// ListenHost/ListenPort are where the public AP HTTP server binds (actor, WebFinger, inbox,
	// outbox). MetricsPort serves Prometheus /metrics on a separate, never-public side port.
	ListenHost  string `env:"LISTEN_HOST" envDefault:"0.0.0.0"`
	ListenPort  int    `env:"LISTEN_PORT" envDefault:"8480"`
	MetricsPort int    `env:"METRICS_PORT" envDefault:"9090"`

	// AgentsPath is the projected ConfigMap file mapping each exposed ghost (agent-<name>) to one
	// local kagent namespace/name plus a fallback description. GhostPrefix is the required handle
	// prefix so an AP actor is unambiguously a platform agent, never a human localpart.
	AgentsPath  string `env:"AGENTS_PATH" envDefault:"/etc/activitypub-agent-gateway/agents/agents.yaml"`
	GhostPrefix string `env:"GHOST_PREFIX" envDefault:"agent-"`

	// A2ABaseURL routes A2A THROUGH agentgateway by default (unified telemetry + the model-
	// credential chokepoint on the agent's own egress). A2AAPIKey authenticates this workload at
	// the gateway; it is deliberately separate from the asserted end-user identity (X-User-Id).
	A2ABaseURL string `env:"A2A_BASE_URL" envDefault:"http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8080"`
	A2AAPIKey  string `env:"A2A_API_KEY"`

	// PolicyPath is the mounted, git-reloadable federation-border allowlist (policy.json). When
	// empty the border is DISABLED — valid only for local-only dev where no untrusted actor can
	// reach the inbox; any public exposure requires a policy. PolicyReloadInterval is how often the
	// projected file is polled for a hot-swap without a pod restart (docs/fediverse.md §3).
	PolicyPath           string        `env:"POLICY_PATH"`
	PolicyReloadInterval time.Duration `env:"POLICY_RELOAD_INTERVAL" envDefault:"5s"`
	// SignatureMaxSkew bounds how far an inbound signature timestamp may drift from now (replay).
	SignatureMaxSkew time.Duration `env:"SIGNATURE_MAX_SKEW" envDefault:"12h"`

	// IntegrityKeyPath is the mounted, SOPS-backed PKCS#8 PEM Ed25519 key used to attach FEP-8b32
	// object integrity proofs to outbound replies (and publish the actor's assertionMethod Multikey).
	// Empty disables outbound signing. IntegrityKeyFragment names the key on each actor
	// (verificationMethod = <actorID>#<fragment>). IntegrityRequireInbound makes a valid object proof
	// mandatory on the inbox — it needs the border (POLICY_PATH), the single admission choke point.
	IntegrityKeyPath        string `env:"INTEGRITY_KEY_PATH"`
	IntegrityKeyFragment    string `env:"INTEGRITY_KEY_FRAGMENT" envDefault:"ed25519-key"`
	IntegrityRequireInbound bool   `env:"INTEGRITY_REQUIRE_INBOUND" envDefault:"false"`

	// IdentityKeyPath is the mounted, SOPS-backed PKCS#8 PEM P-256 key that anchors the FEP-c390
	// cross-transport identity (issue #218): each actor attaches a VerifiableIdentityStatement bound
	// to this key's did:key, and its AgentCard publishes the matching JWK. Empty disables the binding.
	IdentityKeyPath string `env:"IDENTITY_KEY_PATH"`

	// BudgetEnabled turns on per-actor/per-domain token-budget admission (D7/D8): every inbound
	// delegation reserves its token ceiling from the verified actor's and domain's git-configured
	// pools before any A2A call, deny-by-default for an allowlisted-but-unbudgeted domain. It needs
	// the border (POLICY_PATH), where budgets live. BudgetWindow is the rolling reservation window;
	// BudgetCapacity bounds the number of tracked actor/domain keys (cardinality safety).
	BudgetEnabled  bool          `env:"BUDGET_ENABLED" envDefault:"false"`
	BudgetWindow   time.Duration `env:"BUDGET_WINDOW" envDefault:"1m"`
	BudgetCapacity int           `env:"BUDGET_CAPACITY" envDefault:"4096"`

	// GroupsPath is the projected groups.yaml mapping each exposed collaboration room to an AP Group
	// actor (issue #217). Empty disables the Group surface. Groups need the signing key (for the
	// actor publicKey and signed outbound delivery) and the border (F3/F4/F5 on inbound group traffic).
	GroupsPath string `env:"GROUPS_PATH"`

	// StatusFeedEnabled turns on the follow-to-subscribe agent status feed (issue #219): agents accept
	// Follows and operational events (Prometheus alerts POSTed to the internal /alerts endpoint) are
	// published as signed status Notes fanned out to followers, capped at StatusMaxPerWindow per agent
	// per StatusWindow. Like groups, it needs the signing key and the border.
	StatusFeedEnabled  bool          `env:"STATUS_FEED_ENABLED" envDefault:"false"`
	StatusWindow       time.Duration `env:"STATUS_WINDOW" envDefault:"1m"`
	StatusMaxPerWindow int           `env:"STATUS_MAX_PER_WINDOW" envDefault:"6"`

	// RequestTimeout bounds one synchronous A2A message/send transport round trip. TaskTimeout
	// bounds the whole delegation when the agent returns a long-running Task polled via tasks/get.
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"60s"`
	TaskTimeout    time.Duration `env:"TASK_TIMEOUT" envDefault:"10m"`
	// ShutdownTimeout is the graceful HTTP drain budget on SIGTERM before the context is cancelled.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`
}

// Load parses the environment into a Config and validates it, failing fast on bad input.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}
	if c.PublicHost == "" {
		c.PublicHost = c.ServerName
	}
	if c.A2APublicBaseURL == "" {
		c.A2APublicBaseURL = c.PublicScheme + "://a2a." + c.PublicHost
	}
	c.A2APublicBaseURL = strings.TrimRight(c.A2APublicBaseURL, "/")
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("validate environment: %w", err)
	}
	return c, nil
}

// PublicBaseURL is the scheme+host prefix every actor, inbox, and outbox URL is built from.
func (c Config) PublicBaseURL() string {
	return c.PublicScheme + "://" + c.PublicHost
}

func (c Config) validate() error {
	if c.ServerName == "" {
		return fmt.Errorf("SERVER_NAME must not be empty")
	}
	if c.PublicScheme != "https" && c.PublicScheme != "http" {
		return fmt.Errorf("PUBLIC_SCHEME %q must be %q or %q", c.PublicScheme, "https", "http")
	}
	if c.PublicHost == "" {
		return fmt.Errorf("PUBLIC_HOST must not be empty")
	}
	if !strings.HasPrefix(c.A2APublicBaseURL, "http://") && !strings.HasPrefix(c.A2APublicBaseURL, "https://") {
		return fmt.Errorf("A2A_PUBLIC_BASE_URL %q must be an absolute http(s) URL", c.A2APublicBaseURL)
	}
	if c.A2ABaseURL == "" {
		return fmt.Errorf("A2A_BASE_URL must not be empty")
	}
	if c.AgentsPath == "" {
		return fmt.Errorf("AGENTS_PATH must not be empty")
	}
	if c.GhostPrefix == "" || c.GhostPrefix != strings.TrimSpace(c.GhostPrefix) {
		return fmt.Errorf("GHOST_PREFIX must be a non-empty, unpadded prefix")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("LISTEN_PORT %d out of range 1-65535", c.ListenPort)
	}
	if c.MetricsPort < 1 || c.MetricsPort > 65535 {
		return fmt.Errorf("METRICS_PORT %d out of range 1-65535", c.MetricsPort)
	}
	if c.MetricsPort == c.ListenPort {
		return fmt.Errorf("METRICS_PORT must differ from LISTEN_PORT (%d)", c.ListenPort)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("REQUEST_TIMEOUT must be positive")
	}
	if c.TaskTimeout < c.RequestTimeout {
		return fmt.Errorf("TASK_TIMEOUT (%s) must be >= REQUEST_TIMEOUT (%s)", c.TaskTimeout, c.RequestTimeout)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be positive")
	}
	if c.PolicyReloadInterval <= 0 {
		return fmt.Errorf("POLICY_RELOAD_INTERVAL must be positive")
	}
	if c.SignatureMaxSkew <= 0 {
		return fmt.Errorf("SIGNATURE_MAX_SKEW must be positive")
	}
	if c.IntegrityKeyFragment == "" || strings.ContainsAny(c.IntegrityKeyFragment, "#/ ") {
		return fmt.Errorf("INTEGRITY_KEY_FRAGMENT %q must be a non-empty fragment without '#', '/', or spaces", c.IntegrityKeyFragment)
	}
	if c.IntegrityRequireInbound && c.PolicyPath == "" {
		return fmt.Errorf("INTEGRITY_REQUIRE_INBOUND needs POLICY_PATH (object integrity gates the border)")
	}
	if c.GroupsPath != "" {
		if c.IntegrityKeyPath == "" {
			return fmt.Errorf("GROUPS_PATH needs INTEGRITY_KEY_PATH (the signing key authenticates group delivery)")
		}
		if c.PolicyPath == "" {
			return fmt.Errorf("GROUPS_PATH needs POLICY_PATH (the border gates inbound group traffic)")
		}
	}
	if c.StatusFeedEnabled {
		if c.IntegrityKeyPath == "" {
			return fmt.Errorf("STATUS_FEED_ENABLED needs INTEGRITY_KEY_PATH (status Notes are signed and delivery is signed)")
		}
		if c.PolicyPath == "" {
			return fmt.Errorf("STATUS_FEED_ENABLED needs POLICY_PATH (the border gates who may subscribe)")
		}
		if c.StatusWindow <= 0 {
			return fmt.Errorf("STATUS_WINDOW must be positive")
		}
		if c.StatusMaxPerWindow < 1 {
			return fmt.Errorf("STATUS_MAX_PER_WINDOW must be at least 1")
		}
	}
	if c.BudgetEnabled {
		if c.PolicyPath == "" {
			return fmt.Errorf("BUDGET_ENABLED needs POLICY_PATH (budgets live in the border policy)")
		}
		if c.BudgetWindow <= 0 {
			return fmt.Errorf("BUDGET_WINDOW must be positive")
		}
		if c.BudgetCapacity < 1 {
			return fmt.Errorf("BUDGET_CAPACITY must be at least 1")
		}
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		return fmt.Errorf("LOG_FORMAT %q must be %q or %q", c.LogFormat, LogFormatJSON, LogFormatText)
	}
	return nil
}

// SlogLevel parses LOG_LEVEL through the standard library's accepted level grammar.
func (c Config) SlogLevel() (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return 0, fmt.Errorf("LOG_LEVEL %q: %w", c.LogLevel, err)
	}
	return level, nil
}
