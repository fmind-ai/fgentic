package a2aclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/gowebpki/jcs"

	"github.com/fmind/matrix-a2a-bridge/internal/agentcardjws"
)

const remoteFixturePath = "/remote-agent"

type remoteContractFixture struct {
	mu sync.Mutex

	server       *httptest.Server
	key          *ecdsa.PrivateKey
	identity     CardIdentity
	baseCard     *a2a.AgentCard
	cardBody     []byte
	contentType  string
	etag         string
	lastModified string
	cacheControl string
	return304    bool
	cardStatus   int

	cardHeaders []http.Header
	callHeaders []http.Header
	messages    []*a2a.Message
}

func newRemoteContractFixture(
	t *testing.T,
	logger *slog.Logger,
	jku string,
) (*remoteContractFixture, *Client, Target) {
	t.Helper()
	fixture := &remoteContractFixture{
		key:          newTestSigningKey(t),
		contentType:  "application/a2a+json; charset=utf-8",
		etag:         `"card-v1"`,
		lastModified: "Sat, 11 Jul 2026 08:00:00 GMT",
		cacheControl: "public, max-age=600",
	}
	fixture.identity = testCardIdentity(t, fixture.key)

	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		fixture.mu.Lock()
		fixture.messages = append(fixture.messages, execCtx.Message)
		fixture.mu.Unlock()
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("remote ack")), nil)
		}
	})
	handler := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	endpoint := a2asrv.NewJSONRPCHandler(handler)
	mux := http.NewServeMux()
	mux.HandleFunc(remoteFixturePath+remoteAgentCardPath, fixture.serveCard)
	mux.HandleFunc(remoteFixturePath, func(w http.ResponseWriter, req *http.Request) {
		fixture.mu.Lock()
		fixture.callHeaders = append(fixture.callHeaders, req.Header.Clone())
		fixture.mu.Unlock()
		endpoint.ServeHTTP(w, req)
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)

	fixture.baseCard = validRemoteCard(fixture.server.URL + remoteFixturePath)
	if jku == "" {
		fixture.cardBody = signValidAgentCard(t, fixture.baseCard, fixture.key, fixture.identity.KeyID)
	} else {
		fixture.cardBody = signAgentCard(t, fixture.baseCard, fixture.key, fixture.identity.KeyID, jku, nil, nil)
	}
	target, err := NewRemoteTarget(fixture.server.URL+remoteFixturePath, fixture.identity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return fixture, New("http://local.invalid", "local-gateway-secret", logger), target
}

func (f *remoteContractFixture) serveCard(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	f.cardHeaders = append(f.cardHeaders, req.Header.Clone())
	body := slices.Clone(f.cardBody)
	contentType := f.contentType
	etag := f.etag
	lastModified := f.lastModified
	cacheControl := f.cacheControl
	return304 := f.return304
	cardStatus := f.cardStatus
	f.mu.Unlock()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastModified)
	w.Header().Set("Cache-Control", cacheControl)
	if return304 && req.Header.Get("If-None-Match") == etag && req.Header.Get("If-Modified-Since") == lastModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if cardStatus != 0 && cardStatus != http.StatusOK {
		w.WriteHeader(cardStatus)
		return
	}
	_, _ = w.Write(body)
}

func (f *remoteContractFixture) setCard(body []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cardBody = slices.Clone(body)
	f.etag = etag
	f.return304 = false
}

func validRemoteCard(endpoint string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        "Remote contract agent",
		Description: "Signed remote contract fixture",
		Provider: &a2a.AgentProvider{
			Org: "Partner Org",
			URL: "https://partner.example",
		},
		Version: "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL:             endpoint,
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.ProtocolVersion("1.0"),
		}},
		Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{
			URI:         TokenBudgetExtensionURI,
			Description: "Partner-enforced response token ceiling",
		}}},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID:          "answer",
			Name:        "Answer prompts",
			Description: "Answers the delegated text prompt",
			Tags:        []string{"text"},
		}},
	}
}

func cloneCardForTest(t *testing.T, card *a2a.AgentCard) *a2a.AgentCard {
	t.Helper()
	cloned, err := cloneAgentCard(card)
	if err != nil {
		t.Fatalf("cloneAgentCard: %v", err)
	}
	cloned.Signatures = nil
	return cloned
}

