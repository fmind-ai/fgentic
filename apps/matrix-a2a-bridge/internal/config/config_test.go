package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadDefaults checks that, with only the required-but-defaulted vars left unset, the
// documented defaults apply. We do not set any env here so envDefault governs every field.
func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with defaults: %v", err)
	}
	if cfg.ServerName != "fgentic.fmind.ai" {
		t.Errorf("ServerName = %q, want fgentic.fmind.ai", cfg.ServerName)
	}
	if cfg.ListenPort != 29331 {
		t.Errorf("ListenPort = %d, want 29331", cfg.ListenPort)
	}
	if cfg.RequestTimeout != 60*time.Second {
		t.Errorf("RequestTimeout = %s, want 60s", cfg.RequestTimeout)
	}
	if cfg.ShutdownTimeout != 25*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 25s", cfg.ShutdownTimeout)
	}
	if cfg.GhostPrefix != "agent-" {
		t.Errorf("GhostPrefix = %q, want agent-", cfg.GhostPrefix)
	}
	if cfg.AgentsReloadInterval != 5*time.Second {
		t.Errorf("AgentsReloadInterval = %s, want 5s", cfg.AgentsReloadInterval)
	}
	if cfg.AgentCardRefreshInterval != 5*time.Minute {
		t.Errorf("AgentCardRefreshInterval = %s, want 5m", cfg.AgentCardRefreshInterval)
	}
	if cfg.AgentsPath != "/etc/matrix-a2a-bridge/agents/agents.yaml" {
		t.Errorf("AgentsPath = %q", cfg.AgentsPath)
	}
	if cfg.RoomQueueCapacity != 32 || cfg.GlobalQueueCapacity != 256 {
		t.Errorf("queue capacities = (%d, %d), want (32, 256)", cfg.RoomQueueCapacity, cfg.GlobalQueueCapacity)
	}
	if cfg.RateLimitBucketCapacity != 4096 {
		t.Errorf("RateLimitBucketCapacity = %d, want 4096", cfg.RateLimitBucketCapacity)
	}
	if cfg.AppserviceTransactionMaxBytes != 16*1024*1024 {
		t.Errorf("AppserviceTransactionMaxBytes = %d, want 16777216", cfg.AppserviceTransactionMaxBytes)
	}
	if cfg.DelegationClaimInterval != time.Second || cfg.DelegationLeaseDuration != 30*time.Second ||
		cfg.DelegationRetryInitial != time.Second || cfg.DelegationRetryMax != 30*time.Second ||
		cfg.DelegationMaxAttempts != 5 {
		t.Errorf("durable worker defaults = (%s, %s, %s, %s, %d)",
			cfg.DelegationClaimInterval, cfg.DelegationLeaseDuration,
			cfg.DelegationRetryInitial, cfg.DelegationRetryMax, cfg.DelegationMaxAttempts)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("SERVER_NAME", "example.org")
	t.Setenv("HOMESERVER_URL", "http://hs:8008")
	t.Setenv("A2A_BASE_URL", "http://kagent-controller.kagent:8083")
	t.Setenv("A2A_API_KEY", "test-workload-key")
	t.Setenv("LISTEN_PORT", "9999")
	t.Setenv("REQUEST_TIMEOUT", "5s")
	t.Setenv("SHUTDOWN_TIMEOUT", "15s")
	t.Setenv("GHOST_PREFIX", "bot-")
	t.Setenv("AGENTS_RELOAD_INTERVAL", "2s")
	t.Setenv("AGENT_CARD_REFRESH_INTERVAL", "30s")
	t.Setenv("ROOM_QUEUE_CAPACITY", "12")
	t.Setenv("GLOBAL_QUEUE_CAPACITY", "64")
	t.Setenv("RATE_LIMIT_BUCKET_CAPACITY", "128")
	t.Setenv("APPSERVICE_TRANSACTION_MAX_BYTES", "1048576")
	t.Setenv("DELEGATION_CLAIM_INTERVAL", "250ms")
	t.Setenv("DELEGATION_LEASE_DURATION", "12s")
	t.Setenv("DELEGATION_RETRY_INITIAL", "2s")
	t.Setenv("DELEGATION_RETRY_MAX", "20s")
	t.Setenv("DELEGATION_MAX_ATTEMPTS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ServerName != "example.org" {
		t.Errorf("ServerName = %q", cfg.ServerName)
	}
	if cfg.ListenPort != 9999 {
		t.Errorf("ListenPort = %d, want 9999", cfg.ListenPort)
	}
	if cfg.RequestTimeout != 5*time.Second {
		t.Errorf("RequestTimeout = %s, want 5s", cfg.RequestTimeout)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.GhostPrefix != "bot-" {
		t.Errorf("GhostPrefix = %q, want bot-", cfg.GhostPrefix)
	}
	if cfg.A2AAPIKey != "test-workload-key" {
		t.Errorf("A2AAPIKey was not loaded")
	}
	if cfg.AgentsReloadInterval != 2*time.Second {
		t.Errorf("AgentsReloadInterval = %s, want 2s", cfg.AgentsReloadInterval)
	}
	if cfg.AgentCardRefreshInterval != 30*time.Second {
		t.Errorf("AgentCardRefreshInterval = %s, want 30s", cfg.AgentCardRefreshInterval)
	}
	if cfg.RoomQueueCapacity != 12 || cfg.GlobalQueueCapacity != 64 {
		t.Errorf("queue capacities = (%d, %d), want (12, 64)", cfg.RoomQueueCapacity, cfg.GlobalQueueCapacity)
	}
	if cfg.RateLimitBucketCapacity != 128 {
		t.Errorf("RateLimitBucketCapacity = %d, want 128", cfg.RateLimitBucketCapacity)
	}
	if cfg.AppserviceTransactionMaxBytes != 1048576 || cfg.DelegationClaimInterval != 250*time.Millisecond ||
		cfg.DelegationLeaseDuration != 12*time.Second || cfg.DelegationRetryInitial != 2*time.Second ||
		cfg.DelegationRetryMax != 20*time.Second || cfg.DelegationMaxAttempts != 7 {
		t.Errorf("durable overrides were not loaded: %+v", cfg)
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	t.Setenv("LISTEN_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for out-of-range LISTEN_PORT, got nil")
	}
}

func TestLoadStagingRooms(t *testing.T) {
	t.Setenv("STAGING_ROOMS", "!alpha:example.org,!beta:example.org")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.StagingRooms) != 2 || cfg.StagingRooms[0] != "!alpha:example.org" || cfg.StagingRooms[1] != "!beta:example.org" {
		t.Fatalf("StagingRooms = %v", cfg.StagingRooms)
	}
}

