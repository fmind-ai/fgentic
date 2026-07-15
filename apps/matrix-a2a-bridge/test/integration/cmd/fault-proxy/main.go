// Command fault-proxy provides deterministic external crash boundaries for the integration fixture.
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
	"strings"
	"syscall"
	"time"
)

type proxyConfig struct {
	controlListen    string
	postgresListen   string
	postgresUpstream string
	matrixListen     string
	matrixUpstream   string
	a2aListen        string
	a2aUpstream      string
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("fault proxy exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := proxyConfig{
		controlListen:    envOrDefault("FAULT_CONTROL_LISTEN", ":8081"),
		postgresListen:   envOrDefault("FAULT_POSTGRES_LISTEN", ":5432"),
		postgresUpstream: envOrDefault("FAULT_POSTGRES_UPSTREAM", "postgres:5432"),
		matrixListen:     envOrDefault("FAULT_MATRIX_LISTEN", ":8008"),
		matrixUpstream:   envOrDefault("FAULT_MATRIX_UPSTREAM", "http://synapse:8008"),
		a2aListen:        envOrDefault("FAULT_A2A_LISTEN", ":8080"),
		a2aUpstream:      envOrDefault("FAULT_A2A_UPSTREAM", "http://plain-a2a-agent.plain-agent.svc.cluster.local:8080"),
	}
	controller := &faultController{}
	matrixHandler, err := newHTTPProxy(cfg.matrixUpstream, "matrix", controller)
	if err != nil {
		return err
	}
	a2aHandler, err := newHTTPProxy(cfg.a2aUpstream, "a2a", controller)
	if err != nil {
		return err
	}
	postgresListener, err := net.Listen("tcp", cfg.postgresListen)
	if err != nil {
		return fmt.Errorf("listen for PostgreSQL proxy: %w", err)
	}
	defer func() { _ = postgresListener.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	servers := []*http.Server{
		newHTTPServer(cfg.controlListen, controller.controlHandler()),
		newHTTPServer(cfg.matrixListen, matrixHandler),
		newHTTPServer(cfg.a2aListen, a2aHandler),
	}
	errs := make(chan error, len(servers)+1)
	for _, server := range servers {
		go func() { errs <- server.ListenAndServe() }()
	}
	go func() {
		errs <- (postgresProxy{controller: controller, upstream: cfg.postgresUpstream}).serve(ctx, postgresListener)
	}()

	slog.Info("crash fault proxy started")
	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	stop()
	_ = postgresListener.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("shut down fault proxy: %w", err)
		}
	}
	return nil
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