func agentCardDocument(t *testing.T, card *a2a.AgentCard) map[string]any {
	t.Helper()
	unsigned := cloneCardForTest(t, card)
	raw, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatalf("Marshal AgentCard: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("Decode AgentCard document: %v", err)
	}
	return document
}

func cloneDocument(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal document: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var cloned map[string]any
	if err := decoder.Decode(&cloned); err != nil {
		t.Fatalf("Decode document: %v", err)
	}
	return cloned
}

func signAgentCard(
	t *testing.T,
	card *a2a.AgentCard,
	key *ecdsa.PrivateKey,
	keyID string,
	jku string,
	protectedOverrides map[string]any,
	unprotected map[string]any,
) []byte {
	t.Helper()
	document := agentCardDocument(t, card)
	return signCardDocument(t, document, cloneDocument(t, document), key, keyID, jku, protectedOverrides, unprotected)
}

func signValidAgentCard(t *testing.T, card *a2a.AgentCard, key *ecdsa.PrivateKey, keyID string) []byte {
	t.Helper()
	document := agentCardDocument(t, card)
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal unsigned AgentCard: %v", err)
	}
	bundle, err := agentcardjws.Sign(raw, key, keyID)
	if err != nil {
		t.Fatalf("Sign AgentCard: %v", err)
	}
	return bundle.AgentCard
}

func signCardDocument(
	t *testing.T,
	wireDocument map[string]any,
	payloadDocument map[string]any,
	key *ecdsa.PrivateKey,
	keyID string,
	jku string,
	protectedOverrides map[string]any,
	unprotected map[string]any,
) []byte {
	t.Helper()
	delete(payloadDocument, "signatures")
	payloadJSON, err := json.Marshal(payloadDocument)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	payload, err := jcs.Transform(payloadJSON)
	if err != nil {
		t.Fatalf("JCS payload: %v", err)
	}
	protected := map[string]any{"alg": "ES256", "typ": "JOSE", "kid": keyID}
	if jku != "" {
		protected["jku"] = jku
	}
	for name, value := range protectedOverrides {
		protected[name] = value
	}
	protectedJSON, err := json.Marshal(protected)
	if err != nil {
		t.Fatalf("Marshal protected header: %v", err)
	}
	protectedEncoded := base64.RawURLEncoding.EncodeToString(protectedJSON)
	digest := sha256.Sum256([]byte(protectedEncoded + "." + base64.RawURLEncoding.EncodeToString(payload)))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("Sign card: %v", err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	signatureDocument := map[string]any{
		"protected": protectedEncoded,
		"signature": base64.RawURLEncoding.EncodeToString(signature),
	}
	if unprotected != nil {
		signatureDocument["header"] = unprotected
	}
	wireDocument = cloneDocument(t, wireDocument)
	wireDocument["signatures"] = []any{signatureDocument}
	raw, err := json.Marshal(wireDocument)
	if err != nil {
		t.Fatalf("Marshal signed card: %v", err)
	}
	return raw
}

func tamperCardField(t *testing.T, raw []byte, field string, value any) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("Unmarshal signed card: %v", err)
	}
	document[field] = value
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal tampered card: %v", err)
	}
	return tampered
}

