// Command bridge is the Matrix <-> A2A bridge: a Matrix Application Service that lets humans
// (and other agents) @mention an AI agent in a Matrix room and delegates the task to that
// agent's A2A endpoint (SendMessage, with GetTask polling for long-running tasks), posting
// the reply back into the room.
//
// It owns the @agent-* ghost-user namespace on the homeserver, so every kagent agent appears
// as a first-class room member (@agent-k8s:fgentic.fmind.ai, ...). See the package READMEs and the
// repo docs/ for the full design.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Postgres driver for dbutil (mautrix SQL StateStore + the bridge's own state tables).
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/bridge"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/matrixapp"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/sessioncontrol"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/telemetry"
)

const (
	appserviceShutdownTimeout = 5 * time.Second
	appserviceReadTimeout     = 30 * time.Second
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
	log, err := newLogger(cfg)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("configure logger", "err", err)
		os.Exit(1)
	}

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
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	shutdownTelemetry, err := telemetry.Setup(runtimeCtx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			log.Error("shutdown telemetry", "err", err)
		}
	}()

	store, stateStore, closeDB, err := openState(runtimeCtx, cfg, log)
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
	agents.LogSchemaVersionWarning(log, cfg.AgentsPath)
	log.Info("loaded agent routing map", "agents", agents.Names())

	client := a2aclient.New(cfg.A2ABaseURL, cfg.A2AAPIKey, log)
	if cfg.FediverseBrokerURL != "" {
		if err := client.UseFediverseBroker(cfg.FediverseBrokerURL, cfg.FediverseBrokerToken); err != nil {
			return err
		}
	}
	purger, err := sessioncontrol.New(cfg.KagentAPIURL, &http.Client{Timeout: cfg.RequestTimeout})
	if err != nil {
		return fmt.Errorf("configure kagent session deletion: %w", err)
	}
	br := bridge.New(cfg, as, agents, client, store, log)
	br.SetSessionPurger(purger)
	br.EnableDurableIntake()
	if err := br.Start(runtimeCtx); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}

	ep := newEventProcessor(as, br.HandleMessage, br.HandleMembership, br.HandleReaction)
	ep.Start(runtimeCtx)

	// Prometheus metrics on a side port (docs/observability.md §9.3), never on the appservice listener.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.MetricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server failed", "err", err)
		}
	}()

	intake, err := matrixapp.NewTransactionIntake(
		as,
		matrixapp.TransactionAcceptorFunc(func(
			ctx context.Context,
			transactionID string,
			_ [sha256.Size]byte,
			body []byte,
		) (matrixapp.TransactionDisposition, error) {
			result, err := br.AdmitAppserviceTransaction(ctx, transactionID, body)
			if errors.Is(err, state.ErrTransactionHashConflict) {
				return matrixapp.TransactionConflict, nil
			}
			if err != nil {
				return 0, err
			}
			switch result.Disposition {
			case state.TransactionAccepted:
				return matrixapp.TransactionAccepted, nil
			case state.TransactionReplay:
				return matrixapp.TransactionReplay, nil
			default:
				return 0, fmt.Errorf("unsupported state transaction disposition %d", result.Disposition)
			}
		}),
		as.Router,
		cfg.AppserviceTransactionMaxBytes,
		matrixapp.WithTransactionConsumptionBarrier(
			br.HoldDurableExecutionUntilTransactionConsumed,
			br.NotifyDurableQueue,
		),
	)
	if err != nil {
		return fmt.Errorf("configure durable appservice intake: %w", err)
	}
	appserviceSrv := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenHost, fmt.Sprintf("%d", cfg.ListenPort)),
		Handler:           intake,
		ReadHeaderTimeout: 5 * time.Second,
		// Intake is intentionally serialized before allocating the bounded body. Bound the complete
		// authenticated read so one slow sender cannot hold that global ordering slot indefinitely.
		ReadTimeout: appserviceReadTimeout,
	}
	appserviceErr := serveHTTPServer(appserviceSrv)

	// The bridge-owned HTTP server receives homeserver AS transactions. Owning the server is
	// necessary so a timed-out graceful shutdown can force-close transaction connections before
	// the event-processor barrier is inserted.
	// Ready gates mautrix's /_matrix/mau/ready (the Deployment readiness probe): everything is
	// wired at this point — flip it before serving so the pod is routable.
	as.Ready = true
	log.Info("matrix-a2a-bridge started",
		"listen", cfg.ListenHost, "port", cfg.ListenPort,
		"homeserver", cfg.HomeserverURL, "server_name", cfg.ServerName,
		"a2a_base_url", cfg.A2ABaseURL, "persistent_state", cfg.DatabaseURL != "")

	var exitErr error
	appserviceDone := false
	select {
	case <-signalCtx.Done():
	case err := <-appserviceErr:
		appserviceDone = true
		if err == nil {
			err = errors.New("appservice HTTP server stopped unexpectedly")
		}
		exitErr = fmt.Errorf("serve appservice HTTP: %w", err)
		log.Error("appservice HTTP server stopped", "err", err)
	}
	log.Info("stopping matrix-a2a-bridge")
	_ = metricsSrv.Close()
	as.Ready = false
	if !appserviceDone {
		forced, shutdownErr := shutdownHTTPServer(appserviceSrv, appserviceShutdownTimeout)
		if forced {
			// Close severs every active transaction connection, so a handler that later reaches
			// its response cannot successfully ACK events produced after the barrier.
			log.Warn("appservice HTTP graceful shutdown failed; force-closed active connections")
		}
		if shutdownErr != nil {
			exitErr = errors.Join(exitErr, fmt.Errorf("shutdown appservice HTTP: %w", shutdownErr))
		}
		if err := <-appserviceErr; err != nil {
			exitErr = errors.Join(exitErr, fmt.Errorf("serve appservice HTTP during shutdown: %w", err))
		}
	}
	drainedWithinGrace, err := drainEventProcessor(ep, cancelRuntime, 5*time.Second)
	if err != nil {
		log.Error("drain Matrix event processor", "err", err)
	}
	if !drainedWithinGrace {
		log.Warn("Matrix event drain grace exceeded; cancelled delegations while waiting for acknowledged events")
	}
	ep.Stop()
	if !drainBridge(br, cancelRuntime, cfg.ShutdownTimeout) {
		log.Warn(
			"bridge drain deadline exceeded; cancelled remaining delegations",
			"timeout", cfg.ShutdownTimeout,
		)
	}
	cancelRuntime()
	return exitErr
}