func TestValidateRejectsBadStagingRoom(t *testing.T) {
	t.Setenv("STAGING_ROOMS", "not-a-room-id")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for malformed STAGING_ROOMS entry, got nil")
	}
}

func TestLoadMediaDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaMaxBytes != 10*1024*1024 {
		t.Errorf("MediaMaxBytes = %d, want 10 MiB", cfg.MediaMaxBytes)
	}
	if cfg.MediaMaxTotalBytes != 25*1024*1024 {
		t.Errorf("MediaMaxTotalBytes = %d, want 25 MiB", cfg.MediaMaxTotalBytes)
	}
	if len(cfg.MediaMIMEAllowlist) == 0 {
		t.Fatal("default MediaMIMEAllowlist must be non-empty so the demo round-trip works")
	}
	found := false
	for _, m := range cfg.MediaMIMEAllowlist {
		if m == "text/csv" {
			found = true
		}
	}
	if !found {
		t.Errorf("default allowlist should include text/csv, got %v", cfg.MediaMIMEAllowlist)
	}
}

func TestLoadMediaDisableViaZeroCap(t *testing.T) {
	// MEDIA_MAX_BYTES=0 disables the media path even though the default allowlist stays populated.
	t.Setenv("MEDIA_MAX_BYTES", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaMaxBytes != 0 {
		t.Fatalf("MediaMaxBytes = %d, want 0", cfg.MediaMaxBytes)
	}
}

func TestLoadMediaCustomAllowlist(t *testing.T) {
	t.Setenv("MEDIA_MIME_ALLOWLIST", "text/csv,application/pdf")
	t.Setenv("MEDIA_MAX_BYTES", "2048")
	t.Setenv("MEDIA_MAX_TOTAL_BYTES", "4096")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MediaMIMEAllowlist) != 2 || cfg.MediaMaxBytes != 2048 || cfg.MediaMaxTotalBytes != 4096 {
		t.Fatalf("media override not applied: %#v", cfg.MediaMIMEAllowlist)
	}
}

