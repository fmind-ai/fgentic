// Package config holds the matrix-group-sync reconciler's typed, environment-parsed configuration.
// Every operational value is parsed and validated at the boundary (fail fast); nothing is hardcoded.
//
// The reconciler is a one-way GitOps controller: it reads authoritative IdP-group membership from
// Keycloak and materializes it into managed Matrix room membership through a NARROWLY SCOPED
// access-manager Matrix client identity (docs/adr/0009). It deliberately holds neither a
// Synapse-admin credential nor a MAS `urn:mas:admin` token; both are absent by construction because
// no such value is ever parsed here.
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

// Config is the fully-resolved reconciler configuration.
type Config struct {
	// ServerName is the Matrix homeserver server_name (the domain in every local MXID). The
	// reconciler forms the FULL local MXID `@<matrix_localpart>:<ServerName>` from each IdP member;
	// it never matches a user by localpart alone, so the design stays federation-safe (D6).
	ServerName string `env:"SERVER_NAME,required"`

	// AccessManagerMXID is the scoped Matrix identity that creates and owns every managed room. It
	// is the expected room creator and the only principal kept at full power; the reconciler never
	// revokes it and never kicks agent ghosts. It must be a full MXID on ServerName.
	AccessManagerMXID string `env:"ACCESS_MANAGER_MXID,required"`

	// GhostPrefix marks bridge-owned agent ghosts (`@agent-<name>:<server>`). The reconciler never
	// invites or revokes a ghost: agents are placed in rooms by the bridge, not by IdP groups.
	GhostPrefix string `env:"GHOST_PREFIX" envDefault:"agent-"`

	// MatrixHomeserverURL is the client-server API base the access-manager client dials. Only NORMAL
	// client endpoints are used (resolve alias, room state, profile lookup, invite, kick, ban).
	MatrixHomeserverURL string `env:"MATRIX_HOMESERVER_URL,required"`
	// MatrixAccessTokenPath is the mounted file holding the SOPS-backed access-manager access token.
	// Reading it from a file keeps the secret out of the process environment and out of git.
	MatrixAccessTokenPath string `env:"MATRIX_ACCESS_TOKEN_PATH,required"`

	// KeycloakBaseURL/Realm identify the reference IdP directory. ClientID + the client secret at
	// ClientSecretPath authenticate a client-credentials read client scoped to reading the bound
	// groups, their members, and the administrator-managed `matrix_localpart` attribute — nothing
	// that can mutate identity or issue tokens for a user.
	KeycloakBaseURL          string `env:"KEYCLOAK_BASE_URL,required"`
	KeycloakRealm            string `env:"KEYCLOAK_REALM,required"`
	KeycloakClientID         string `env:"KEYCLOAK_CLIENT_ID,required"`
	KeycloakClientSecretPath string `env:"KEYCLOAK_CLIENT_SECRET_PATH,required"`
	// KeycloakPageSize bounds one members page; the directory read pages the whole group and only a
	// COMPLETE, successful traversal is eligible to drive mutations (D6 partial-read fail-closed).
	KeycloakPageSize int `env:"KEYCLOAK_PAGE_SIZE" envDefault:"100"`

	// BindingsPath is the projected, git-declared ConfigMap mapping each exact Keycloak group path
	// to one managed room and its explicit agent set (docs/adr/0009). It is the sole source of which
	// rooms this reconciler manages; a room absent from it is unmanaged and never granted into.
	BindingsPath string `env:"BINDINGS_PATH" envDefault:"/etc/matrix-group-sync/bindings/bindings.yaml"`

	// Enforce turns off audit-only mode. Audit-only (the DEFAULT) computes and reports every
	// membership diff but performs NO Matrix mutation and raises NO revocation-SLO alert. Real
	// invites, kicks, and the SLO alert enable together only when this is explicitly true, after
	// reviewed room adoption (docs/adr/0009 rollout gate).
	Enforce bool `env:"ENFORCE" envDefault:"false"`

	// ReconcileInterval is how often the full desired set is reconciled from a complete directory
	// snapshot. RevocationSLO bounds how long a computed revocation may remain unapplied before the
	// SLO-breach alert fires (enforce mode only). MissedIntervalAlert is how many consecutive
	// incomplete/ambiguous cycles raise the stall alert.
	ReconcileInterval   time.Duration `env:"RECONCILE_INTERVAL" envDefault:"60s"`
	RevocationSLO       time.Duration `env:"REVOCATION_SLO" envDefault:"2m"`
	MissedIntervalAlert int           `env:"MISSED_INTERVAL_ALERT" envDefault:"2"`

	// RequestTimeout bounds one directory or Matrix client round trip. A timeout is a partial read:
	// it retains last-known Matrix state and creates no grants or removals.
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"30s"`
	// ShutdownTimeout is the graceful drain budget on SIGTERM.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"20s"`

	// MetricsPort serves Prometheus /metrics plus /healthz and /readyz. There is NO public surface:
	// the reconciler only reads the IdP and drives Matrix, so nothing untrusted reaches it.
	MetricsHost string `env:"METRICS_HOST" envDefault:"0.0.0.0"`
	MetricsPort int    `env:"METRICS_PORT" envDefault:"9090"`

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
	if strings.TrimSpace(c.ServerName) == "" {
		return fmt.Errorf("SERVER_NAME must not be empty")
	}
	if err := validateFullMXID(c.AccessManagerMXID, c.ServerName); err != nil {
		return fmt.Errorf("ACCESS_MANAGER_MXID: %w", err)
	}
	if c.GhostPrefix == "" || c.GhostPrefix != strings.TrimSpace(c.GhostPrefix) {
		return fmt.Errorf("GHOST_PREFIX must be a non-empty, unpadded prefix")
	}
	if !hasHTTPScheme(c.MatrixHomeserverURL) {
		return fmt.Errorf("MATRIX_HOMESERVER_URL %q must be an absolute http(s) URL", c.MatrixHomeserverURL)
	}
	if c.MatrixAccessTokenPath == "" {
		return fmt.Errorf("MATRIX_ACCESS_TOKEN_PATH must not be empty")
	}
	if !hasHTTPScheme(c.KeycloakBaseURL) {
		return fmt.Errorf("KEYCLOAK_BASE_URL %q must be an absolute http(s) URL", c.KeycloakBaseURL)
	}
	if strings.TrimSpace(c.KeycloakRealm) == "" {
		return fmt.Errorf("KEYCLOAK_REALM must not be empty")
	}
	if strings.TrimSpace(c.KeycloakClientID) == "" {
		return fmt.Errorf("KEYCLOAK_CLIENT_ID must not be empty")
	}
	if c.KeycloakClientSecretPath == "" {
		return fmt.Errorf("KEYCLOAK_CLIENT_SECRET_PATH must not be empty")
	}
	if c.KeycloakPageSize < 1 {
		return fmt.Errorf("KEYCLOAK_PAGE_SIZE must be at least 1")
	}
	if c.BindingsPath == "" {
		return fmt.Errorf("BINDINGS_PATH must not be empty")
	}
	if c.ReconcileInterval <= 0 {
		return fmt.Errorf("RECONCILE_INTERVAL must be positive")
	}
	if c.RevocationSLO <= 0 {
		return fmt.Errorf("REVOCATION_SLO must be positive")
	}
	if c.MissedIntervalAlert < 1 {
		return fmt.Errorf("MISSED_INTERVAL_ALERT must be at least 1")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("REQUEST_TIMEOUT must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be positive")
	}
	if c.MetricsPort < 1 || c.MetricsPort > 65535 {
		return fmt.Errorf("METRICS_PORT %d out of range 1-65535", c.MetricsPort)
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

func hasHTTPScheme(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// validateFullMXID checks that the access-manager identity is a full MXID on the local server,
// never a bare localpart — the same federation-safe rule the reconciler applies to every member.
func validateFullMXID(mxid, serverName string) error {
	if !strings.HasPrefix(mxid, "@") {
		return fmt.Errorf("%q must be a full MXID starting with '@'", mxid)
	}
	local, server, ok := strings.Cut(mxid[1:], ":")
	if !ok || local == "" || server == "" {
		return fmt.Errorf("%q must be a full MXID '@localpart:server'", mxid)
	}
	if server != serverName {
		return fmt.Errorf("%q must be on the local server %q, not %q", mxid, serverName, server)
	}
	return nil
}
