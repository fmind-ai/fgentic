// Package matrixapp constructs the mautrix Application Service the bridge runs on, and provides
// a bootstrap helper to generate the registration file the homeserver must load.
package matrixapp

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/appservice"

	"github.com/fmind/matrix-a2a-bridge/internal/config"
)

// New loads the appservice registration and builds a ready-to-start AppService. The registration
// file is the source of truth for the as_token/hs_token — they MUST match the homeserver's copy.
// stateStore may be nil (in-memory default, dev only); production passes the Postgres-backed
// mautrix SQL StateStore so ghost registrations/memberships survive pod restarts (SPEC §5).
func New(cfg config.Config, stateStore appservice.StateStore) (*appservice.AppService, error) {
	reg, err := appservice.LoadRegistration(cfg.RegistrationPath)
	if err != nil {
		return nil, fmt.Errorf("load registration %q: %w", cfg.RegistrationPath, err)
	}
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     reg,
		HomeserverDomain: cfg.ServerName,
		HomeserverURL:    cfg.HomeserverURL,
		HostConfig:       appservice.HostConfig{Hostname: cfg.ListenHost, Port: uint16(cfg.ListenPort)},
	})
	if err != nil {
		return nil, fmt.Errorf("create appservice: %w", err)
	}
	if stateStore != nil {
		as.StateStore = stateStore
	}
	// mautrix logs via zerolog; keep it JSON on stdout to sit alongside our slog output.
	as.Log = zerolog.New(os.Stdout).With().Timestamp().Logger()
	return as, nil
}

// GenerateRegistration writes a fresh appservice registration.yaml (id, generated tokens, and the
// @a2a-bridge bot + @<prefix>.* ghost namespaces) to RegistrationPath. Run once at bootstrap, then
// hand the same file to both the bridge and the homeserver (ESS/Synapse app_service_config_files).
func GenerateRegistration(cfg config.Config, log *slog.Logger) error {
	reg := appservice.CreateRegistration() // random as_token/hs_token
	reg.ID = "matrix-a2a-bridge"
	reg.SenderLocalpart = "a2a-bridge"
	reg.URL = fmt.Sprintf("http://matrix-a2a-bridge.bridge.svc.cluster.local:%d", cfg.ListenPort)
	// Ghost replies must not be throttled by the homeserver's per-user message rate limits
	// (SPEC §4 F3): the bridge enforces its own invocation rate limits instead.
	rateLimited := false
	reg.RateLimited = &rateLimited

	domain := regexp.QuoteMeta(cfg.ServerName)
	botRegex := regexp.MustCompile(fmt.Sprintf(`@a2a-bridge:%s`, domain))
	ghostRegex := regexp.MustCompile(fmt.Sprintf(`@%s.*:%s`, regexp.QuoteMeta(cfg.GhostPrefix), domain))
	reg.Namespaces.UserIDs.Register(botRegex, true)   // exclusive: the bridge owns the bot user
	reg.Namespaces.UserIDs.Register(ghostRegex, true) // exclusive: the bridge owns all @agent-* ghosts

	if err := reg.Save(cfg.RegistrationPath); err != nil {
		return fmt.Errorf("save registration %q: %w", cfg.RegistrationPath, err)
	}
	log.Info("generated appservice registration",
		"path", cfg.RegistrationPath,
		"id", reg.ID,
		"sender", reg.SenderLocalpart,
		"ghost_namespace", ghostRegex.String())
	return nil
}