func TestRemoteClientSignedRoundTripAndCredentialBoundary(t *testing.T) {
	var jkuRequests atomic.Int64
	jkuServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		jkuRequests.Add(1)
	}))
	t.Cleanup(jkuServer.Close)
	fixture, client, target := newRemoteContractFixture(t, nil, jkuServer.URL+"/keys.json")

	if client.IsReady(target) {
		t.Fatal("remote target ready before card verification")
	}
	if _, err := client.Call(t.Context(), target, "must not leave", ""); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("Call before verification error = %v", err)
	}
	fixture.mu.Lock()
	if len(fixture.cardHeaders) != 0 || len(fixture.callHeaders) != 0 {
		t.Fatal("Call performed implicit remote trust discovery or delegation")
	}
	fixture.mu.Unlock()

	card, err := client.ResolveAgentCard(t.Context(), target)
	if err != nil {
		t.Fatalf("ResolveAgentCard: %v", err)
	}
	if !client.IsReady(target) || card.Name != fixture.identity.Name {
		t.Fatalf("verified card = %+v, ready = %v", card, client.IsReady(target))
	}
	card.Name = "caller mutation must not affect cached client"

	ctx := WithUser(t.Context(), "@alice:local.example")
	result, err := client.Call(ctx, target, "remote prompt", "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !result.Terminal || result.Text != "remote ack" || result.ContextID == "" {
		t.Fatalf("Call result = %+v", result)
	}
	if jkuRequests.Load() != 0 {
		t.Fatalf("untrusted jku fetched %d times", jkuRequests.Load())
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.cardHeaders) != 1 || len(fixture.callHeaders) != 1 || len(fixture.messages) != 1 {
		t.Fatalf(
			"card requests = %d, calls = %d, messages = %d",
			len(fixture.cardHeaders),
			len(fixture.callHeaders),
			len(fixture.messages),
		)
	}
	if got := fixture.cardHeaders[0].Get("Authorization"); got != "" {
		t.Fatalf("remote card Authorization = %q, want empty", got)
	}
	if got := fixture.callHeaders[0].Get("Authorization"); got != "" {
		t.Fatalf("remote call Authorization = %q, want empty", got)
	}
	if got := fixture.callHeaders[0].Get(userHeader); got != "@alice:local.example" {
		t.Fatalf("remote call %s = %q", userHeader, got)
	}
	if got := fixture.callHeaders[0].Get(a2a.SvcParamExtensions); !strings.Contains(got, TokenBudgetExtensionURI) {
		t.Fatalf("%s = %q", a2a.SvcParamExtensions, got)
	}
	if got := fixture.callHeaders[0].Get(a2a.SvcParamVersion); got != "1.0" {
		t.Fatalf("%s = %q, want 1.0", a2a.SvcParamVersion, got)
	}
	message := fixture.messages[0]
	if !slices.Contains(message.Extensions, TokenBudgetExtensionURI) {
		t.Fatalf("message extensions = %v", message.Extensions)
	}
	budgetJSON, err := json.Marshal(message.Metadata[TokenBudgetExtensionURI])
	if err != nil {
		t.Fatalf("Marshal token budget metadata: %v", err)
	}
	if got := string(budgetJSON); got != `{"maxTokens":4096}` {
		t.Fatalf("token budget metadata = %s", got)
	}
}

func TestRemoteCardConditionalRefreshAndTransientFailureRetention(t *testing.T) {
	fixture, client, target := newRemoteContractFixture(t, nil, "")
	fixture.mu.Lock()
	fixture.cacheControl = "no-store, max-age=0" // Refresh cadence is an explicit bridge policy.
	fixture.mu.Unlock()
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("first ResolveAgentCard: %v", err)
	}
	fixture.mu.Lock()
	fixture.return304 = true
	fixture.mu.Unlock()
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("conditional ResolveAgentCard: %v", err)
	}

	fixture.mu.Lock()
	if len(fixture.cardHeaders) != 2 {
		t.Fatalf("card requests = %d", len(fixture.cardHeaders))
	}
	conditional := fixture.cardHeaders[1]
	fixture.mu.Unlock()
	if conditional.Get("If-None-Match") != `"card-v1"` || conditional.Get("If-Modified-Since") == "" {
		t.Fatalf("conditional headers = %v", conditional)
	}
	if !client.IsReady(target) {
		t.Fatal("304 revoked verified card")
	}

	client.remoteHTTPClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("partner unavailable")
	})
	if _, err := client.ResolveAgentCard(t.Context(), target); err == nil {
		t.Fatal("transport failure returned nil error")
	}
	if !client.IsReady(target) {
		t.Fatal("transport failure revoked the periodically verified card")
	}
}

func TestRemoteCardWithdrawalQuarantinesButTransientStatusRetains(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture, client, target := newRemoteContractFixture(t, nil, "")
			if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
				t.Fatalf("initial ResolveAgentCard: %v", err)
			}
			client.mu.RLock()
			oldClient := client.cache[target.ID()].client
			client.mu.RUnlock()
			fixture.mu.Lock()
			fixture.cardStatus = status
			fixture.mu.Unlock()

			_, err := client.ResolveAgentCard(t.Context(), target)
			withdrawn := status == http.StatusNotFound || status == http.StatusGone
			if withdrawn {
				if !errors.Is(err, ErrRemoteTargetUntrusted) || client.IsReady(target) {
					t.Fatalf("withdrawal error = %v, ready = %v", err, client.IsReady(target))
				}
				message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("stale lease"))
				_, err = oldClient.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: message})
				if !errors.Is(err, ErrRemoteTargetUntrusted) {
					t.Fatalf("old generation SendMessage error = %v", err)
				}
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				if len(fixture.callHeaders) != 0 {
					t.Fatal("withdrawn generation crossed the remote transport")
				}
				return
			}
			if err == nil || !client.IsReady(target) {
				t.Fatalf("transient HTTP error = %v, ready = %v", err, client.IsReady(target))
			}
		})
	}
}

