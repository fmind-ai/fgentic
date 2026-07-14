package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"

	"github.com/fmind/matrix-a2a-bridge/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEventProcessorPreservesDeliveryOrder(t *testing.T) {
	as := appservice.Create()
	handler := func(context.Context, *event.Event) {}
	processor := newEventProcessor(as, handler, handler, handler)
	if processor.ExecMode != appservice.Sync {
		t.Fatalf("event processor mode = %v, want synchronous delivery", processor.ExecMode)
	}
}

func TestEventProcessorDrainWaitsForAcceptedHandlers(t *testing.T) {
	as := appservice.Create()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	handler := func(context.Context, *event.Event) {
		close(started)
		<-release
	}
	processor := newEventProcessor(as, handler, func(context.Context, *event.Event) {}, func(context.Context, *event.Event) {})
	processor.Start(t.Context())
	defer processor.Stop()
	as.Events <- &event.Event{Type: event.EventMessage}
	<-started

	drainCtx, cancelDrain := context.WithTimeout(t.Context(), time.Second)
	defer cancelDrain()
	drainDone := make(chan error, 1)
	go func() { drainDone <- processor.Drain(drainCtx) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(as.Events) == 0 {
		select {
		case err := <-drainDone:
			t.Fatalf("Drain returned before prior handler completed: %v", err)
		case <-deadline.C:
			t.Fatal("shutdown barrier was not queued")
		default:
			runtime.Gosched()
		}
	}
	select {
	case err := <-drainDone:
		t.Fatalf("Drain returned before releasing prior handler: %v", err)
	default:
	}

	unblock()
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-drainCtx.Done():
		t.Fatalf("Drain did not observe shutdown barrier: %v", drainCtx.Err())
	}
}

type stopFunc func()

func (fn stopFunc) Stop() {
	fn()
}

type drainFunc func(context.Context) error

func (fn drainFunc) Drain(ctx context.Context) error {
	return fn(ctx)
}

func TestDrainBridgeReturnsAfterGracefulCompletion(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
	defer cancelRuntime()
	stopped := false

	if graceful := drainBridge(stopFunc(func() { stopped = true }), cancelRuntime, time.Second); !graceful {
		t.Fatal("drainBridge reported a timeout for an immediate stop")
	}
	if !stopped {
		t.Fatal("drainBridge returned before Stop completed")
	}
	if err := runtimeCtx.Err(); err != nil {
		t.Fatalf("graceful drain cancelled the runtime context: %v", err)
	}
}

func TestDrainBridgeCancelsRuntimeAfterDeadlineAndWaitsForCleanup(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	stopper := stopFunc(func() {
		<-runtimeCtx.Done()
		close(stopped)
	})

	if graceful := drainBridge(stopper, cancelRuntime, 0); graceful {
		t.Fatal("drainBridge reported graceful completion after the deadline")
	}
	if !errors.Is(runtimeCtx.Err(), context.Canceled) {
		t.Fatalf("runtime context error = %v, want canceled", runtimeCtx.Err())
	}
	select {
	case <-stopped:
	default:
		t.Fatal("drainBridge returned before cancellation cleanup completed")
	}
}

func TestDrainEventProcessorReturnsWithinGraceWithoutCancellingRuntime(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
	defer cancelRuntime()
	calls := 0
	drainer := drainFunc(func(ctx context.Context) error {
		calls++
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			return errors.New("event processor barrier used a cancellable deadline")
		}
		return nil
	})

	withinGrace, err := drainEventProcessor(drainer, cancelRuntime, time.Second)
	if err != nil || !withinGrace {
		t.Fatalf("drainEventProcessor = (%v, %v), want (true, nil)", withinGrace, err)
	}
	if calls != 1 {
		t.Fatalf("Drain calls = %d, want exactly 1", calls)
	}
	if err := runtimeCtx.Err(); err != nil {
		t.Fatalf("event drain cancelled runtime within grace: %v", err)
	}
}

func TestDrainEventProcessorCancelsAfterGraceAndStillWaitsForBarrier(t *testing.T) {
	runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
	drained := make(chan struct{})
	calls := 0
	drainer := drainFunc(func(ctx context.Context) error {
		calls++
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			return errors.New("event processor barrier used a cancellable deadline")
		}
		<-runtimeCtx.Done()
		close(drained)
		return nil
	})

	withinGrace, err := drainEventProcessor(drainer, cancelRuntime, 0)
	if err != nil || withinGrace {
		t.Fatalf("drainEventProcessor = (%v, %v), want (false, nil)", withinGrace, err)
	}
	if calls != 1 {
		t.Fatalf("Drain calls = %d, want exactly 1", calls)
	}
	if !errors.Is(runtimeCtx.Err(), context.Canceled) {
		t.Fatalf("runtime context error = %v, want canceled", runtimeCtx.Err())
	}
	select {
	case <-drained:
	default:
		t.Fatal("drainEventProcessor returned before the barrier completed")
	}
}

type httpClientResult struct {
	response *http.Response
	err      error
}