func TestValidateRejectsBadMediaPolicy(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "wildcard mime", env: map[string]string{"MEDIA_MIME_ALLOWLIST": "text/*"}},
		{name: "malformed mime", env: map[string]string{"MEDIA_MIME_ALLOWLIST": "notamime"}},
		{
			name: "total below per-file",
			env:  map[string]string{"MEDIA_MAX_BYTES": "1000", "MEDIA_MAX_TOTAL_BYTES": "500"},
		},
		{name: "negative per-file", env: map[string]string{"MEDIA_MAX_BYTES": "-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load accepted an invalid media policy")
			}
		})
	}
}

func TestValidateRejectsNonPositiveInputWait(t *testing.T) {
	t.Setenv("INPUT_WAIT_TIMEOUT", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive INPUT_WAIT_TIMEOUT, got nil")
	}
}

// validate is exercised directly: caarlos0/env applies envDefault to empty-set variables, so an
// empty SERVER_NAME can only reach validate through a struct, never through the environment.
func TestValidateRejectsEmptyServerName(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with defaults: %v", err)
	}
	cfg.ServerName = ""
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for empty ServerName, got nil")
	}
}

func TestValidateRejectsTaskTimeoutBelowRequestTimeout(t *testing.T) {
	t.Setenv("REQUEST_TIMEOUT", "60s")
	t.Setenv("TASK_TIMEOUT", "30s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for TASK_TIMEOUT < REQUEST_TIMEOUT, got nil")
	}
}

func TestValidateRejectsNonPositiveAgentsReloadInterval(t *testing.T) {
	t.Setenv("AGENTS_RELOAD_INTERVAL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive AGENTS_RELOAD_INTERVAL, got nil")
	}
}

func TestValidateRejectsNonPositiveAgentCardRefreshInterval(t *testing.T) {
	t.Setenv("AGENT_CARD_REFRESH_INTERVAL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive AGENT_CARD_REFRESH_INTERVAL, got nil")
	}
}

func TestValidateRejectsNonPositiveShutdownTimeout(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive SHUTDOWN_TIMEOUT, got nil")
	}
}

func TestValidateRejectsUnsafeQueueCapacities(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "empty room capacity", env: map[string]string{"ROOM_QUEUE_CAPACITY": "0"}},
		{
			name: "global capacity below concurrency",
			env: map[string]string{
				"CONCURRENCY":           "4",
				"GLOBAL_QUEUE_CAPACITY": "3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load accepted unsafe queue capacities")
			}
		})
	}
}

func TestValidateRejectsUnsafeDurableSettings(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "empty transaction body", env: map[string]string{"APPSERVICE_TRANSACTION_MAX_BYTES": "0"}},
		{name: "empty claim interval", env: map[string]string{"DELEGATION_CLAIM_INTERVAL": "0s"}},
		{name: "empty lease", env: map[string]string{"DELEGATION_LEASE_DURATION": "0s"}},
		{name: "sub-nanosecond heartbeat", env: map[string]string{"DELEGATION_LEASE_DURATION": "2ns"}},
		{name: "empty retry", env: map[string]string{"DELEGATION_RETRY_INITIAL": "0s"}},
		{
			name: "reversed retry range",
			env:  map[string]string{"DELEGATION_RETRY_INITIAL": "10s", "DELEGATION_RETRY_MAX": "5s"},
		},
		{name: "empty attempt limit", env: map[string]string{"DELEGATION_MAX_ATTEMPTS": "0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted unsafe durable settings: %v", tt.env)
			}
		})
	}
}

func TestValidateRejectsNonPositiveRateLimitBucketCapacity(t *testing.T) {
	t.Setenv("RATE_LIMIT_BUCKET_CAPACITY", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive RATE_LIMIT_BUCKET_CAPACITY, got nil")
	}
}

func TestLoadRejectsInvalidLoggingConfig(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "level", key: "LOG_LEVEL", value: "bogus", want: `validate environment: LOG_LEVEL "bogus"`},
		{name: "format", key: "LOG_FORMAT", value: "yaml", want: `validate environment: LOG_FORMAT "yaml"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
