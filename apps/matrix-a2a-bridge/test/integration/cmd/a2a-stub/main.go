// Command a2a-stub serves the smallest real A2A endpoint needed by the kind integration test.
// It uses the official A2A server SDK, so the bridge still exercises AgentCard discovery and
// JSON-RPC message/send on the wire without bringing an LLM or kagent into the fixture.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/gowebpki/jcs"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
)

const (
	localAgentPath          = "/api/a2a/kagent/integration-agent"
	remoteAgentPath         = "/api/a2a/remote-agent"
	localReplyText          = "integration reply"
	remoteReplyText         = "remote integration reply"
	remoteAgentName         = "Fgentic signed remote integration stub"
	remoteAgentOrganization = "Fgentic integration"
	remoteKeyID             = "integration-p256-v1"
	integrationTokenBudget  = 4096
	minLoadDelay            = 2 * time.Second
	maxLoadDelay            = 5 * time.Second
)

var loadMarkerPattern = regexp.MustCompile(`\bload room=(\d{2}) seq=(\d{2})\b`)

type requestRecord struct {
	Room     int `json:"room"`
	Sequence int `json:"sequence"`
}

type statsSnapshot struct {
	Active             int             `json:"active"`
	CardTampered       bool            `json:"card_tampered"`
	DelayMillis        int64           `json:"delay_millis"`
	MaxActive          int             `json:"max_active"`
	RemoteCardRequests int             `json:"remote_card_requests"`
	RemoteRequests     int             `json:"remote_requests"`
	TokenBudgetValid   bool            `json:"token_budget_valid"`
	TotalRequests      int             `json:"total_requests"`
	TotalStarted       int             `json:"total_started"`
	TotalCompleted     int             `json:"total_completed"`
	Starts             []requestRecord `json:"starts"`
	Completions        []requestRecord `json:"completions"`
}

type statsRecorder struct {
	mu                 sync.Mutex
	delay              time.Duration
	active             int
	maxActive          int
	totalRequests      int
	remoteRequests     int
	remoteCardRequests int
	cardTampered       bool
	tokenBudgetValid   bool
	totalStarted       int
	totalCompleted     int
	starts             []requestRecord
	completions        []requestRecord
}

func (r *statsRecorder) request(remote bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.totalRequests++
	if remote {
		r.remoteRequests++
	}
}

func (r *statsRecorder) remoteCard() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remoteCardRequests++
	return r.cardTampered
}

func (r *statsRecorder) tamperCard() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cardTampered = true
}

func (r *statsRecorder) markTokenBudgetValid() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokenBudgetValid = true
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
		Active:             r.active,
		CardTampered:       r.cardTampered,
		DelayMillis:        r.delay.Milliseconds(),
		MaxActive:          r.maxActive,
		RemoteCardRequests: r.remoteCardRequests,
		RemoteRequests:     r.remoteRequests,
		TokenBudgetValid:   r.tokenBudgetValid,
		TotalRequests:      r.totalRequests,
		TotalStarted:       r.totalStarted,
		TotalCompleted:     r.totalCompleted,
		Starts:             append([]requestRecord(nil), r.starts...),
		Completions:        append([]requestRecord(nil), r.completions...),
	}
}

type executor struct {
	delay  time.Duration
	remote bool
	reply  string
	stats  *statsRecorder
}

func (e executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.stats.request(e.remote)
		if e.remote {
			if !validTokenBudgetContract(execCtx) {
				yield(nil, fmt.Errorf("remote request did not carry the configured token-budget contract"))
				return
			}
			e.stats.markTokenBudgetValid()
		}
		record, loadRequest := parseLoadMarker(messageText(execCtx.Message))
		if !loadRequest {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(e.reply)), nil)
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

