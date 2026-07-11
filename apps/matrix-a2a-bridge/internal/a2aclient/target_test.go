package a2aclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func newTestSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

func testPublicJWK(t *testing.T, key *ecdsa.PrivateKey, keyID string) string {
	t.Helper()
	x := key.X.FillBytes(make([]byte, 32))
	y := key.Y.FillBytes(make([]byte, 32))
	value := map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(x),
		"y":   base64.RawURLEncoding.EncodeToString(y),
		"alg": "ES256",
		"use": "sig",
	}
	if keyID != "" {
		value["kid"] = keyID
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal JWK: %v", err)
	}
	return string(raw)
}

func testCardIdentity(t *testing.T, key *ecdsa.PrivateKey) CardIdentity {
	t.Helper()
	return CardIdentity{
		Name:         "Remote contract agent",
		Organization: "Partner Org",
		KeyID:        "partner-key-1",
		PublicKeyJWK: testPublicJWK(t, key, "partner-key-1"),
	}
}

func TestTargetValidationAndIdentity(t *testing.T) {
	key := newTestSigningKey(t)
	identity := testCardIdentity(t, key)

	local, err := NewLocalTarget("/api/a2a/kagent/k8s-agent")
	if err != nil {
		t.Fatalf("NewLocalTarget: %v", err)
	}
	if local.IsRemote() || local.String() != "/api/a2a/kagent/k8s-agent" || local.TokenBudget() != 0 {
		t.Fatalf("local target = %+v", local)
	}

	first, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	second, err := NewRemoteTarget("https://partner.example/a2a", identity, 8192)
	if err != nil {
		t.Fatalf("NewRemoteTarget second budget: %v", err)
	}
	if !first.IsRemote() || first.String() != "https://partner.example/a2a" || first.TokenBudget() != 4096 {
		t.Fatalf("remote target = %+v", first)
	}
	if !first.SameIdentity(second) || first.IdentityFingerprint() != second.IdentityFingerprint() {
		t.Fatal("token budget changed the pinned remote identity")
	}
	if first.ID() == second.ID() {
		t.Fatal("token budget did not change the routing/cache ID")
	}
}

func TestNewLocalTargetRejectsAmbiguousPaths(t *testing.T) {
	for _, raw := range []string{
		"",
		"agent",
		"/",
		"//remote.example/agent",
		"/agent/../other",
		"/agent?tenant=other",
		"/agent?",
		"/agent#fragment",
		"https://remote.example/agent",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NewLocalTarget(raw); err == nil {
				t.Fatalf("NewLocalTarget(%q) succeeded", raw)
			}
		})
	}
}

func TestNewRemoteTargetRejectsUnsafeURLs(t *testing.T) {
	identity := testCardIdentity(t, newTestSigningKey(t))
	for _, raw := range []string{
		"",
		"partner.example/a2a",
		"ftp://partner.example/a2a",
		"https://user:secret@partner.example/a2a",
		"https://partner.example/a2a?tenant=other",
		"https://partner.example/a2a?",
		"https://partner.example/a2a#fragment",
		"https://partner.example/a2a/",
		"https://partner.example/a2a///",
		"https://partner.example/a/../a2a",
		"https://partner.example/a%2Fa",
		"http://partner.example/a2a",
		"http://192.0.2.1/a2a",
		"http://[fe80::1%25eth0]/a2a",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NewRemoteTarget(raw, identity, 4096); err == nil {
				t.Fatalf("NewRemoteTarget(%q) succeeded", raw)
			}
		})
	}
}

func TestNewRemoteTargetAllowsDevelopmentAndClusterHTTP(t *testing.T) {
	identity := testCardIdentity(t, newTestSigningKey(t))
	for _, raw := range []string{
		"http://localhost:8080/a2a",
		"http://127.0.0.1:8080/a2a",
		"http://[::1]:8080/a2a",
		"http://partner-agent:8080/a2a",
		"http://partner-agent.agents.svc:8080/a2a",
		"http://partner-agent.agents.svc.cluster.local:8080/a2a",
		"http://partner-agent.localhost:8080/a2a",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NewRemoteTarget(raw, identity, 4096); err != nil {
				t.Fatalf("NewRemoteTarget(%q): %v", raw, err)
			}
		})
	}
}

func TestNewRemoteTargetRejectsInvalidTrustMaterialWithoutPanicking(t *testing.T) {
	key := newTestSigningKey(t)
	valid := testCardIdentity(t, key)
	cases := []CardIdentity{
		{Name: "", Organization: valid.Organization, KeyID: valid.KeyID, PublicKeyJWK: valid.PublicKeyJWK},
		{Name: " padded ", Organization: valid.Organization, KeyID: valid.KeyID, PublicKeyJWK: valid.PublicKeyJWK},
		{Name: valid.Name, Organization: "", KeyID: valid.KeyID, PublicKeyJWK: valid.PublicKeyJWK},
		{Name: valid.Name, Organization: valid.Organization, KeyID: "", PublicKeyJWK: valid.PublicKeyJWK},
		{Name: valid.Name, Organization: valid.Organization, KeyID: valid.KeyID, PublicKeyJWK: `{}`},
		{
			Name:         valid.Name,
			Organization: valid.Organization,
			KeyID:        valid.KeyID,
			PublicKeyJWK: `{"kty":"EC","crv":"P-256","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
		},
	}
	for index, identity := range cases {
		if _, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096); err == nil {
			t.Errorf("case %d accepted invalid trust material", index)
		}
	}
	if _, err := NewRemoteTarget("https://partner.example/a2a", valid, 0); err == nil {
		t.Fatal("zero token budget accepted")
	}
	if _, err := NewRemoteTarget("https://partner.example/a2a", valid, maxExactJSONInteger+1); err == nil {
		t.Fatal("inexact JSON token budget accepted")
	}
}
