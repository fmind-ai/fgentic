package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("SERVER_NAME", "fgentic.localhost")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicHost != "fgentic.localhost" {
		t.Errorf("PublicHost = %q, want default to ServerName", cfg.PublicHost)
	}
	if got := cfg.PublicBaseURL(); got != "https://fgentic.localhost" {
		t.Errorf("PublicBaseURL = %q", got)
	}
	if cfg.ListenPort == cfg.MetricsPort {
		t.Errorf("listen and metrics ports must differ by default")
	}
}

func TestLoadPublicHostOverride(t *testing.T) {
	t.Setenv("SERVER_NAME", "fgentic.localhost")
	t.Setenv("PUBLIC_HOST", "a2a.fgentic.localhost")
	t.Setenv("PUBLIC_SCHEME", "http")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.PublicBaseURL(); got != "http://a2a.fgentic.localhost" {
		t.Errorf("PublicBaseURL = %q", got)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	base := func() Config {
		return Config{
			ServerName: "s", PublicScheme: "https", PublicHost: "h",
			A2ABaseURL: "http://gw", AgentsPath: "/a", GhostPrefix: "agent-",
			ListenPort: 8480, MetricsPort: 9090,
			RequestTimeout: time.Minute, TaskTimeout: 10 * time.Minute,
			ShutdownTimeout: time.Second, LogLevel: "info", LogFormat: "json",
			PolicyReloadInterval: 5 * time.Second, SignatureMaxSkew: 12 * time.Hour,
			IntegrityKeyFragment: "ed25519-key",
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("baseline config should be valid: %v", err)
	}

	tests := map[string]func(*Config){
		"empty server":   func(c *Config) { c.ServerName = "" },
		"bad scheme":     func(c *Config) { c.PublicScheme = "ftp" },
		"empty public":   func(c *Config) { c.PublicHost = "" },
		"empty a2a":      func(c *Config) { c.A2ABaseURL = "" },
		"empty agents":   func(c *Config) { c.AgentsPath = "" },
		"padded prefix":  func(c *Config) { c.GhostPrefix = " agent-" },
		"bad listen":     func(c *Config) { c.ListenPort = 0 },
		"bad metrics":    func(c *Config) { c.MetricsPort = 70000 },
		"same ports":     func(c *Config) { c.MetricsPort = c.ListenPort },
		"bad request":    func(c *Config) { c.RequestTimeout = 0 },
		"task below req": func(c *Config) { c.TaskTimeout = time.Second },
		"bad shutdown":   func(c *Config) { c.ShutdownTimeout = 0 },
		"bad reload":     func(c *Config) { c.PolicyReloadInterval = 0 },
		"bad skew":       func(c *Config) { c.SignatureMaxSkew = 0 },
		"empty fragment": func(c *Config) { c.IntegrityKeyFragment = "" },
		"fragment hash":  func(c *Config) { c.IntegrityKeyFragment = "key#extra" },
		"inbound no pol": func(c *Config) { c.IntegrityRequireInbound = true; c.PolicyPath = "" },
		"bad level":      func(c *Config) { c.LogLevel = "loud" },
		"bad format":     func(c *Config) { c.LogFormat = "xml" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Errorf("expected validation error")
			}
		})
	}
}

func TestSlogLevel(t *testing.T) {
	cfg := Config{LogLevel: "warn"}
	level, err := cfg.SlogLevel()
	if err != nil {
		t.Fatalf("SlogLevel: %v", err)
	}
	if level != slog.LevelWarn {
		t.Errorf("level = %v, want warn", level)
	}
}
