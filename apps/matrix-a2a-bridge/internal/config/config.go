// Package config holds the bridge's typed, environment-parsed configuration. Every operational
// value is parsed at the boundary and validated up front (fail fast); nothing is hardcoded.
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
	// A2A contexts, durable appservice intake, and delegation leases). Empty falls back to in-memory
	// state — dev only: restarts then lose the ledger and conversation threading.
	DatabaseURL string `env:"DATABASE_URL"`
	// AppserviceTransactionMaxBytes bounds the exact request body held while its hash and eligible
	// jobs are committed before acknowledgement. It is independent of media byte limits: Matrix
	// transactions carry event JSON and MXC references, not attachment bodies.
	AppserviceTransactionMaxBytes int64 `env:"APPSERVICE_TRANSACTION_MAX_BYTES" envDefault:"16777216"`

	// RequestTimeout bounds the synchronous A2A message/send transport round trip. TaskTimeout
	// bounds the whole delegation when the agent returns a long-running Task that the bridge
	// polls via tasks/get (SPEC §6).
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"60s"`
	TaskTimeout    time.Duration `env:"TASK_TIMEOUT" envDefault:"10m"`
	// InputWaitTimeout bounds how long a task paused in TASK_STATE_INPUT_REQUIRED waits for the
	// original sender's threaded reply before the bridge drops it (#116). Separate from TaskTimeout:
	// the poll clock does not burn while a human is thinking, so this is its own budget.
	InputWaitTimeout time.Duration `env:"INPUT_WAIT_TIMEOUT" envDefault:"10m"`
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
	// Durable worker timing. One coordinator polls while idle, so ClaimInterval bounds recovery
	// latency without multiplying idle database traffic by Concurrency. Active jobs heartbeat their
	// fenced leases; failed preflight/Matrix operations retry with capped exponential backoff.
	DelegationClaimInterval time.Duration `env:"DELEGATION_CLAIM_INTERVAL" envDefault:"1s"`
	DelegationLeaseDuration time.Duration `env:"DELEGATION_LEASE_DURATION" envDefault:"30s"`
	DelegationRetryInitial  time.Duration `env:"DELEGATION_RETRY_INITIAL" envDefault:"1s"`
	DelegationRetryMax      time.Duration `env:"DELEGATION_RETRY_MAX" envDefault:"30s"`
	DelegationMaxAttempts   int           `env:"DELEGATION_MAX_ATTEMPTS" envDefault:"5"`

	// Rate limits (token bucket) guarding LLM spend: per (sender, agent) pair and per room.
	SenderRatePerMinute float64 `env:"SENDER_RATE_PER_MINUTE" envDefault:"6"`
	SenderRateBurst     int     `env:"SENDER_RATE_BURST" envDefault:"3"`
	RoomRatePerMinute   float64 `env:"ROOM_RATE_PER_MINUTE" envDefault:"30"`
	RoomRateBurst       int     `env:"ROOM_RATE_BURST" envDefault:"10"`
	// RateLimitBucketCapacity independently caps each sender/room invocation/notice bucket map.
	RateLimitBucketCapacity int `env:"RATE_LIMIT_BUCKET_CAPACITY" envDefault:"4096"`

	// CancelModeratorPowerLevel is the room power level a member who did NOT start a delegation must
	// hold to cancel it by reacting to its placeholder (❌). The original delegating sender may always
	// cancel their own task regardless of power level. 50 is the Matrix moderator convention; raise it
	// to restrict cancellation to admins, or lower it toward 0 to let any room member cancel.
	CancelModeratorPowerLevel int `env:"CANCEL_MODERATOR_POWER_LEVEL" envDefault:"50"`

	// MaxTaskProgressPosts bounds the threaded working-state updates a long task may post to its
	// placeholder thread (SPEC §6). Progress is deduplicated and capped so it never becomes notice
	// spam under a flood of status changes (D7 response plane); 0 disables progress posting entirely.
	MaxTaskProgressPosts int `env:"MAX_TASK_PROGRESS_POSTS" envDefault:"3"`
	// PinInFlightTasks opts into pinning a running long task's placeholder (m.room.pinned_events) for
	// a zero-UI "what is running here" view, unpinning on any terminal state. Off by default because
	// it needs the ghost to hold the room's state-event power level; where power is missing it degrades
	// silently. Enable it only after granting agent ghosts that power (matrix-agents add-agent runbook).
	PinInFlightTasks bool `env:"PIN_IN_FLIGHT_TASKS" envDefault:"false"`

	// StagingRooms lists the Matrix room IDs where `stage: dev` agents may be invoked (#128). A dev
	// agent mentioned in any other room is refused fail-closed with a stage_policy_rejected audit.
	// This is the single authoritative staging-room list; it never lives in agents.yaml, so the
	// stage flag and the room set cannot drift across files. Empty means every dev agent is
	// deny-by-default everywhere, so an unconfigured staging boundary never silently promotes.
	StagingRooms []string `env:"STAGING_ROOMS" envSeparator:","`

	// Media policy (#115): files flowing either direction between Matrix and A2A are gated by an
	// exact-match MIME allowlist and byte caps, because documents are a sharper injection/malware
	// vector than chat text. MediaMaxBytes is the on/off switch: 0 disables the media path entirely
	// (inbound files are refused, outbound artifact bytes are withheld). It is the cap on a single
	// file; MediaMaxTotalBytes caps the summed bytes of one delegation in each direction.
	// MediaMIMEAllowlist entries are exact `type/subtype` (no wildcards) matched case-insensitively
	// after stripping any parameters (e.g. "; charset=utf-8"); only allowlisted types cross.
	MediaMIMEAllowlist []string `env:"MEDIA_MIME_ALLOWLIST" envSeparator:"," envDefault:"text/csv,text/plain,text/markdown,application/json,application/pdf,image/png,image/jpeg,image/gif,image/webp"`
	MediaMaxBytes      int64    `env:"MEDIA_MAX_BYTES" envDefault:"10485760"`
	MediaMaxTotalBytes int64    `env:"MEDIA_MAX_TOTAL_BYTES" envDefault:"26214400"`

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
		return Config{}, fmt.Errorf("validate environment: %w", err)
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
	if c.AppserviceTransactionMaxBytes <= 0 {
		return fmt.Errorf("APPSERVICE_TRANSACTION_MAX_BYTES must be positive")
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
	if c.InputWaitTimeout <= 0 {
		return fmt.Errorf("INPUT_WAIT_TIMEOUT must be positive")
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
	if c.DelegationClaimInterval <= 0 {
		return fmt.Errorf("DELEGATION_CLAIM_INTERVAL must be positive")
	}
	if c.DelegationLeaseDuration <= 0 || c.DelegationLeaseDuration/3 <= 0 {
		return fmt.Errorf("DELEGATION_LEASE_DURATION must allow a positive heartbeat interval")
	}
	if c.DelegationRetryInitial <= 0 || c.DelegationRetryMax < c.DelegationRetryInitial {
		return fmt.Errorf("delegation retry durations must be positive and RETRY_MAX >= RETRY_INITIAL")
	}
	if c.DelegationMaxAttempts < 1 {
		return fmt.Errorf("DELEGATION_MAX_ATTEMPTS must be >= 1")
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
	if c.CancelModeratorPowerLevel < 0 {
		return fmt.Errorf("CANCEL_MODERATOR_POWER_LEVEL must be >= 0")
	}
	if c.MaxTaskProgressPosts < 0 {
		return fmt.Errorf("MAX_TASK_PROGRESS_POSTS must be >= 0")
	}
	for _, room := range c.StagingRooms {
		if !strings.HasPrefix(room, "!") || !strings.Contains(room, ":") {
			return fmt.Errorf("STAGING_ROOMS entry %q must be a Matrix room ID (!opaque:server)", room)
		}
	}
	if err := c.validateMedia(); err != nil {
		return err
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		return fmt.Errorf("LOG_FORMAT %q must be %q or %q", c.LogFormat, LogFormatJSON, LogFormatText)
	}
	return nil
}

// validateMedia rejects an inconsistent media policy fail-fast (#115). MEDIA_MAX_BYTES=0 disables the
// media path (allowlist then ignored); a positive cap turns it on and requires a non-empty allowlist
// and an ordered per-delegation total. Every allowlist entry must be a bare `type/subtype` so a
// wildcard or malformed value can never silently widen the gate. Setting MEDIA_MIME_ALLOWLIST="" is
// NOT a disable switch — caarlos0/env re-applies the built-in default for an empty value — so
// disabling is done through MEDIA_MAX_BYTES.
func (c Config) validateMedia() error {
	if c.MediaMaxBytes < 0 {
		return fmt.Errorf("MEDIA_MAX_BYTES must be >= 0")
	}
	if c.MediaMaxTotalBytes < 0 {
		return fmt.Errorf("MEDIA_MAX_TOTAL_BYTES must be >= 0")
	}
	entries := 0
	for _, mime := range c.MediaMIMEAllowlist {
		if strings.TrimSpace(mime) == "" {
			continue
		}
		entries++
		if strings.ContainsAny(mime, " *") || strings.Count(mime, "/") != 1 ||
			strings.HasPrefix(mime, "/") || strings.HasSuffix(mime, "/") {
			return fmt.Errorf("MEDIA_MIME_ALLOWLIST entry %q must be an exact type/subtype", mime)
		}
	}
	if c.MediaMaxBytes > 0 {
		if entries == 0 {
			return fmt.Errorf("MEDIA_MIME_ALLOWLIST must list at least one type when MEDIA_MAX_BYTES > 0 (set MEDIA_MAX_BYTES=0 to disable media)")
		}
		if c.MediaMaxTotalBytes < c.MediaMaxBytes {
			return fmt.Errorf("MEDIA_MAX_TOTAL_BYTES (%d) must be >= MEDIA_MAX_BYTES (%d)", c.MediaMaxTotalBytes, c.MediaMaxBytes)
		}
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