type eventProcessorDrainer interface {
	Drain(context.Context) error
}

// drainEventProcessor never abandons the barrier: acknowledged Matrix events must finish
// classification before the processor stops. The grace only controls when delegation work is
// cancelled to unblock a slow handler; it does not bound or duplicate the Drain operation.
func drainEventProcessor(
	drainer eventProcessorDrainer,
	cancelRuntime context.CancelFunc,
	grace time.Duration,
) (withinGrace bool, err error) {
	done := make(chan error, 1)
	go func() {
		done <- drainer.Drain(context.Background())
	}()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		return true, err
	case <-timer.C:
		cancelRuntime()
		return false, <-done
	}
}

func serveHTTPServer(server *http.Server) <-chan error {
	done := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	return done
}

// shutdownHTTPServer stops new intake and waits for accepted handlers. On any Shutdown failure,
// Close prevents a still-running transaction from successfully ACKing before main inserts the
// event-channel barrier; the homeserver will retry such a transaction on the replacement pod.
type httpShutdownServer interface {
	Shutdown(context.Context) error
	Close() error
}

func shutdownHTTPServer(server httpShutdownServer, timeout time.Duration) (forced bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := server.Shutdown(ctx)
	if shutdownErr == nil {
		return false, nil
	}
	closeErr := server.Close()
	if errors.Is(closeErr, http.ErrServerClosed) {
		closeErr = nil
	}
	if errors.Is(shutdownErr, context.DeadlineExceeded) || errors.Is(shutdownErr, context.Canceled) {
		if closeErr != nil {
			return true, fmt.Errorf("force close after graceful shutdown timeout: %w", closeErr)
		}
		return true, nil
	}
	return true, errors.Join(
		fmt.Errorf("graceful shutdown: %w", shutdownErr),
		closeErr,
	)
}

