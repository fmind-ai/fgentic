// Package config holds the bridge's typed, environment-parsed configuration. Every operational
// value is parsed at the boundary and validated up front (fail fast); nothing is hardcoded.
package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the fully-resolved bridge configuration.
type Config struct {
	// HomeserverURL is the Matrix Client-Server API base URL the bridge talks to (Synapse).
	HomeserverURL string `env:"HOMESERVER_URL" envDefault:"http://synapse.matrix.svc.cluster.local:8008"`
	// ServerName is the Matrix server_name (the ":fgentic.fmind.ai" part of every user ID).
	ServerName string `env:"SERVER_NAME" envDefault:"fgentic.fmind.ai"`

	// ListenHost/ListenPort are where the appservice HTTP transaction server binds; the
	// homeserver pushes room events here (PUT /_matrix/app/v1/transactions/{txnID}).
	ListenHost string `env:"LISTEN_HOST" envDefault:"0.0.0.0"`
	ListenPort int    `env:"LISTEN_PORT" envDefault:"29331"`

	// MetricsPort serves Prometheus /metrics (side port, never exposed publicly — SPEC §9.3).
	MetricsPort int `env:"METRICS_PORT" envDefault:"9090"`

	// RegistrationPath is the appservice registration.yaml (as_token/hs_token must match the
	// homeserver's copy). AgentsPath is the ghost->agent routing map + sender policy. The bridge
	// polls that projected ConfigMap file at AgentsReloadInterval for atomic live reloads.
	RegistrationPath     string        `env:"REGISTRATION_PATH" envDefault:"/etc/matrix-a2a-bridge/registration.yaml"`
	AgentsPath           string        `env:"AGENTS_PATH" envDefault:"/etc/matrix-a2a-bridge/agents/agents.yaml"`
	AgentsReloadInterval time.Duration `env:"AGENTS_RELOAD_INTERVAL" envDefault:"5s"`
	// AgentCardRefreshInterval controls independent revalidation of remote, signed AgentCards.
	// It is deliberately slower than projected-config polling: remote trust refreshes perform
	// network I/O and use HTTP validators, while agents.yaml reloads are local file reads.
	AgentCardRefreshInterval time.Duration `env:"AGENT_CARD_REFRESH_INTERVAL" envDefault:"5m"`

	// A2ABaseURL is the base the bridge dials for A2A. By default it routes THROUGH agentgateway
	// (unified LLM/MCP/A2A telemetry + the model-credential chokepoint on the agent's own egress).
	// Point it directly at kagent (http://kagent-controller.kagent.svc.cluster.local:8083) to skip
	// the gateway hop — functionally equivalent for fire-and-forget message/send.
	A2ABaseURL string `env:"A2A_BASE_URL" envDefault:"http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8080"`
	// A2AAPIKey authenticates this bridge workload at agentgateway. It is deliberately separate
	// from X-User-Id, which carries Matrix attribution but is not a caller credential.
	A2AAPIKey string `env:"A2A_API_KEY"`

	// GhostPrefix is the local-part prefix for agent ghost users (@agent-k8s -> prefix "agent-").
	GhostPrefix string `env:"GHOST_PREFIX" envDefault:"agent-"`

	// DatabaseURL is the Postgres URL backing the bridge state (mautrix StateStore, per-room/agent
	// A2A contexts, processed-event dedup). Empty falls back to in-memory state — dev only:
	// restarts then lose conversation threading and may re-process redelivered transactions.
	DatabaseURL string `env:"DATABASE_URL"`

	// RequestTimeout bounds the synchronous A2A message/send transport round trip. TaskTimeout
	// bounds the whole delegation when the agent returns a long-running Task that the bridge
	// polls via tasks/get (SPEC §6).
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"60s"`
	TaskTimeout    time.Duration `env:"TASK_TIMEOUT" envDefault:"10m"`
	// ShutdownTimeout is the graceful dispatcher-drain budget after Matrix intake stops. When it
	// expires, the runtime context is cancelled so running jobs terminate and queued jobs audit as
	// shutdown drops before process resources are released.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`

	// Concurrency caps in-flight A2A delegations across all rooms (per-room order is preserved
	// by the dispatcher regardless).
	Concurrency int `env:"CONCURRENCY" envDefault:"16"`
	// RoomQueueCapacity and GlobalQueueCapacity bound all accepted jobs, including running jobs,
	// so room floods and high-cardinality room churn cannot grow queues or drain goroutines
	// without limit.
	RoomQueueCapacity   int `env:"ROOM_QUEUE_CAPACITY" envDefault:"32"`
	GlobalQueueCapacity int `env:"GLOBAL_QUEUE_CAPACITY" envDefault:"256"`

	// Rate limits (token bucket) guarding LLM spend: per (sender, agent) pair and per room.
	SenderRatePerMinute float64 `env:"SENDER_RATE_PER_MINUTE" envDefault:"6"`
	SenderRateBurst     int     `env:"SENDER_RATE_BURST" envDefault:"3"`
	RoomRatePerMinute   float64 `env:"ROOM_RATE_PER_MINUTE" envDefault:"30"`
	RoomRateBurst       int     `env:"ROOM_RATE_BURST" envDefault:"10"`
	// RateLimitBucketCapacity independently caps each sender/room invocation/notice bucket map.
	RateLimitBucketCapacity int `env:"RATE_LIMIT_BUCKET_CAPACITY" envDefault:"4096"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`
}

// Load parses the environment into a Config and validates it, failing fast on bad input.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.ServerName == "" {
		return fmt.Errorf("SERVER_NAME must not be empty")
	}
	if c.HomeserverURL == "" {
		return fmt.Errorf("HOMESERVER_URL must not be empty")
	}
	if c.A2ABaseURL == "" {
		return fmt.Errorf("A2A_BASE_URL must not be empty")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("LISTEN_PORT %d out of range 1-65535", c.ListenPort)
	}
	if c.MetricsPort < 1 || c.MetricsPort > 65535 {
		return fmt.Errorf("METRICS_PORT %d out of range 1-65535", c.MetricsPort)
	}
	if c.GhostPrefix == "" {
		return fmt.Errorf("GHOST_PREFIX must not be empty")
	}
	if c.AgentsReloadInterval <= 0 {
		return fmt.Errorf("AGENTS_RELOAD_INTERVAL must be positive")
	}
	if c.AgentCardRefreshInterval <= 0 {
		return fmt.Errorf("AGENT_CARD_REFRESH_INTERVAL must be positive")
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
	if c.Concurrency < 1 {
		return fmt.Errorf("CONCURRENCY must be >= 1")
	}
	if c.RoomQueueCapacity < 1 {
		return fmt.Errorf("ROOM_QUEUE_CAPACITY must be >= 1")
	}
	if c.GlobalQueueCapacity < c.Concurrency {
		return fmt.Errorf("GLOBAL_QUEUE_CAPACITY must be >= CONCURRENCY")
	}
	if c.SenderRatePerMinute <= 0 || c.RoomRatePerMinute <= 0 {
		return fmt.Errorf("rate limits must be positive")
	}
	if c.SenderRateBurst < 1 || c.RoomRateBurst < 1 {
		return fmt.Errorf("rate bursts must be >= 1")
	}
	if c.RateLimitBucketCapacity < 1 {
		return fmt.Errorf("RATE_LIMIT_BUCKET_CAPACITY must be >= 1")
	}
	return nil
}
