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
	if cfg.GhostPrefix != "agent-" {
		t.Errorf("GhostPrefix = %q, want agent-", cfg.GhostPrefix)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("SERVER_NAME", "example.org")
	t.Setenv("HOMESERVER_URL", "http://hs:8008")
	t.Setenv("A2A_BASE_URL", "http://kagent-controller.kagent:8083")
	t.Setenv("LISTEN_PORT", "9999")
	t.Setenv("REQUEST_TIMEOUT", "5s")
	t.Setenv("GHOST_PREFIX", "bot-")

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
	if cfg.GhostPrefix != "bot-" {
		t.Errorf("GhostPrefix = %q, want bot-", cfg.GhostPrefix)
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
