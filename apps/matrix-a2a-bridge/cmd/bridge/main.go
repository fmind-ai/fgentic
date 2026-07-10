// Command bridge is the Matrix <-> A2A bridge: a Matrix Application Service that lets humans
// (and other agents) @mention an AI agent in a Matrix room and delegates the task to that
// agent's A2A endpoint (message/send, with tasks/get polling for long-running tasks), posting
// the reply back into the room.
//
// It owns the @agent-* ghost-user namespace on the homeserver, so every kagent agent appears
// as a first-class room member (@agent-k8s:fgentic.fmind.ai, ...). See the package READMEs and the
// repo PLAN.md / SPEC.md for the full design.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	// Postgres driver for dbutil (mautrix SQL StateStore + the bridge's own state tables).
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind/matrix-a2a-bridge/internal/bridge"
	"github.com/fmind/matrix-a2a-bridge/internal/config"
	"github.com/fmind/matrix-a2a-bridge/internal/matrixapp"
	"github.com/fmind/matrix-a2a-bridge/internal/state"
)

func main() {
	genReg := flag.Bool("generate-registration", false,
		"generate the appservice registration.yaml at REGISTRATION_PATH and exit (bootstrap helper)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		// No logger yet — fail fast to stderr.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("load config", "err", err)
		os.Exit(1)
	}
	log := newLogger(cfg)

	if *genReg {
		if err := matrixapp.GenerateRegistration(cfg, log); err != nil {
			log.Error("generate registration", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := run(cfg, log); err != nil {
		log.Error("bridge exited with error", "err", err)
		os.Exit(1)
	}
}

// run wires the appservice, the persistent state, the agent routing map, and the A2A client
// together, starts the HTTP transaction server + event loop, and blocks until signalled.
func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, stateStore, closeDB, err := openState(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeDB()

	as, err := matrixapp.New(cfg, stateStore)
	if err != nil {
		return err
	}

	agents, err := bridge.LoadAgents(cfg.AgentsPath)
	if err != nil {
		return err
	}
	log.Info("loaded agent routing map", "agents", agents.Names())

	client := a2aclient.New(cfg.A2ABaseURL, log)
	br := bridge.New(cfg, as, agents, client, store, log)
	br.Start(ctx)

	ep := appservice.NewEventProcessor(as)
	ep.On(event.EventMessage, br.HandleMessage)
	// Invites to the bot/ghosts must be accepted for Synapse to deliver room traffic at all.
	ep.On(event.StateMember, br.HandleMembership)
	ep.Start(ctx)

	// as.Start() runs the blocking HTTP server that receives homeserver AS transactions.
	// Ready gates mautrix's /_matrix/mau/ready (the Deployment readiness probe): everything is
	// wired at this point — flip it before serving so the pod is routable.
	as.Ready = true
	go as.Start()
	log.Info("matrix-a2a-bridge started",
		"listen", cfg.ListenHost, "port", cfg.ListenPort,
		"homeserver", cfg.HomeserverURL, "server_name", cfg.ServerName,
		"a2a_base_url", cfg.A2ABaseURL, "persistent_state", cfg.DatabaseURL != "")

	<-ctx.Done()
	log.Info("shutdown signal received, stopping")
	as.Stop()
	br.Stop() // drain in-flight delegations before releasing the database
	return nil
}

// openState builds the bridge's state layer. With DATABASE_URL set, one shared Postgres pool
// backs both the mautrix SQL StateStore and the bridge's own tables (SPEC §5); without it,
// everything is in-memory (dev only — restarts lose threading and dedup).
func openState(ctx context.Context, cfg config.Config, log *slog.Logger) (state.Store, appservice.StateStore, func(), error) {
	if cfg.DatabaseURL == "" {
		log.Warn("DATABASE_URL not set — using in-memory state (dev only)")
		return state.NewMemory(), nil, func() {}, nil
	}
	db, err := dbutil.NewWithDialect(cfg.DatabaseURL, "pgx")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open bridge database: %w", err)
	}
	db.Owner = "matrix-a2a-bridge"
	closeDB := func() {
		if err := db.Close(); err != nil {
			log.Error("close bridge database", "err", err)
		}
	}

	stateStore := sqlstatestore.NewSQLStateStore(db, dbutil.NoopLogger, false)
	if err := stateStore.Upgrade(ctx); err != nil {
		closeDB()
		return nil, nil, nil, fmt.Errorf("upgrade mautrix state store schema: %w", err)
	}
	store, err := state.NewPostgres(ctx, db)
	if err != nil {
		closeDB()
		return nil, nil, nil, err
	}
	return store, stateStore, closeDB, nil
}

// newLogger builds the structured logger from config (JSON by default, text on request).
func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