func TestShutdownHTTPServerWaitsForBlockedTransaction(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	shutdownStarted := make(chan struct{})
	server.Config.RegisterOnShutdown(func() { close(shutdownStarted) })
	request, err := http.NewRequest(
		http.MethodPut,
		server.URL+"/_matrix/app/v1/transactions/blocked",
		strings.NewReader(`{"events":[]}`),
	)
	if err != nil {
		t.Fatalf("build transaction request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	requestDone := make(chan httpClientResult, 1)
	go func() {
		response, err := server.Client().Do(request)
		requestDone <- httpClientResult{response: response, err: err}
	}()
	<-started

	shutdownDone := make(chan struct {
		forced bool
		err    error
	}, 1)
	go func() {
		forced, err := shutdownHTTPServer(server.Config, time.Second)
		shutdownDone <- struct {
			forced bool
			err    error
		}{forced: forced, err: err}
	}()
	<-shutdownStarted
	select {
	case result := <-shutdownDone:
		t.Fatalf("shutdown returned while transaction was blocked: %+v", result)
	default:
	}

	unblock()
	requestResult := <-requestDone
	if requestResult.err != nil {
		t.Fatalf("blocked transaction request: %v", requestResult.err)
	}
	t.Cleanup(func() {
		if err := requestResult.response.Body.Close(); err != nil {
			t.Errorf("close transaction response: %v", err)
		}
	})
	if requestResult.response.StatusCode != http.StatusOK {
		t.Fatalf("blocked transaction status = %d, want 200", requestResult.response.StatusCode)
	}
	shutdown := <-shutdownDone
	if shutdown.err != nil || shutdown.forced {
		t.Fatalf("graceful shutdown = %+v, want unforced success", shutdown)
	}
}

func TestShutdownHTTPServerTimeoutForceClosePreventsSuccessfulResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		close(finished)
	}))
	t.Cleanup(server.Close)
	request, err := http.NewRequest(
		http.MethodPut,
		server.URL+"/_matrix/app/v1/transactions/blocked",
		strings.NewReader(`{"events":[]}`),
	)
	if err != nil {
		t.Fatalf("build transaction request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	requestDone := make(chan httpClientResult, 1)
	go func() {
		response, err := server.Client().Do(request)
		requestDone <- httpClientResult{response: response, err: err}
	}()
	<-started

	forced, err := shutdownHTTPServer(server.Config, 0)
	if err != nil {
		t.Fatalf("shutdownHTTPServer: %v", err)
	}
	if !forced {
		t.Fatal("shutdownHTTPServer did not force-close the blocked transaction")
	}
	unblock()
	<-finished
	requestResult := <-requestDone
	if requestResult.response != nil {
		if err := requestResult.response.Body.Close(); err != nil {
			t.Errorf("close force-closed transaction response: %v", err)
		}
	}
	if requestResult.err == nil {
		t.Fatalf("force-closed transaction received successful status %d", requestResult.response.StatusCode)
	}
}

type fakeHTTPShutdownServer struct {
	shutdownErr error
	closeErr    error
	closeCalls  int
}

func (server *fakeHTTPShutdownServer) Shutdown(context.Context) error {
	return server.shutdownErr
}

func (server *fakeHTTPShutdownServer) Close() error {
	server.closeCalls++
	return server.closeErr
}

func TestShutdownHTTPServerPreservesNonContextError(t *testing.T) {
	want := errors.New("close listener")
	wantClose := errors.New("force close listener")
	server := &fakeHTTPShutdownServer{shutdownErr: want, closeErr: wantClose}

	forced, err := shutdownHTTPServer(server, time.Second)
	if !forced {
		t.Fatal("non-context shutdown error did not trigger a force close")
	}
	if !errors.Is(err, want) {
		t.Fatalf("shutdownHTTPServer error = %v, want wrapped %v", err, want)
	}
	if !errors.Is(err, wantClose) {
		t.Fatalf("shutdownHTTPServer error = %v, want wrapped %v", err, wantClose)
	}
	if server.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", server.closeCalls)
	}
}

func TestServeHTTPServerReportsListenerStartupFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close reserved listener: %v", err)
		}
	})

	server := &http.Server{Addr: listener.Addr().String(), Handler: http.NotFoundHandler()}
	if err := <-serveHTTPServer(server); err == nil {
		t.Fatal("serveHTTPServer hid listener startup failure")
	}
}

func TestRunFailsFastOnMissingRegistration(t *testing.T) {
	cfg := config.Config{RegistrationPath: t.TempDir() + "/missing.yaml"}
	err := run(cfg, testLogger())
	if err == nil || !strings.Contains(err.Error(), "load registration") {
		t.Fatalf("run error = %v, want registration failure", err)
	}
}

func TestRunFailsFastOnMissingAgentsMap(t *testing.T) {
	registrationPath := t.TempDir() + "/registration.yaml"
	registration := appservice.CreateRegistration()
	registration.ID = "test"
	registration.SenderLocalpart = "a2a-bridge"
	if err := registration.Save(registrationPath); err != nil {
		t.Fatalf("save registration: %v", err)
	}

	cfg := config.Config{
		RegistrationPath: registrationPath,
		AgentsPath:       t.TempDir() + "/missing-agents.yaml",
		ServerName:       "matrix.example",
		HomeserverURL:    "http://matrix.example",
		ListenHost:       "127.0.0.1",
		ListenPort:       29331,
	}
	err := run(cfg, testLogger())
	if err == nil || !strings.Contains(err.Error(), "read agents file") {
		t.Fatalf("run error = %v, want agents-map failure", err)
	}
}

func TestOpenStateUsesMemoryWithoutDatabaseURL(t *testing.T) {
	store, stateStore, closeStore, err := openState(t.Context(), config.Config{}, testLogger())
	if err != nil {
		t.Fatalf("openState: %v", err)
	}
	defer closeStore()
	if stateStore != nil {
		t.Fatal("stateStore is non-nil for the in-memory fallback")
	}
	first, err := store.MarkEventProcessed(t.Context(), "$event")
	if err != nil || !first {
		t.Fatalf("memory store MarkEventProcessed = (%v, %v), want (true, nil)", first, err)
	}
}
