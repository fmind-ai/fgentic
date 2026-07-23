// Command matrix-group-sync runs the one-way GitOps reconciler that materializes authoritative
// IdP-group membership into managed Matrix room membership through a scoped access-manager identity
// (docs/adr/0009). It is a self-contained app, deliberately separate from the mautrix bridge: the
// bridge authorizes within already-materialized room state, this controller maintains that state.
// It holds neither a Synapse-admin credential nor a MAS admin token, and audit-only is the default.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fmind-ai/matrix-group-sync/internal/bindings"
	"github.com/fmind-ai/matrix-group-sync/internal/config"
	"github.com/fmind-ai/matrix-group-sync/internal/directory"
	"github.com/fmind-ai/matrix-group-sync/internal/matrix"
	"github.com/fmind-ai/matrix-group-sync/internal/metrics"
	"github.com/fmind-ai/matrix-group-sync/internal/reconcile"
)

func main() {
	if err := run(); err != nil {
		slog.Error("matrix-group-sync exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, err := newLogger(cfg)
	if err != nil {
		return err
	}

	bs, err := bindings.Load(cfg.BindingsPath, cfg.GhostPrefix)
	if err != nil {
		return err
	}

	matrixToken, err := readSecretFile(cfg.MatrixAccessTokenPath)
	if err != nil {
		return err
	}
	clientSecret, err := readSecretFile(cfg.KeycloakClientSecretPath)
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: cfg.RequestTimeout}
	dir := directory.NewKeycloak(cfg.KeycloakBaseURL, cfg.KeycloakRealm, cfg.KeycloakClientID, clientSecret, cfg.KeycloakPageSize, httpClient)
	rooms, err := matrix.NewClient(cfg.MatrixHomeserverURL, cfg.AccessManagerMXID, matrixToken)
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	reconciler := reconcile.New(bs, dir, rooms, m, log, reconcile.Options{
		ServerName:          cfg.ServerName,
		AccessManagerMXID:   cfg.AccessManagerMXID,
		GhostPrefix:         cfg.GhostPrefix,
		Enforce:             cfg.Enforce,
		RevocationSLO:       cfg.RevocationSLO,
		MissedIntervalAlert: cfg.MissedIntervalAlert,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ready := &atomicReady{}
	metricsServer := newInternalServer(cfg, reg, ready)
	srvErr := make(chan error, 1)
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	log.Info("matrix-group-sync started",
		"server_name", cfg.ServerName,
		"access_manager", cfg.AccessManagerMXID,
		"enforce", cfg.Enforce,
		"interval", cfg.ReconcileInterval,
		"bindings", len(bs.All()),
		"metrics", metricsServer.Addr)
	if !cfg.Enforce {
		log.Warn("AUDIT-ONLY mode: diffs are computed and reported but NO Matrix mutation is made (set ENFORCE=true after reviewed room adoption)")
	}

	// Reconcile once immediately, then on the interval. The controller is stateless-from-truth: it
	// derives desired state from the IdP directory and actual state from live Matrix room state, so
	// a restart loses no durable data (no scoped DB role is required).
	ticker := time.NewTicker(cfg.ReconcileInterval)
	defer ticker.Stop()
	runCycle(ctx, reconciler, log, cfg, ready)
	for {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			return metricsServer.Shutdown(shutdownCtx)
		case err := <-srvErr:
			return err
		case <-ticker.C:
			runCycle(ctx, reconciler, log, cfg, ready)
		}
	}
}

func runCycle(ctx context.Context, reconciler *reconcile.Reconciler, log *slog.Logger, cfg config.Config, ready *atomicReady) {
	cycleCtx, cancel := context.WithTimeout(ctx, cfg.ReconcileInterval)
	defer cancel()
	res := reconciler.Reconcile(cycleCtx)
	ready.set(true)
	log.Info("reconcile cycle complete",
		"complete", res.Complete,
		"ambiguous", res.Ambiguous,
		"applied", res.Applied,
		"stalled", res.Stalled,
		"slo_breached", res.SLOBreached,
		"rooms", len(res.Plans))
}

func newInternalServer(cfg config.Config, reg *prometheus.Registry, ready *atomicReady) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.get() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	return &http.Server{
		Addr:              net.JoinHostPort(cfg.MetricsHost, strconv.Itoa(cfg.MetricsPort)),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// atomicReady tracks readiness after the first reconcile cycle without a mutex on the hot path.
type atomicReady struct {
	ready atomic.Bool
}

func (a *atomicReady) set(v bool) { a.ready.Store(v) }
func (a *atomicReady) get() bool  { return a.ready.Load() }

func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", errors.New("secret file " + path + " is empty")
	}
	return value, nil
}

func newLogger(cfg config.Config) (*slog.Logger, error) {
	level, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == config.LogFormatText {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler), nil
}
