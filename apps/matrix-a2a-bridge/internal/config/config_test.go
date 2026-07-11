package config

import (
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
	if cfg.AgentsPath != "/etc/matrix-a2a-bridge/agents/agents.yaml" {
		t.Errorf("AgentsPath = %q", cfg.AgentsPath)
	}
	if cfg.RoomQueueCapacity != 32 || cfg.GlobalQueueCapacity != 256 {
		t.Errorf("queue capacities = (%d, %d), want (32, 256)", cfg.RoomQueueCapacity, cfg.GlobalQueueCapacity)
	}
	if cfg.RateLimitBucketCapacity != 4096 {
		t.Errorf("RateLimitBucketCapacity = %d, want 4096", cfg.RateLimitBucketCapacity)
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
	t.Setenv("ROOM_QUEUE_CAPACITY", "12")
	t.Setenv("GLOBAL_QUEUE_CAPACITY", "64")
	t.Setenv("RATE_LIMIT_BUCKET_CAPACITY", "128")

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
	if cfg.RoomQueueCapacity != 12 || cfg.GlobalQueueCapacity != 64 {
		t.Errorf("queue capacities = (%d, %d), want (12, 64)", cfg.RoomQueueCapacity, cfg.GlobalQueueCapacity)
	}
	if cfg.RateLimitBucketCapacity != 128 {
		t.Errorf("RateLimitBucketCapacity = %d, want 128", cfg.RateLimitBucketCapacity)
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	t.Setenv("LISTEN_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for out-of-range LISTEN_PORT, got nil")
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

func TestValidateRejectsNonPositiveRateLimitBucketCapacity(t *testing.T) {
	t.Setenv("RATE_LIMIT_BUCKET_CAPACITY", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive RATE_LIMIT_BUCKET_CAPACITY, got nil")
	}
}