type bridgeStopper interface {
	Stop()
}

// drainBridge gives accepted work a bounded grace period with its runtime context still live.
// On expiry it cancels running work and lets the dispatcher emit drop callbacks for queued work,
// then waits for all callbacks and terminal audits before process resources are released.
func drainBridge(stopper bridgeStopper, cancelRuntime context.CancelFunc, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		stopper.Stop()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		cancelRuntime()
		<-done
		return false
	}
}

var shutdownBarrierEventType = event.Type{
	Type:  "com.fgentic.matrix_a2a_bridge.shutdown_barrier",
	Class: event.MessageEventType,
}

type drainingEventProcessor struct {
	*appservice.EventProcessor
	as      *appservice.AppService
	barrier *event.Event
	drained chan struct{}
}

// Drain places an identity-checked sentinel behind all events produced before the bridge-owned
// HTTP server finished shutdown. Synchronous handler mode makes observing it proof that those
// earlier events have finished classification and enqueueing.
func (ep *drainingEventProcessor) Drain(ctx context.Context) error {
	select {
	case ep.as.Events <- ep.barrier:
	case <-ctx.Done():
		return fmt.Errorf("enqueue shutdown barrier: %w", ctx.Err())
	}
	select {
	case <-ep.drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for shutdown barrier: %w", ctx.Err())
	}
}

func newEventProcessor(
	as *appservice.AppService,
	messageHandler, membershipHandler, reactionHandler appservice.EventHandler,
) *drainingEventProcessor {
	ep := appservice.NewEventProcessor(as)
	// The bridge handlers only classify/enqueue ordinary messages. Running them synchronously
	// preserves the homeserver's event order before jobs enter the per-room FIFO dispatcher;
	// mautrix's default one-goroutine-per-event mode can race two events from the same room.
	ep.ExecMode = appservice.Sync
	ep.On(event.EventMessage, messageHandler)
	// Invites to the bot/ghosts must be accepted for Synapse to deliver room traffic at all.
	ep.On(event.StateMember, membershipHandler)
	// Reactions are only inspected as a cancel gesture on an in-flight task placeholder (#98); the
	// handler never invokes an agent, so running it in event order is cheap and non-blocking.
	ep.On(event.EventReaction, reactionHandler)
	barrier := &event.Event{Type: shutdownBarrierEventType}
	drained := make(chan struct{})
	ep.On(shutdownBarrierEventType, func(_ context.Context, evt *event.Event) {
		// A remote event can copy the type string, but never this in-process pointer.
		if evt == barrier {
			close(drained)
		}
	})
	return &drainingEventProcessor{
		EventProcessor: ep,
		as:             as,
		barrier:        barrier,
		drained:        drained,
	}
}

// openState builds the bridge's state layer. With DATABASE_URL set, one shared Postgres pool
// backs both the mautrix SQL StateStore and the bridge's own tables (docs/bridge.md §5); without it,
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

// newLogger builds the validated structured logger (JSON by default, text on request).
func newLogger(cfg config.Config) (*slog.Logger, error) {
	level, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch cfg.LogFormat {
	case config.LogFormatText:
		handler = slog.NewTextHandler(os.Stdout, opts)
	case config.LogFormatJSON:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		return nil, fmt.Errorf("unsupported LOG_FORMAT %q", cfg.LogFormat)
	}
	return slog.New(handler), nil
}
