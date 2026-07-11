// Command a2a-stub serves the smallest real A2A endpoint needed by the kind integration test.
// It uses the official A2A server SDK, so the bridge still exercises AgentCard discovery and
// JSON-RPC message/send on the wire without bringing an LLM or kagent into the fixture.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

const (
	agentPath    = "/api/a2a/kagent/integration-agent"
	replyText    = "integration reply"
	minLoadDelay = 2 * time.Second
	maxLoadDelay = 5 * time.Second
)

var loadMarkerPattern = regexp.MustCompile(`\bload room=(\d{2}) seq=(\d{2})\b`)

type requestRecord struct {
	Room     int `json:"room"`
	Sequence int `json:"sequence"`
}

type statsSnapshot struct {
	Active         int             `json:"active"`
	DelayMillis    int64           `json:"delay_millis"`
	MaxActive      int             `json:"max_active"`
	TotalStarted   int             `json:"total_started"`
	TotalCompleted int             `json:"total_completed"`
	Starts         []requestRecord `json:"starts"`
	Completions    []requestRecord `json:"completions"`
}

type statsRecorder struct {
	mu             sync.Mutex
	delay          time.Duration
	active         int
	maxActive      int
	totalStarted   int
	totalCompleted int
	starts         []requestRecord
	completions    []requestRecord
}

func (r *statsRecorder) start(record requestRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active++
	r.totalStarted++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.starts = append(r.starts, record)
}

func (r *statsRecorder) finish(record requestRecord, completed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	if completed {
		r.totalCompleted++
		r.completions = append(r.completions, record)
	}
}

func (r *statsRecorder) snapshot() statsSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return statsSnapshot{
		Active:         r.active,
		DelayMillis:    r.delay.Milliseconds(),
		MaxActive:      r.maxActive,
		TotalStarted:   r.totalStarted,
		TotalCompleted: r.totalCompleted,
		Starts:         append([]requestRecord(nil), r.starts...),
		Completions:    append([]requestRecord(nil), r.completions...),
	}
}

type executor struct {
	delay time.Duration
	stats *statsRecorder
}

func (e executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		record, loadRequest := parseLoadMarker(messageText(execCtx.Message))
		if !loadRequest {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(replyText)), nil)
			return
		}

		e.stats.start(record)
		if err := waitDelay(ctx, e.delay); err != nil {
			e.stats.finish(record, false)
			yield(nil, err)
			return
		}
		e.stats.finish(record, true)
		reply := fmt.Sprintf("load reply room=%02d seq=%02d", record.Room, record.Sequence)
		yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(reply)), nil)
	}
}

func (executor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func main() {
	if err := run(); err != nil {
		slog.Error("A2A stub exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	baseURL := strings.TrimRight(envOrDefault("A2A_STUB_BASE_URL", "http://a2a-stub:8080"), "/")
	addr := envOrDefault("A2A_STUB_LISTEN", ":8080")
	delay, err := loadDelay()
	if err != nil {
		return err
	}
	stats := &statsRecorder{delay: delay}

	handler := a2asrv.NewHandler(executor{delay: delay, stats: stats}, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	card := &a2a.AgentCard{
		Name:                "Fgentic bridge integration stub",
		Description:         "Deterministic A2A endpoint for the Matrix appservice wire test",
		Version:             "integration",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(baseURL+agentPath, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{},
		Skills:              []a2a.AgentSkill{},
	}

	mux := http.NewServeMux()
	mux.Handle(agentPath+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(agentPath, a2asrv.NewJSONRPCHandler(handler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(stats.snapshot()); err != nil {
			slog.Error("encode A2A stub stats", "err", err)
		}
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("A2A integration stub started", "listen", addr, "agent_path", agentPath, "load_delay", delay)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve A2A stub: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down A2A stub: %w", err)
		}
		return nil
	}
}

func loadDelay() (time.Duration, error) {
	raw := envOrDefault("A2A_STUB_DELAY", "0s")
	delay, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse A2A_STUB_DELAY %q: %w", raw, err)
	}
	if delay != 0 && (delay < minLoadDelay || delay > maxLoadDelay) {
		return 0, fmt.Errorf("A2A_STUB_DELAY must be 0 or between %s and %s, got %s", minLoadDelay, maxLoadDelay, delay)
	}
	return delay, nil
}

func parseLoadMarker(text string) (requestRecord, bool) {
	match := loadMarkerPattern.FindStringSubmatch(text)
	if len(match) != 3 {
		return requestRecord{}, false
	}
	room, roomErr := strconv.Atoi(match[1])
	sequence, sequenceErr := strconv.Atoi(match[2])
	if roomErr != nil || sequenceErr != nil {
		return requestRecord{}, false
	}
	return requestRecord{Room: room, Sequence: sequence}, true
}

func messageText(message *a2a.Message) string {
	if message == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range message.Parts {
		text.WriteString(part.Text())
	}
	return text.String()
}

func waitDelay(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