func TestRemoteVerifiedReplacementInvalidatesCopiedSDKClient(t *testing.T) {
	fixture, client, target := newRemoteContractFixture(t, nil, "")
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("initial ResolveAgentCard: %v", err)
	}
	client.mu.RLock()
	oldClient := client.cache[target.ID()].client
	client.mu.RUnlock()
	replacementCard := cloneCardForTest(t, fixture.baseCard)
	replacementCard.Description = "Verified replacement"
	replacement := signAgentCard(t, replacementCard, fixture.key, fixture.identity.KeyID, "", nil, nil)
	fixture.setCard(replacement, `"card-v2"`)
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("replacement ResolveAgentCard: %v", err)
	}

	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("stale generation"))
	if _, err := oldClient.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: message}); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("old generation SendMessage error = %v", err)
	}
	if _, err := client.Call(t.Context(), target, "current generation", ""); err != nil {
		t.Fatalf("current generation Call: %v", err)
	}
}

func TestRemoteCardConcurrentRefreshAndCallsRemainAtomic(t *testing.T) {
	fixture, client, target := newRemoteContractFixture(t, nil, "")
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("initial ResolveAgentCard: %v", err)
	}
	fixture.mu.Lock()
	fixture.return304 = true
	fixture.mu.Unlock()

	const workers = 8
	errorsSeen := make(chan error, workers*2)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(2)
		go func() {
			defer wait.Done()
			if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
				errorsSeen <- err
			}
		}()
		go func() {
			defer wait.Done()
			result, err := client.Call(t.Context(), target, "concurrent prompt", "")
			if err != nil {
				errorsSeen <- err
				return
			}
			if result.Text != "remote ack" {
				errorsSeen <- fmt.Errorf("reply = %q", result.Text)
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent operation: %v", err)
	}
	if !client.IsReady(target) {
		t.Fatal("concurrent refresh revoked verified client")
	}
}

func TestRemoteCardTamperQuarantinesAndRecoversWithoutValidators(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	fixture, client, target := newRemoteContractFixture(t, logger, "")
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("initial ResolveAgentCard: %v", err)
	}

	marker := strings.Repeat("REMOTE-CARD-CONTENT-MUST-NOT-BE-LOGGED", 1024)
	tampered := tamperCardField(t, fixture.cardBody, "description", marker)
	fixture.setCard(tampered, `"tampered"`)
	_, err := client.ResolveAgentCard(t.Context(), target)
	if !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("tampered ResolveAgentCard error = %v", err)
	}
	if client.IsReady(target) {
		t.Fatal("tampered card did not quarantine target")
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(logs.String(), marker) {
		t.Fatal("untrusted card content leaked into diagnostics")
	}
	if _, err := client.Call(t.Context(), target, "must not run", ""); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("quarantined Call error = %v", err)
	}
	fixture.mu.Lock()
	if len(fixture.callHeaders) != 0 {
		t.Fatal("quarantined target was delegated to")
	}
	fixture.mu.Unlock()

	valid := signAgentCard(t, fixture.baseCard, fixture.key, fixture.identity.KeyID, "", nil, nil)
	fixture.setCard(valid, `"card-v2"`)
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("recovery ResolveAgentCard: %v", err)
	}
	fixture.mu.Lock()
	recoveryHeaders := fixture.cardHeaders[len(fixture.cardHeaders)-1]
	fixture.mu.Unlock()
	if recoveryHeaders.Get("If-None-Match") != "" || recoveryHeaders.Get("If-Modified-Since") != "" {
		t.Fatalf("quarantine retained validators: %v", recoveryHeaders)
	}
	if !client.IsReady(target) {
		t.Fatal("valid replacement did not recover target")
	}
}