func validTokenBudgetContract(execCtx *a2asrv.ExecutorContext) bool {
	if execCtx == nil || execCtx.Message == nil {
		return false
	}
	if !slices.Contains(execCtx.Message.Extensions, a2aclient.TokenBudgetExtensionURI) {
		return false
	}
	extensionMetadata, ok := execCtx.Message.Metadata[a2aclient.TokenBudgetExtensionURI].(map[string]any)
	if !ok {
		return false
	}
	// JSON numbers decode into float64 at the A2A wire boundary.
	maxTokens, ok := extensionMetadata["maxTokens"].(float64)
	if !ok || maxTokens != integrationTokenBudget {
		return false
	}
	extensions, ok := execCtx.ServiceParams.Get(a2a.SvcParamExtensions)
	return ok && slices.Contains(extensions, a2aclient.TokenBudgetExtensionURI)
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

	localHandler := a2asrv.NewHandler(
		executor{delay: delay, reply: localReplyText, stats: stats},
		a2asrv.WithTaskStore(taskstore.NewInMemory(nil)),
	)
	localCard := &a2a.AgentCard{
		Name:                "Fgentic bridge integration stub",
		Description:         "Deterministic A2A endpoint for the Matrix appservice wire test",
		Version:             "integration",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(baseURL+localAgentPath, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{},
		Skills:              []a2a.AgentSkill{},
	}
	remoteCard, err := signedRemoteAgentCard(baseURL)
	if err != nil {
		return fmt.Errorf("create signed remote AgentCard: %w", err)
	}
	validRemoteCard, err := json.Marshal(remoteCard)
	if err != nil {
		return fmt.Errorf("encode signed remote AgentCard: %w", err)
	}
	tamperedCard := *remoteCard
	tamperedCard.Name += " (tampered after signing)"
	tamperedRemoteCard, err := json.Marshal(&tamperedCard)
	if err != nil {
		return fmt.Errorf("encode tampered remote AgentCard: %w", err)
	}
	remoteHandler := a2asrv.NewHandler(
		executor{delay: delay, remote: true, reply: remoteReplyText, stats: stats},
		a2asrv.WithTaskStore(taskstore.NewInMemory(nil)),
		a2asrv.WithCapabilityChecks(&remoteCard.Capabilities),
	)

	mux := http.NewServeMux()
	mux.Handle(localAgentPath+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(localCard))
	mux.Handle(localAgentPath, a2asrv.NewJSONRPCHandler(localHandler))
	mux.HandleFunc("GET "+remoteAgentPath+a2asrv.WellKnownAgentCardPath, func(w http.ResponseWriter, _ *http.Request) {
		cardJSON := validRemoteCard
		if stats.remoteCard() {
			cardJSON = tamperedRemoteCard
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(cardJSON); err != nil {
			slog.Warn("write remote AgentCard", "err", err)
		}
	})
	mux.Handle(remoteAgentPath, a2asrv.NewJSONRPCHandler(remoteHandler))
	mux.HandleFunc("POST /control/tamper", func(w http.ResponseWriter, _ *http.Request) {
		stats.tamperCard()
		w.WriteHeader(http.StatusNoContent)
	})
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
		slog.Info(
			"A2A integration stub started",
			"listen", addr,
			"local_agent_path", localAgentPath,
			"remote_agent_path", remoteAgentPath,
			"load_delay", delay,
		)
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

func signedRemoteAgentCard(baseURL string) (*a2a.AgentCard, error) {
	card := &a2a.AgentCard{
		Name:        remoteAgentName,
		Description: "Deterministic signed remote endpoint for the Matrix appservice trust test",
		Provider: &a2a.AgentProvider{
			Org: remoteAgentOrganization,
			URL: baseURL,
		},
		Version:             "integration",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(baseURL+remoteAgentPath, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities: a2a.AgentCapabilities{
			Extensions: []a2a.AgentExtension{{URI: a2aclient.TokenBudgetExtensionURI, Required: true}},
		},
		Skills: []a2a.AgentSkill{{
			ID:          "echo",
			Name:        "Echo delegated text",
			Description: "Returns a deterministic response for the remote delegation contract test",
			Tags:        []string{"integration", "text"},
		}},
	}
	return signAgentCard(card)
}

func signAgentCard(card *a2a.AgentCard) (*a2a.AgentCard, error) {
	unsigned := *card
	unsigned.Signatures = nil
	encoded, err := json.Marshal(&unsigned)
	if err != nil {
		return nil, fmt.Errorf("encode unsigned AgentCard: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize unsigned AgentCard: %w", err)
	}

	protectedJSON := []byte(`{"alg":"ES256","kid":"` + remoteKeyID + `","typ":"JOSE"}`)
	protected := base64.RawURLEncoding.EncodeToString(protectedJSON)
	payload := base64.RawURLEncoding.EncodeToString(canonical)
	digest := sha256.Sum256([]byte(protected + "." + payload))
	privateKey := fixturePrivateKey()
	der, err := privateKey.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign AgentCard: %w", err)
	}
	signature, err := joseSignature(der)
	if err != nil {
		return nil, err
	}

	signed := *card
	signed.Signatures = []a2a.AgentCardSignature{{
		Protected: protected,
		Signature: base64.RawURLEncoding.EncodeToString(signature),
	}}
	return &signed, nil
}

func fixturePrivateKey() *ecdsa.PrivateKey {
	// Scalar 1 is intentionally public and test-only. It fixes the P-256 identity; signatures
	// still use secure runtime randomness and are verified by behavior rather than golden bytes.
	curve := elliptic.P256()
	params := curve.Params()
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).Set(params.Gx),
			Y:     new(big.Int).Set(params.Gy),
		},
		D: big.NewInt(1),
	}
}

func joseSignature(der []byte) ([]byte, error) {
	var decoded struct {
		R *big.Int
		S *big.Int
	}
	rest, err := asn1.Unmarshal(der, &decoded)
	if err != nil {
		return nil, fmt.Errorf("decode ECDSA signature: %w", err)
	}
	if len(rest) != 0 || decoded.R == nil || decoded.S == nil {
		return nil, fmt.Errorf("decode ECDSA signature: malformed value")
	}
	const coordinateBytes = 32
	if decoded.R.BitLen() > coordinateBytes*8 || decoded.S.BitLen() > coordinateBytes*8 {
		return nil, fmt.Errorf("decode ECDSA signature: coordinate exceeds P-256 width")
	}
	raw := make([]byte, coordinateBytes*2)
	decoded.R.FillBytes(raw[:coordinateBytes])
	decoded.S.FillBytes(raw[coordinateBytes:])
	return raw, nil
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
