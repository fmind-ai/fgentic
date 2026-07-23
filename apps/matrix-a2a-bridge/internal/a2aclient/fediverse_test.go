package a2aclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

const testMultikey = "z1111111111111111111111111111111111111111"

func TestUseFediverseBrokerRejectsPublicEndpoint(t *testing.T) {
	client := New("http://local.invalid", "local-gateway-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.UseFediverseBroker("https://broker.example.com", "broker-secret"); err == nil {
		t.Fatal("UseFediverseBroker accepted a public endpoint")
	}
}

func TestFediverseTargetRejectsBrokerActorMismatch(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(fediverseResolution{
			Transport:   brokerTransportA2A,
			ActorID:     "https://attacker.example.com/users/agent",
			A2AEndpoint: "https://a2a.example.com/agent",
			AgentCard:   "https://a2a.example.com/card",
		})
	}))
	t.Cleanup(broker.Close)

	target := newFediverseTestTarget(t, "https://peer.example.com/users/agent")
	client := New("http://local.invalid", "local-gateway-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.UseFediverseBroker(broker.URL, "broker-secret"); err != nil {
		t.Fatalf("UseFediverseBroker: %v", err)
	}
	if _, err := client.ResolveAgentCard(t.Context(), target); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("ResolveAgentCard error = %v, want ErrRemoteTargetUntrusted", err)
	}
}

func TestFediverseTargetActivityPubFallback(t *testing.T) {
	var mu sync.Mutex
	var brokerHeaders []http.Header
	var brokerRequests []fediverseBrokerRequest
	actorID := "https://peer.example.com/users/agent"
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		brokerHeaders = append(brokerHeaders, r.Header.Clone())
		var req fediverseBrokerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			mu.Unlock()
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		brokerRequests = append(brokerRequests, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		response := fediverseResolution{
			Transport: brokerTransportActivityPub, ActorID: actorID, Name: "AP peer", Summary: "AP-only peer", Inbox: actorID + "/inbox",
		}
		if strings.HasSuffix(r.URL.Path, "/delegate") {
			response.ActivityID = "https://local.example/ap/instance/activities/delegations/1"
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(broker.Close)

	target := newFediverseTestTarget(t, actorID)
	client := New("http://local.invalid", "local-gateway-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := client.UseFediverseBroker(broker.URL, "broker-secret"); err != nil {
		t.Fatalf("UseFediverseBroker: %v", err)
	}
	card, err := client.ResolveAgentCard(t.Context(), target)
	if err != nil {
		t.Fatalf("ResolveAgentCard: %v", err)
	}
	if !client.IsReady(target) || card.Name != "AP peer" {
		t.Fatalf("card = %+v, ready = %v", card, client.IsReady(target))
	}
	result, err := client.CallWithMessageID(
		WithUser(t.Context(), "@alice:local.example"), target, "matrix-event-1", "review this", "", nil,
	)
	if err != nil {
		t.Fatalf("CallWithMessageID: %v", err)
	}
	if !result.Terminal || !strings.Contains(result.Text, "ActivityPub") {
		t.Fatalf("result = %+v", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(brokerHeaders) != 2 || len(brokerRequests) != 2 {
		t.Fatalf("broker calls = %d, requests = %d", len(brokerHeaders), len(brokerRequests))
	}
	for _, headers := range brokerHeaders {
		if got := headers.Get("Authorization"); got != "Bearer broker-secret" || strings.Contains(got, "local-gateway-secret") {
			t.Fatalf("broker Authorization = %q", got)
		}
	}
	if brokerRequests[1].Sender != "@alice:local.example" || brokerRequests[1].Text != "review this" {
		t.Fatalf("delegate request = %+v", brokerRequests[1])
	}
}

func TestFediverseTargetUpgradesToPinnedA2AWithoutLocalCredential(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	var remoteAuthorization string
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("upgraded ack")), nil)
		}
	})
	a2aHandler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor, a2asrv.WithTaskStore(taskstore.NewInMemory(nil))))
	mux := http.NewServeMux()
	peer := httptest.NewTLSServer(mux)
	t.Cleanup(peer.Close)
	endpoint := peer.URL + "/a2a"
	cardURL := peer.URL + "/card"
	card := validRemoteCard(endpoint)
	signedCard := signValidAgentCard(t, card, key, identity.KeyID)
	mux.HandleFunc("/card", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		_, _ = w.Write(signedCard)
	})
	mux.HandleFunc("/a2a", func(w http.ResponseWriter, r *http.Request) {
		remoteAuthorization = r.Header.Get("Authorization")
		a2aHandler.ServeHTTP(w, r)
	})

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer broker-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(fediverseResolution{
			Transport: brokerTransportA2A, ActorID: "https://peer.example.com/users/agent",
			A2AEndpoint: endpoint, AgentCard: cardURL,
		})
	}))
	t.Cleanup(broker.Close)

	target, err := NewFediverseTarget("acct:agent@peer.example.com", identity, ActivityPubIdentity{
		ActorID: "https://peer.example.com/users/agent", VerificationMethod: "https://peer.example.com/users/agent#key", PublicKeyMultibase: testMultikey, ProofMaxAge: 24 * time.Hour,
	}, 4096, nil)
	if err != nil {
		t.Fatalf("NewFediverseTarget: %v", err)
	}
	client := New("http://local.invalid", "local-gateway-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	resolvedTarget, err := target.resolvedRemote(endpoint, cardURL)
	if err != nil {
		t.Fatalf("resolvedRemote fixture: %v", err)
	}
	client.remoteTransports.Store(resolvedTarget.ID(), &userTransport{base: peer.Client().Transport})
	if err := client.UseFediverseBroker(broker.URL, "broker-secret"); err != nil {
		t.Fatalf("UseFediverseBroker: %v", err)
	}
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("ResolveAgentCard: %v", err)
	}
	result, err := client.Call(WithUser(t.Context(), "@alice:local.example"), target, "hello", "", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Text != "upgraded ack" || !result.Terminal {
		t.Fatalf("result = %+v", result)
	}
	if remoteAuthorization != "" {
		t.Fatalf("remote A2A Authorization = %q, want empty", remoteAuthorization)
	}
}