func TestRemoteCardCannotRedirectSDKThroughAlternateEndpointRepresentation(t *testing.T) {
	fixture, client, target := newRemoteContractFixture(t, nil, "")
	alternate := cloneCardForTest(t, fixture.baseCard)
	alternate.SupportedInterfaces[0].URL += "/"
	fixture.setCard(signAgentCard(t, alternate, fixture.key, fixture.identity.KeyID, "", nil, nil), `"alternate-path"`)

	if _, err := client.ResolveAgentCard(t.Context(), target); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("ResolveAgentCard alternate endpoint error = %v", err)
	}
	if _, err := client.Call(t.Context(), target, "must not run", ""); !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("Call alternate endpoint error = %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.callHeaders) != 0 {
		t.Fatalf("alternate endpoint received %d A2A requests, want zero", len(fixture.callHeaders))
	}
}

func TestVerifyRemoteCardContractFailures(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	target, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	base := validRemoteCard(target.String())

	unsignedJSON, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("Marshal unsigned card: %v", err)
	}
	valid := signAgentCard(t, base, key, identity.KeyID, "", nil, nil)
	otherKey := newTestSigningKey(t)
	otherIdentity := identity
	otherIdentity.PublicKeyJWK = testPublicJWK(t, otherKey, identity.KeyID)
	otherTarget, err := NewRemoteTarget(target.String(), otherIdentity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget other key: %v", err)
	}

	tests := []struct {
		name   string
		raw    []byte
		target Target
	}{
		{name: "unsigned", raw: unsignedJSON, target: target},
		{name: "tampered payload", raw: tamperCardField(t, valid, "description", "changed after signing"), target: target},
		{name: "wrong public key", raw: valid, target: otherTarget},
		{name: "wrong key ID", raw: signAgentCard(t, base, key, "other-key", "", nil, nil), target: target},
	}
	missingDescription := agentCardDocument(t, base)
	delete(missingDescription, "description")
	tests = append(tests, struct {
		name   string
		raw    []byte
		target Target
	}{
		name:   "missing required description",
		raw:    signCardDocument(t, missingDescription, cloneDocument(t, missingDescription), key, identity.KeyID, "", nil, nil),
		target: target,
	})
	mutations := []struct {
		name   string
		mutate func(*a2a.AgentCard)
	}{
		{name: "name", mutate: func(card *a2a.AgentCard) { card.Name = "Impostor" }},
		{name: "organization", mutate: func(card *a2a.AgentCard) { card.Provider.Org = "Other Org" }},
		{name: "provider URL empty", mutate: func(card *a2a.AgentCard) { card.Provider.URL = "" }},
		{name: "version empty", mutate: func(card *a2a.AgentCard) { card.Version = "" }},
		{name: "no skills", mutate: func(card *a2a.AgentCard) { card.Skills = nil }},
		{name: "skill ID empty", mutate: func(card *a2a.AgentCard) { card.Skills[0].ID = "" }},
		{name: "skill tags empty", mutate: func(card *a2a.AgentCard) { card.Skills[0].Tags = nil }},
		{name: "duplicate skill IDs", mutate: func(card *a2a.AgentCard) { card.Skills = append(card.Skills, card.Skills[0]) }},
		{name: "no input mode", mutate: func(card *a2a.AgentCard) { card.DefaultInputModes = nil }},
		{name: "no text output", mutate: func(card *a2a.AgentCard) { card.DefaultOutputModes = []string{"application/json"} }},
		{name: "wrong endpoint", mutate: func(card *a2a.AgentCard) { card.SupportedInterfaces[0].URL = "https://other.example/a2a" }},
		{name: "noncanonical endpoint", mutate: func(card *a2a.AgentCard) { card.SupportedInterfaces[0].URL += "/" }},
		{name: "unpinned tenant", mutate: func(card *a2a.AgentCard) { card.SupportedInterfaces[0].Tenant = "other" }},
		{name: "wrong protocol", mutate: func(card *a2a.AgentCard) { card.SupportedInterfaces[0].ProtocolVersion = "0.3" }},
		{name: "wrong binding", mutate: func(card *a2a.AgentCard) { card.SupportedInterfaces[0].ProtocolBinding = a2a.TransportProtocolGRPC }},
		{name: "no extension", mutate: func(card *a2a.AgentCard) { card.Capabilities.Extensions = nil }},
		{name: "unknown required extension", mutate: func(card *a2a.AgentCard) {
			card.Capabilities.Extensions = append(card.Capabilities.Extensions, a2a.AgentExtension{
				URI:      "https://partner.example/a2a/extensions/unknown/v1",
				Required: true,
			})
		}},
	}
	for _, mutation := range mutations {
		card := cloneCardForTest(t, base)
		mutation.mutate(card)
		tests = append(tests, struct {
			name   string
			raw    []byte
			target Target
		}{name: mutation.name, raw: signAgentCard(t, card, key, identity.KeyID, "", nil, nil), target: target})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := verifyRemoteAgentCard(test.raw, test.target); err == nil {
				t.Fatal("verifyRemoteAgentCard succeeded")
			}
		})
	}
}

