// Command gateway runs the ActivityPub agent gateway: it serves each exposed platform agent as an
// AP Service actor and delegates inbound mentions to kagent over A2A through agentgateway. It is a
// self-contained app, deliberately NOT part of the mautrix bridge, so the bridge stays AGPL-free
// and homeserver-portable and no agent holds a model credential (docs/adr/0014, docs/fediverse.md).
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
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fmind/activitypub-agent-gateway/internal/a2a"
	"github.com/fmind/activitypub-agent-gateway/internal/apgateway"
	"github.com/fmind/activitypub-agent-gateway/internal/budget"
	"github.com/fmind/activitypub-agent-gateway/internal/config"
	"github.com/fmind/activitypub-agent-gateway/internal/delivery"
	"github.com/fmind/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind/activitypub-agent-gateway/internal/policy"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway exited with error", "error", err)
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

	registry, err := apgateway.LoadRegistry(cfg.AgentsPath, cfg.GhostPrefix)
	if err != nil {
		return err
	}
	delegator := a2a.New(cfg.A2ABaseURL, cfg.A2AAPIKey, cfg.RequestTimeout, cfg.TaskTimeout, log)

	reg := prometheus.NewRegistry()
	gateway, err := apgateway.New(cfg.PublicBaseURL(), cfg.ServerName, registry, delegator, reg, log)
	if err != nil {
		return err
	}
	gateway.SetA2APublicBase(cfg.A2APublicBaseURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Object integrity signing (FEP-8b32): when a key is mounted, outbound replies carry an
	// eddsa-jcs-2022 proof and each actor publishes its assertionMethod Multikey. An empty
	// INTEGRITY_KEY_PATH serves replies without a proof — valid for local-only dev. The same key is
	// reused for Group HTTP-Signature delivery (one platform identity).
	var signer *integrity.Signer
	if cfg.IntegrityKeyPath != "" {
		signer, err = integrity.LoadSignerFromFile(cfg.IntegrityKeyPath, cfg.IntegrityKeyFragment)
		if err != nil {
			return err
		}
		gateway.UseSigner(signer)
		log.Info("object integrity signing enabled", "key", cfg.IntegrityKeyPath, "fragment", signer.KeyFragment())
	} else {
		log.Warn("object integrity signing DISABLED (INTEGRITY_KEY_PATH empty) — replies carry no FEP-8b32 proof")
	}

	// The federation policy border. When a policy is configured, every inbound activity must pass
	// signature verification and the allowlist before any delegation; the policy hot-reloads from
	// its mounted file without a pod restart. An empty POLICY_PATH disables the border and is valid
	// only for local-only dev where no untrusted actor can reach the inbox (docs/fediverse.md §3).
	if cfg.PolicyPath != "" {
		store := policy.NewStore(cfg.PolicyPath, log)
		go store.Watch(ctx, cfg.PolicyReloadInterval)
		keyClient := &http.Client{Timeout: cfg.RequestTimeout}
		verifier := httpsig.NewVerifier(httpsig.NewHTTPKeyResolver(keyClient), cfg.SignatureMaxSkew)
		border := apgateway.NewBorder(verifier, store, log)
		if cfg.IntegrityRequireInbound {
			resolver := integrity.NewHTTPKeyResolver(&http.Client{Timeout: cfg.RequestTimeout})
			border.RequireObjectIntegrity(integrity.NewVerifier(resolver))
			log.Info("inbound object integrity REQUIRED — activities without a valid FEP-8b32 proof are dropped")
		}
		if cfg.BudgetEnabled {
			border.RequireBudget(budget.New(cfg.BudgetWindow, cfg.BudgetCapacity))
			log.Info("token-budget admission ENABLED — over-budget or unbudgeted actors are dropped before A2A",
				"window", cfg.BudgetWindow, "capacity", cfg.BudgetCapacity)
		}
		gateway.UseBorder(border)
		log.Info("federation policy border enabled", "policy", cfg.PolicyPath, "healthy", store.Healthy())
	} else {
		log.Warn("federation policy border DISABLED (POLICY_PATH empty) — local-only dev posture")
	}

	// Group collaboration (issue #217): expose designated rooms as AP Group actors that remote actors
	// can follow and post to, with Announce fan-out and governed @agent routing. Signed outbound
	// delivery reuses the object-integrity key; config validation guarantees the signer and border.
	if cfg.GroupsPath != "" {
		groupRegistry, gerr := apgateway.LoadGroupRegistry(cfg.GroupsPath)
		if gerr != nil {
			return gerr
		}
		deliverer := delivery.New(&http.Client{Timeout: cfg.RequestTimeout}, signer.PrivateKey(), log)
		gateway.UseGroups(groupRegistry, deliverer, &http.Client{Timeout: cfg.RequestTimeout})
		log.Info("group collaboration ENABLED", "groups", groupRegistry.Groups())
	}

	apServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.ListenPort)),
		Handler:           gateway.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.MetricsPort)),
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 2)
	go serve(apServer, "activitypub", log, errCh)
	go serve(metricsServer, "metrics", log, errCh)
	log.Info("activitypub-agent-gateway started",
		"listen", apServer.Addr, "metrics", metricsServer.Addr,
		"server_name", cfg.ServerName, "public", cfg.PublicBaseURL(),
		"agents", registry.Ghosts())

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	_ = metricsServer.Shutdown(shutdownCtx)
	if err := apServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}

func serve(server *http.Server, name string, log *slog.Logger, errCh chan<- error) {
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server stopped", "server", name, "error", err)
		errCh <- err
	}
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