func TestFediverseTargetRouteChangeInvalidatesOldA2AClientBeforeVerification(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	var useChangedRoute atomic.Bool
	mux := http.NewServeMux()
	peer := httptest.NewTLSServer(mux)
	t.Cleanup(peer.Close)
	initialEndpoint := peer.URL + "/a2a-initial"
	initialCardURL := peer.URL + "/card-initial"
	signedCard := signValidAgentCard(t, validRemoteCard(initialEndpoint), key, identity.KeyID)
	mux.HandleFunc("/card-initial", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		_, _ = w.Write(signedCard)
	})
	mux.HandleFunc("/card-changed", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	})

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		endpoint, cardURL := initialEndpoint, initialCardURL
		if useChangedRoute.Load() {
			endpoint, cardURL = peer.URL+"/a2a-changed", peer.URL+"/card-changed"
		}
		_ = json.NewEncoder(w).Encode(fediverseResolution{
			Transport: brokerTransportA2A, ActorID: "https://peer.example.com/users/agent",
			A2AEndpoint: endpoint, AgentCard: cardURL,
		})
	}))
	t.Cleanup(broker.Close)

	target, err := NewFediverseTarget("acct:agent@peer.example.com", identity, ActivityPubIdentity{
		ActorID: "https://peer.example.com/users/agent", VerificationMethod: "https://peer.example.com/users/agent#key", PublicKeyMultibase: testMultikey, ProofMaxAge: 24 * time.Hour,
	}, 4096, nil)
	if err != nil {
		t.Fatalf("NewFediverseTarget: %v", err)
	}
	client := New("http://local.invalid", "local-gateway-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, route := range [][2]string{
		{initialEndpoint, initialCardURL},
		{peer.URL + "/a2a-changed", peer.URL + "/card-changed"},
	} {
		resolvedTarget, resolveErr := target.resolvedRemote(route[0], route[1])
		if resolveErr != nil {
			t.Fatalf("resolvedRemote fixture: %v", resolveErr)
		}
		client.remoteTransports.Store(resolvedTarget.ID(), &userTransport{base: peer.Client().Transport})
	}
	if err := client.UseFediverseBroker(broker.URL, "broker-secret"); err != nil {
		t.Fatalf("UseFediverseBroker: %v", err)
	}
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("initial ResolveAgentCard: %v", err)
	}
	if !client.IsReady(target) {
		t.Fatal("initial verified route is not ready")
	}

	useChangedRoute.Store(true)
	if _, err := client.ResolveAgentCard(t.Context(), target); err == nil {
		t.Fatal("changed unverified route was accepted")
	}
	if client.IsReady(target) {
		t.Fatal("old verified route remained ready after WebFinger changed it")
	}
	if _, err := client.Call(t.Context(), target, "must not use stale route", "", nil); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("Call error = %v, want ErrRemoteTargetUntrusted", err)
	}
}

func newFediverseTestTarget(t *testing.T, actorID string) Target {
	t.Helper()
	target, err := NewFediverseTarget("acct:agent@peer.example.com", testCardIdentity(t, newTestSigningKey(t)), ActivityPubIdentity{
		ActorID: actorID, VerificationMethod: actorID + "#ed25519-key", PublicKeyMultibase: testMultikey, ProofMaxAge: 24 * time.Hour,
	}, 4096, nil)
	if err != nil {
		t.Fatalf("NewFediverseTarget: %v", err)
	}
	return target
}