func TestVerifyRemoteCardAllowsUnknownFieldsOpaqueParamsAndMultipleInterfaces(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	target, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	card := validRemoteCard(target.String())
	card.SupportedInterfaces = append([]*a2a.AgentInterface{
		{
			URL:             "https://other.example/grpc",
			ProtocolBinding: a2a.TransportProtocolGRPC,
			ProtocolVersion: "1.0",
		},
	}, card.SupportedInterfaces...)
	card.Capabilities.Extensions[0].Params = map[string]any{
		"enabled": false,
		"empty":   "",
		"nested":  map[string]any{"limit": json.Number("0")},
	}
	document := agentCardDocument(t, card)
	document["futureSignedField"] = map[string]any{"enabled": false, "values": []any{}}
	raw := signCardDocument(t, document, cloneDocument(t, document), key, identity.KeyID, "", nil, nil)

	verified, err := verifyRemoteAgentCard(raw, target)
	if err != nil {
		t.Fatalf("verifyRemoteAgentCard: %v", err)
	}
	if len(verified.SupportedInterfaces) != 1 || verified.SupportedInterfaces[0].URL != target.String() {
		t.Fatalf("filtered interfaces = %+v", verified.SupportedInterfaces)
	}
	params := verified.Capabilities.Extensions[0].Params
	if enabled, ok := params["enabled"].(bool); !ok || enabled {
		t.Fatalf("opaque params = %#v", params)
	}
}

func TestVerifyRemoteCardPresenceNormalization(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	target, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	wire := agentCardDocument(t, validRemoteCard(target.String()))
	wire["documentationUrl"] = "" // proto optional: explicit default remains signed
	wire["iconUrl"] = ""          // proto optional: explicit default remains signed
	capabilities := wire["capabilities"].(map[string]any)
	capabilities["streaming"] = false // proto optional: explicit default remains signed
	interfaces := wire["supportedInterfaces"].([]any)
	interfaces[0].(map[string]any)["tenant"] = "" // ordinary scalar default is omitted from payload
	wire["securityRequirements"] = nil            // ProtoJSON null represents an unset repeated field
	wire["securitySchemes"] = map[string]any{
		"oauth": map[string]any{
			"oauth2SecurityScheme": map[string]any{
				"description":       "",
				"oauth2MetadataUrl": "",
				"flows": map[string]any{
					"authorizationCode": map[string]any{
						"authorizationUrl": "https://partner.example/authorize",
						"tokenUrl":         "https://partner.example/token",
						"refreshUrl":       "",
						"scopes":           map[string]any{}, // REQUIRED map default remains signed.
						"pkceRequired":     false,
					},
				},
			},
		},
	}
	wire["skills"] = []any{map[string]any{
		"id":          "secured",
		"name":        "Secured skill",
		"description": "Exercises signature presence normalization",
		"tags":        []any{"security"},
		"examples":    []any{},
		"securityRequirements": []any{map[string]any{
			"schemes": map[string]any{
				"oauth": []any{}, // Empty scopes is a meaningful OpenAPI requirement value.
			},
		}},
	}}
	payload := cloneDocument(t, wire)
	payloadInterfaces := payload["supportedInterfaces"].([]any)
	delete(payloadInterfaces[0].(map[string]any), "tenant")
	delete(payload, "securityRequirements")
	payloadOAuth := payload["securitySchemes"].(map[string]any)["oauth"].(map[string]any)["oauth2SecurityScheme"].(map[string]any)
	delete(payloadOAuth, "description")
	delete(payloadOAuth, "oauth2MetadataUrl")
	payloadFlow := payloadOAuth["flows"].(map[string]any)["authorizationCode"].(map[string]any)
	delete(payloadFlow, "refreshUrl")
	delete(payloadFlow, "pkceRequired")
	payloadSkill := payload["skills"].([]any)[0].(map[string]any)
	delete(payloadSkill, "examples")
	raw := signCardDocument(t, wire, payload, key, identity.KeyID, "", nil, nil)
	if _, err := verifyRemoteAgentCard(raw, target); err != nil {
		t.Fatalf("verifyRemoteAgentCard: %v", err)
	}
}

