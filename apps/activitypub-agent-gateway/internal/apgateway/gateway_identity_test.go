package apgateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fmind/activitypub-agent-gateway/internal/identity"
)

// TestActorAndCardShareOneIdentity is the FEP-c390 end-to-end proof: the AP actor carries a
// VerifiableIdentityStatement and the A2A AgentCard publishes the matching JWK, so a verifier
// confirms both federation faces share ONE sovereign did:key (issue #218).
func TestActorAndCardShareOneIdentity(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	idSigner, err := identity.NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	g := newTestGateway(t, &fakeDelegator{})
	g.UseIdentity(idSigner)
	const actorURI = "https://fgentic.localhost/ap/agents/agent-docs-qa"

	// The actor attaches a FEP-c390 statement and also-knows-as the did.
	var actor map[string]any
	if err := json.Unmarshal(do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa", "").Body.Bytes(), &actor); err != nil {
		t.Fatalf("unmarshal actor: %v", err)
	}
	attachments, ok := actor["attachment"].([]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("actor attachment = %v", actor["attachment"])
	}
	statement := attachments[0].(map[string]any)
	if aka, _ := actor["alsoKnownAs"].([]any); len(aka) != 1 || aka[0] != idSigner.DID() {
		t.Errorf("actor alsoKnownAs = %v, want the did", actor["alsoKnownAs"])
	}

	// The AgentCard publishes the matching P-256 JWK.
	var card map[string]any
	if err := json.Unmarshal(do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa/agent-card.json", "").Body.Bytes(), &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	cardIdentity, ok := card["identity"].(map[string]any)
	if !ok {
		t.Fatalf("card identity = %v", card["identity"])
	}
	cardJWK := cardIdentity["publicKeyJwk"].(map[string]any)

	// A verifier confirms the bidirectional binding using ONLY the two published faces.
	did, err := identity.VerifyBinding(statement, cardJWK, actorURI)
	if err != nil {
		t.Fatalf("VerifyBinding: %v", err)
	}
	if did != idSigner.DID() {
		t.Errorf("bound did = %q, want %q", did, idSigner.DID())
	}

	// A tampered card key (different principal) must fail closed.
	otherPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherJWK, _ := identity.PublicKeyJWK(&otherPriv.PublicKey)
	if _, err := identity.VerifyBinding(statement, otherJWK, actorURI); err == nil {
		t.Errorf("a card key that does not match the actor did must fail closed")
	}
}

// TestNoIdentityWhenDisabled confirms the actor/card carry no binding without an identity key.
func TestNoIdentityWhenDisabled(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	var actor map[string]any
	_ = json.Unmarshal(do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa", "").Body.Bytes(), &actor)
	if _, present := actor["attachment"]; present {
		t.Errorf("no identity statement should be attached when the anchor is disabled")
	}
	var card map[string]any
	_ = json.Unmarshal(do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa/agent-card.json", "").Body.Bytes(), &card)
	if _, present := card["identity"]; present {
		t.Errorf("card must omit identity when the anchor is disabled")
	}
}
