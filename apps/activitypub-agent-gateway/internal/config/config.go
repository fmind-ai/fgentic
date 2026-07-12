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