func TestVerifyRemoteCardRejectsUnsupportedJWSHeaders(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	target, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	card := validRemoteCard(target.String())
	tests := []struct {
		name        string
		protected   map[string]any
		unprotected map[string]any
	}{
		{name: "critical", protected: map[string]any{"crit": []string{"future"}, "future": true}},
		{name: "detached payload mode", protected: map[string]any{"b64": false}},
		{name: "unprotected crit", unprotected: map[string]any{"crit": []string{"future"}}},
		{name: "unprotected b64", unprotected: map[string]any{"b64": true}},
		{name: "unprotected jku", unprotected: map[string]any{"jku": "https://attacker.example/keys"}},
		{name: "unprotected typ", unprotected: map[string]any{"typ": "JOSE"}},
		{name: "overlapping custom header", protected: map[string]any{"custom": true}, unprotected: map[string]any{"custom": true}},
		{name: "wrong algorithm", protected: map[string]any{"alg": "RS256"}},
		{name: "wrong type", protected: map[string]any{"typ": "JWT"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := signAgentCard(t, card, key, identity.KeyID, "", test.protected, test.unprotected)
			if _, err := verifyRemoteAgentCard(raw, target); err == nil {
				t.Fatal("verifyRemoteAgentCard succeeded")
			}
		})
	}
}

func TestRemoteCardFetchBoundaries(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)
	tests := []struct {
		name        string
		contentType string
		body        []byte
		redirect    bool
	}{
		{name: "non JSON", contentType: "text/html", body: []byte("<html>not a card</html>")},
		{name: "oversized", contentType: "application/json", body: bytes.Repeat([]byte("x"), maxAgentCardBytes+1)},
		{name: "cross origin redirect", redirect: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var destinationRequests atomic.Int64
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				destinationRequests.Add(1)
			}))
			defer destination.Close()
			mux := http.NewServeMux()
			server := httptest.NewServer(mux)
			defer server.Close()
			mux.HandleFunc("/agent"+remoteAgentCardPath, func(w http.ResponseWriter, req *http.Request) {
				if test.redirect {
					http.Redirect(w, req, destination.URL, http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", test.contentType)
				_, _ = w.Write(test.body)
			})
			target, err := NewRemoteTarget(server.URL+"/agent", identity, 4096)
			if err != nil {
				t.Fatalf("NewRemoteTarget: %v", err)
			}
			client := New("http://local.invalid", "must-not-leak", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if _, err := client.ResolveAgentCard(t.Context(), target); err == nil {
				t.Fatal("ResolveAgentCard succeeded")
			}
			if client.IsReady(target) {
				t.Fatal("invalid response installed a client")
			}
			if destinationRequests.Load() != 0 {
				t.Fatal("cross-origin redirect was followed")
			}
		})
	}
}

func TestRemoteCardMediaTypes(t *testing.T) {
	for _, mediaType := range []string{"application/json", "application/a2a+json; charset=utf-8", "application/vendor+json"} {
		if !isJSONMediaType(mediaType) {
			t.Errorf("isJSONMediaType(%q) = false", mediaType)
		}
	}
	for _, mediaType := range []string{"", "text/json", "text/vendor+json", "application/json; bad"} {
		if isJSONMediaType(mediaType) {
			t.Errorf("isJSONMediaType(%q) = true", mediaType)
		}
	}
}

func TestRemoteTargetErrorsAreStable(t *testing.T) {
	client := New("http://local.invalid", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if client.IsReady(Target{}) {
		t.Fatal("zero target is ready")
	}
	if _, err := client.ResolveAgentCard(t.Context(), Target{}); err == nil {
		t.Fatal("ResolveAgentCard accepted zero target")
	}
	if _, err := client.Call(t.Context(), Target{}, "prompt", ""); err == nil {
		t.Fatal("Call accepted zero target")
	}
	if _, err := client.PollTask(t.Context(), Target{}, "task"); err == nil {
		t.Fatal("PollTask accepted zero target")
	}
}

func ExampleTokenBudgetExtensionURI() {
	fmt.Println(TokenBudgetExtensionURI)
	// Output: https://fgentic.fmind.ai/a2a/extensions/token-budget/v1
}
