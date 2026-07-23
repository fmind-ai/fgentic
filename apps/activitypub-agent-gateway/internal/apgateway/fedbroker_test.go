package apgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/delivery"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/testhttp"
)

func TestParseAcctHandleRejectsInvalidDomainLabels(t *testing.T) {
	for _, handle := range []string{
		"acct:agent@-peer.example.com",
		"acct:agent@peer-.example.com",
		"acct:agent@peer..example.com",
	} {
		if _, _, err := parseAcctHandle(handle); err == nil {
			t.Fatalf("parseAcctHandle(%q) succeeded", handle)
		}
	}
}

func TestValidAcctDomainRejectsProtocolBounds(t *testing.T) {
	for _, domain := range []string{
		strings.Repeat("a", 254),
		strings.Repeat("a", 64) + ".example.com",
		"peer_example.com",
	} {
		if validAcctDomain(domain) {
			t.Fatalf("validAcctDomain(%q) = true", domain)
		}
	}
}

func TestFediverseBrokerDeliversSignedActivityPubFallback(t *testing.T) {
	const host = "fallback.example.com"
	actorID := testhttp.URL(host) + "/users/agent"
	remotePub, remotePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey remote: %v", err)
	}
	remoteVM := actorID + "#ed25519-key"
	actor := map[string]any{
		"@context": []any{integrity.ActivityStreamsContext, integrity.DataIntegrityContext},
		"id":       actorID, "type": "Service", "inbox": actorID + "/inbox",
	}
	if err := integrity.Sign(actor, remotePriv, remoteVM, time.Now().UTC()); err != nil {
		t.Fatalf("Sign remote actor: %v", err)
	}
	var delivered []byte
	var deliveryHeaders http.Header
	deliveryStatus := http.StatusAccepted
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/webfinger":
			writeJSON(w, map[string]any{
				"subject": "acct:agent@" + host,
				"links":   []any{map[string]any{"rel": "self", "type": activityJSONMediaType, "href": actorID}},
			})
		case "/users/agent":
			writeJSON(w, actor)
		case "/users/agent/inbox":
			delivered, _ = io.ReadAll(r.Body)
			deliveryHeaders = r.Header.Clone()
			w.WriteHeader(deliveryStatus)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	client := testhttp.Client(t, map[string]*httptest.Server{host: peer})

	localPub, localPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey local: %v", err)
	}
	signer, err := integrity.NewSigner(localPriv, "ed25519-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	httpKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey HTTP: %v", err)
	}
	g := newTestGateway(t, &fakeDelegator{})
	g.UseSigner(signer)
	if err := g.UseDelivery(delivery.New(client, httpKey, g.log), client); err != nil {
		t.Fatalf("UseDelivery: %v", err)
	}
	if err := g.UseFediverseBroker("broker-secret", client); err != nil {
		t.Fatalf("UseFediverseBroker: %v", err)
	}
	payload, err := json.Marshal(fedBrokerRequest{
		Handle: "acct:agent@" + host,
		Identity: FediverseIdentity{
			ActorID: actorID, VerificationMethod: remoteVM, PublicKeyMultibase: integrity.EncodePublicKeyMultibase(remotePub), ProofMaxAgeSeconds: 86400,
		},
		Sender: "@alice:local.example", MessageID: "matrix-event-1", Text: "check the deployment",
	})
	if err != nil {
		t.Fatalf("Marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/delegate", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec := httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	if len(delivered) == 0 || deliveryHeaders.Get("Signature-Input") == "" || deliveryHeaders.Get("Content-Digest") == "" {
		t.Fatalf("delivery headers = %v, body bytes = %d", deliveryHeaders, len(delivered))
	}
	if deliveryHeaders.Get("Authorization") != "" {
		t.Fatalf("remote Authorization = %q, want empty", deliveryHeaders.Get("Authorization"))
	}
	var activity map[string]any
	if err := json.Unmarshal(delivered, &activity); err != nil {
		t.Fatalf("decode delivered activity: %v", err)
	}
	if _, err := integrity.Verify(activity, localPub); err != nil {
		t.Fatalf("verify delivered proof: %v", err)
	}
	object := activity["object"].(map[string]any)
	if content := object["content"].(string); !strings.Contains(content, "@alice:local.example") || !strings.Contains(content, "check the deployment") {
		t.Fatalf("content = %q", content)
	}

	// A remote 5xx is ambiguous and must not be reported as delivered.
	deliveryStatus = http.StatusInternalServerError
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/delegate", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec = httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failed delivery code = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestFediverseBrokerResolvesPinnedActivityPubFallback(t *testing.T) {
	const host = "peer.example.com"
	actorID := testhttp.URL(host) + "/users/agent"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	vm := actorID + "#ed25519-key"
	actor := map[string]any{
		"@context": []any{integrity.ActivityStreamsContext, integrity.DataIntegrityContext},
		"id":       actorID,
		"type":     "Service",
		"name":     "Peer agent",
		"summary":  "Pinned AP fallback",
		"inbox":    actorID + "/inbox",
	}
	signedAt := time.Now().UTC()
	if err := integrity.Sign(actor, priv, vm, signedAt); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	peer := discoveryServer(t, host, actor)

	g := newTestGateway(t, &fakeDelegator{})
	g.fedBrokerToken = "broker-secret"
	g.fedBrokerClient = testhttp.Client(t, map[string]*httptest.Server{host: peer})
	pinned := FediverseIdentity{
		ActorID: actorID, VerificationMethod: vm, PublicKeyMultibase: integrity.EncodePublicKeyMultibase(pub), ProofMaxAgeSeconds: 86400,
	}
	if _, err := g.resolveFediverse(t.Context(), "acct:agent@peer.example.com", pinned); err != nil {
		t.Fatalf("resolveFediverse: %v", err)
	}
	body := brokerBody(t, "acct:agent@peer.example.com", pinned)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec := httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	var resolved FediverseResolution
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resolved.Transport != brokerTransportAP || resolved.ActorID != actorID || resolved.Inbox != actorID+"/inbox" {
		t.Fatalf("resolution = %+v", resolved)
	}

	// The mapping's explicit freshness ceiling is part of the trust decision.
	g.now = func() time.Time { return signedAt.Add(25 * time.Hour) }
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec = httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expired proof code = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	g.now = func() time.Time { return signedAt }

	// The handle alone is not trust: changing the operator pin fails closed.
	tampered := brokerBody(t, "acct:agent@peer.example.com", FediverseIdentity{
		ActorID: actorID, VerificationMethod: vm, PublicKeyMultibase: integrity.EncodePublicKeyMultibase(make(ed25519.PublicKey, ed25519.PublicKeySize)), ProofMaxAgeSeconds: 86400,
	})
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/resolve", strings.NewReader(tampered))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec = httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("tampered pin code = %d, want %d", rec.Code, http.StatusBadGateway)
	}

	// A matching actor document without its proof is still untrusted.
	proof := actor["proof"]
	delete(actor, "proof")
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec = httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("missing proof code = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	actor["proof"] = proof
}

func TestFediverseBrokerPrefersAdvertisedA2A(t *testing.T) {
	const host = "hybrid.example.com"
	actorID := testhttp.URL(host) + "/users/agent"
	actor := map[string]any{
		"@context": integrity.ActivityStreamsContext,
		"id":       actorID,
		"type":     "Service",
		"inbox":    actorID + "/inbox",
		"implements": []any{map[string]any{
			"name":      "A2A",
			"href":      testhttp.URL(host) + "/a2a/agent",
			"agentCard": testhttp.URL(host) + "/a2a/agent/.well-known/agent-card.json",
		}},
	}
	peer := discoveryServer(t, host, actor)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	g := newTestGateway(t, &fakeDelegator{})
	g.fedBrokerToken = "broker-secret"
	g.fedBrokerClient = testhttp.Client(t, map[string]*httptest.Server{host: peer})
	pinned := FediverseIdentity{
		ActorID: actorID, VerificationMethod: actorID + "#key", PublicKeyMultibase: integrity.EncodePublicKeyMultibase(priv.Public().(ed25519.PublicKey)), ProofMaxAgeSeconds: 86400,
	}
	if _, err := g.resolveFediverse(t.Context(), "acct:agent@hybrid.example.com", pinned); err != nil {
		t.Fatalf("resolveFediverse: %v", err)
	}
	body := brokerBody(t, "acct:agent@hybrid.example.com", pinned)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/resolve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec := httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	var resolved FediverseResolution
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resolved.Transport != brokerTransportA2A || resolved.A2AEndpoint != testhttp.URL(host)+"/a2a/agent" {
		t.Fatalf("resolution = %+v", resolved)
	}

	// Delegation never silently falls back when current discovery advertises A2A. The bridge must
	// refresh and verify the Signed AgentCard before it can send.
	delegate, err := json.Marshal(fedBrokerRequest{
		Handle: "acct:agent@" + host, Identity: pinned,
		Sender: "@alice:local.example", MessageID: "matrix-event-1", Text: "use A2A",
	})
	if err != nil {
		t.Fatalf("Marshal delegate: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/delegate", strings.NewReader(string(delegate)))
	req.Header.Set("Authorization", "Bearer broker-secret")
	rec = httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delegate code = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestFediverseBrokerRequiresBearerBeforeDiscovery(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	g.fedBrokerToken = "broker-secret"
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/resolve", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	g.FediverseBrokerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestFediverseBrokerRejectsInvalidDelegateBeforeDiscovery(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	g.fedBrokerToken = "broker-secret"
	for _, body := range []string{
		`{`,
		`{"handle":"acct:agent@peer.example.com","activityPubIdentity":{}}`,
		`{"handle":"acct:agent@peer.example.com","activityPubIdentity":{},"sender":"@alice:local.example","messageId":"m","text":"x","unknown":true}`,
		`{"handle":"acct:agent@peer.example.com","activityPubIdentity":{},"sender":"@alice:local.example","messageId":"m","text":"x"} {}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/fediverse/delegate", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer broker-secret")
		rec := httptest.NewRecorder()
		g.FediverseBrokerHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q code = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestWriteJSONStatusHandlesEncodingFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONStatus(rec, make(chan int), http.StatusConflict)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestUseFediverseBrokerRequiresTokenAndDeliveryKeys(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	if err := g.UseFediverseBroker(" broker-secret", http.DefaultClient); err == nil {
		t.Fatal("UseFediverseBroker accepted a token with surrounding whitespace")
	}
	if err := g.UseFediverseBroker("broker-secret", http.DefaultClient); err == nil {
		t.Fatal("UseFediverseBroker accepted missing delivery keys")
	}
}

func TestValidateFediversePinRejectsIncompleteTrust(t *testing.T) {
	validActor := "https://peer.example.com/users/agent"
	validKey := integrity.EncodePublicKeyMultibase(make(ed25519.PublicKey, ed25519.PublicKeySize))
	for name, pin := range map[string]FediverseIdentity{
		"insecure actor": {
			ActorID: "http://peer.example.com/users/agent", VerificationMethod: "http://peer.example.com/users/agent#key",
			PublicKeyMultibase: validKey, ProofMaxAgeSeconds: 60,
		},
		"foreign verification method": {
			ActorID: validActor, VerificationMethod: "https://other.example.com/users/agent#key",
			PublicKeyMultibase: validKey, ProofMaxAgeSeconds: 60,
		},
		"invalid multikey": {
			ActorID: validActor, VerificationMethod: validActor + "#key",
			PublicKeyMultibase: "not-a-multikey", ProofMaxAgeSeconds: 60,
		},
		"zero age": {
			ActorID: validActor, VerificationMethod: validActor + "#key",
			PublicKeyMultibase: validKey, ProofMaxAgeSeconds: 0,
		},
		"excessive age": {
			ActorID: validActor, VerificationMethod: validActor + "#key",
			PublicKeyMultibase: validKey, ProofMaxAgeSeconds: int64((31 * 24 * time.Hour) / time.Second),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateFediversePin(pin); err == nil {
				t.Fatal("validateFediversePin accepted invalid trust material")
			}
		})
	}
}

func TestMarshalFediverseDelegationCarriesProofAndMatrixAttribution(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := integrity.NewSigner(priv, "ed25519-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	g := newTestGateway(t, &fakeDelegator{})
	g.UseSigner(signer)
	activityID, raw, err := g.marshalFediverseDelegation(fedBrokerRequest{
		Handle: "acct:agent@peer.example", Sender: "@alice:fgentic.example", MessageID: "matrix-event-1", Text: `review <script>alert("x")</script>`,
	}, FediverseResolution{ActorID: "https://peer.example/users/agent"})
	if err != nil {
		t.Fatalf("marshalFediverseDelegation: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode activity: %v", err)
	}
	if doc["id"] != activityID {
		t.Fatalf("id = %v, want %s", doc["id"], activityID)
	}
	if _, err := integrity.Verify(doc, pub); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	object := doc["object"].(map[string]any)
	if content := object["content"].(string); !strings.Contains(content, "@alice:fgentic.example") ||
		!strings.Contains(content, `review &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`) ||
		strings.Contains(content, "<script>") {
		t.Fatalf("content = %q", content)
	}
}

func discoveryServer(t *testing.T, host string, actor map[string]any) *httptest.Server {
	t.Helper()
	actorID := testhttp.URL(host) + "/users/agent"
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/webfinger":
			writeJSON(w, map[string]any{
				"subject": "acct:agent@" + host,
				"links":   []any{map[string]any{"rel": "self", "type": activityJSONMediaType, "href": actorID}},
			})
		case "/users/agent":
			writeJSON(w, actor)
		default:
			http.NotFound(w, r)
		}
	}))
}

func brokerBody(t *testing.T, handle string, identity FediverseIdentity) string {
	t.Helper()
	raw, err := json.Marshal(fedBrokerRequest{Handle: handle, Identity: identity})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(raw)
}
