// Command gateway runs the ActivityPub agent gateway: it serves each exposed platform agent as an
// AP Service actor and delegates inbound mentions to kagent over A2A through agentgateway. It is a
// self-contained app, deliberately NOT part of the mautrix bridge, so the bridge stays AGPL-free
// and homeserver-portable and no agent holds a model credential (docs/adr/0014, docs/fediverse.md).
package main

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/fmind-ai/activitypub-agent-gateway/internal/a2a"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/activitystate"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/apgateway"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/budget"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/config"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/delivery"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/identity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/policy"
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
	activityStore := activitystate.Store(activitystate.NewMemory(cfg.ActivityRetention, cfg.ActivityQueueCapacity))
	if cfg.DatabaseURL != "" {
		activityStore, err = activitystate.OpenPostgres(ctx, cfg.DatabaseURL, cfg.ActivityRetention, cfg.ActivityQueueCapacity)
		if err != nil {
			return err
		}
		log.Info("durable activity inbox enabled", "retention", cfg.ActivityRetention, "queue_capacity", cfg.ActivityQueueCapacity)
	} else {
		log.Warn("DATABASE_URL not set — using in-memory activity dedup (local-only dev)")
	}
	defer func() {
		if closeErr := activityStore.Close(); closeErr != nil {
			log.Error("close activity state", "error", closeErr)
		}
	}()
	if err := gateway.UseActivityStore(activityStore); err != nil {
		return err
	}

	// Object integrity signing (FEP-8b32): when a key is mounted, outbound replies carry an
	// eddsa-jcs-2022 proof and each actor publishes its assertionMethod Multikey. An empty
	// INTEGRITY_KEY_PATH serves replies without a proof — valid for local-only dev. Transport HTTP
	// signatures deliberately use the separate RSA key loaded below.
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

	// FEP-c390 cross-transport identity (issue #218): a P-256 did:key anchors the AP actor and the
	// A2A AgentCard to one sovereign key that survives a domain move. Empty disables the binding.
	if cfg.IdentityKeyPath != "" {
		idSigner, ierr := identity.LoadSignerFromFile(cfg.IdentityKeyPath)
		if ierr != nil {
			return ierr
		}
		gateway.UseIdentity(idSigner)
		log.Info("FEP-c390 identity anchor enabled", "did", idSigner.DID())
	}

	// The federation policy border. When a policy is configured, every inbound activity must pass
	// signature verification and the allowlist before any delegation; the policy hot-reloads from
	// its mounted file without a pod restart. An empty POLICY_PATH disables the border and is valid
	// only for local-only dev where no untrusted actor can reach the inbox (docs/fediverse.md §3).
	if cfg.PolicyPath != "" {
		store := policy.NewStore(cfg.PolicyPath, log)
		go store.Watch(ctx, cfg.PolicyReloadInterval)
		keyClient := &http.Client{Timeout: cfg.RequestTimeout}
		var keyResolver httpsig.KeyResolver = httpsig.NewHTTPKeyResolver(keyClient)
		// Optional out-of-band pinned keys for peers that cannot be SSRF-fetched (e.g. in-cluster).
		// A pinned actor skips the network entirely; every other actor still uses the guarded HTTPS
		// resolver above, unchanged — pinning strictly reduces SSRF surface (ADR 0021).
		if cfg.PinnedKeysPath != "" {
			pinned, perr := httpsig.NewPinnedResolver(cfg.PinnedKeysPath, keyResolver)
			if perr != nil {
				return perr
			}
			keyResolver = pinned
			log.Info("pinned key resolver enabled", "pins", pinned.Count(), "path", cfg.PinnedKeysPath)
		}
		verifier := httpsig.NewVerifierWithFutureSkew(
			keyResolver, cfg.SignatureMaxSkew, cfg.SignatureFutureSkew,
		)
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

	// Outbound federation (issues #217, #219): group fan-out and the agent status feed share one
	// dedicated RSA HTTP-signature key and follower store. The Ed25519 key above remains scoped to
	// long-lived object proofs. Config validation guarantees both keys and the border are present.
	if cfg.GroupsPath != "" || cfg.StatusFeedEnabled || cfg.FediverseBrokerToken != "" {
		httpSigner, loadErr := httpsig.LoadRSAPrivateKeyFromFile(cfg.HTTPSignatureKeyPath)
		if loadErr != nil {
			return loadErr
		}
		deliverer := delivery.New(&http.Client{Timeout: cfg.RequestTimeout}, httpSigner, log)
		if cfg.GroupsPath != "" {
			groupRegistry, gerr := apgateway.LoadGroupRegistry(cfg.GroupsPath)
			if gerr != nil {
				return gerr
			}
			if gerr := gateway.UseGroups(groupRegistry, deliverer, &http.Client{Timeout: cfg.RequestTimeout}); gerr != nil {
				return gerr
			}
			log.Info("group collaboration ENABLED", "groups", groupRegistry.Groups())
		} else if derr := gateway.UseDelivery(deliverer, &http.Client{Timeout: cfg.RequestTimeout}); derr != nil {
			return derr
		}
		if cfg.StatusFeedEnabled {
			// Two keys per agent (actor + domain), so the limiter capacity scales with the roster.
			limiter := budget.New(cfg.StatusWindow, 2*len(registry.Ghosts())+2)
			gateway.UseStatusFeed(limiter, uint64(cfg.StatusMaxPerWindow))
			log.Info("agent status feed ENABLED", "window", cfg.StatusWindow, "maxPerWindow", cfg.StatusMaxPerWindow)
		}
	}
	processorDone, err := gateway.StartActivityProcessor(ctx)
	if err != nil {
		return err
	}

	apServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.ListenPort)),
		Handler:           gateway.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	if cfg.FediverseBrokerToken != "" {
		if err := gateway.UseFediverseBroker(cfg.FediverseBrokerToken, &http.Client{Timeout: cfg.RequestTimeout}); err != nil {
			return err
		}
		metricsMux.Handle("/internal/v1/fediverse/", gateway.FediverseBrokerHandler())
		log.Info("Matrix-to-Fediverse broker ENABLED on internal listener")
	}
	if cfg.StatusFeedEnabled {
		// The Alertmanager receiver rides the INTERNAL metrics server, never the public AP surface.
		metricsMux.HandleFunc("POST /alerts", gateway.AlertsHandler())
	}
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

	var runErr error
	processorStopped := false
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		runErr = err
		stop()
	case err := <-processorDone:
		processorStopped = true
		if err == nil && ctx.Err() == nil {
			err = errors.New("activity processor stopped unexpectedly")
		}
		runErr = err
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	_ = metricsServer.Shutdown(shutdownCtx)
	if err := apServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if !processorStopped {
		select {
		case err := <-processorDone:
			if err != nil && runErr == nil {
				runErr = err
			}
		case <-shutdownCtx.Done():
			if runErr == nil {
				runErr = fmt.Errorf("wait for activity processor: %w", shutdownCtx.Err())
			}
		}
	}
	return runErr
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
