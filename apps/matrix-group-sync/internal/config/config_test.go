package config

import (
	"testing"
	"time"
)

func base(t *testing.T) {
	t.Helper()
	t.Setenv("SERVER_NAME", "fgentic.localhost")
	t.Setenv("ACCESS_MANAGER_MXID", "@access-manager:fgentic.localhost")
	t.Setenv("MATRIX_HOMESERVER_URL", "http://ess-synapse.matrix:8008")
	t.Setenv("MATRIX_ACCESS_TOKEN_PATH", "/etc/secret/token")
	t.Setenv("KEYCLOAK_BASE_URL", "http://keycloak.keycloak:8080")
	t.Setenv("KEYCLOAK_REALM", "fgentic")
	t.Setenv("KEYCLOAK_CLIENT_ID", "matrix-group-sync")
	t.Setenv("KEYCLOAK_CLIENT_SECRET_PATH", "/etc/secret/client")
}

func TestLoadValidDefaults(t *testing.T) {
	base(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Enforce {
		t.Fatal("audit-only must be the default (Enforce=false)")
	}
	if c.ReconcileInterval != time.Minute || c.RevocationSLO != 2*time.Minute {
		t.Fatalf("unexpected SLO defaults: %v %v", c.ReconcileInterval, c.RevocationSLO)
	}
	if c.MissedIntervalAlert != 2 {
		t.Fatalf("expected 2 missed-interval alert threshold")
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"missing server": func(t *testing.T) { base(t); t.Setenv("SERVER_NAME", "") },
		"bare access-manager": func(t *testing.T) {
			base(t)
			t.Setenv("ACCESS_MANAGER_MXID", "access-manager")
		},
		"foreign access-manager": func(t *testing.T) {
			base(t)
			t.Setenv("ACCESS_MANAGER_MXID", "@access-manager:other.example")
		},
		"non-http homeserver": func(t *testing.T) {
			base(t)
			t.Setenv("MATRIX_HOMESERVER_URL", "ess-synapse:8008")
		},
		"non-http keycloak": func(t *testing.T) {
			base(t)
			t.Setenv("KEYCLOAK_BASE_URL", "keycloak")
		},
		"bad log format":   func(t *testing.T) { base(t); t.Setenv("LOG_FORMAT", "xml") },
		"bad log level":    func(t *testing.T) { base(t); t.Setenv("LOG_LEVEL", "loud") },
		"zero interval":    func(t *testing.T) { base(t); t.Setenv("RECONCILE_INTERVAL", "0s") },
		"zero slo":         func(t *testing.T) { base(t); t.Setenv("REVOCATION_SLO", "0s") },
		"zero request":     func(t *testing.T) { base(t); t.Setenv("REQUEST_TIMEOUT", "0s") },
		"zero shutdown":    func(t *testing.T) { base(t); t.Setenv("SHUTDOWN_TIMEOUT", "0s") },
		"missed alert 0":   func(t *testing.T) { base(t); t.Setenv("MISSED_INTERVAL_ALERT", "0") },
		"page size 0":      func(t *testing.T) { base(t); t.Setenv("KEYCLOAK_PAGE_SIZE", "0") },
		"bad metrics port": func(t *testing.T) { base(t); t.Setenv("METRICS_PORT", "70000") },
		"empty ghost":      func(t *testing.T) { base(t); t.Setenv("GHOST_PREFIX", " ") },
		"empty realm":      func(t *testing.T) { base(t); t.Setenv("KEYCLOAK_REALM", "") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			setup(t)
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestLoadMissingRequired(t *testing.T) {
	// No environment set at all: required fields must fail.
	if _, err := Load(); err == nil {
		t.Fatal("expected missing required fields to fail")
	}
}
