package a2aclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
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

	first, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096, nil)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	second, err := NewRemoteTarget("https://partner.example/a2a", identity, 8192, nil)
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

func TestRemoteTargetActivatedExtensions(t *testing.T) {
	identity := testCardIdentity(t, newTestSigningKey(t))

	local, err := NewLocalTarget("/api/a2a/kagent/k8s-agent")
	if err != nil {
		t.Fatalf("NewLocalTarget: %v", err)
	}
	if got := local.ActivatedExtensions(); got != nil {
		t.Fatalf("local ActivatedExtensions() = %v, want nil", got)
	}

	base, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096, nil)
	if err != nil {
		t.Fatalf("NewRemoteTarget base: %v", err)
	}
	if got := base.ActivatedExtensions(); len(got) != 1 || got[0] != TokenBudgetExtensionURI {
		t.Fatalf("base ActivatedExtensions() = %v, want only token-budget", got)
	}

	// Config order must not affect the negotiated set or the derived identity: extras are sorted.
	quote := "https://fgentic.fmind.ai/a2a/extensions/skill-quote/v1"
	receipt := "https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1"
	withExtras, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096, []string{receipt, quote})
	if err != nil {
		t.Fatalf("NewRemoteTarget with extensions: %v", err)
	}
	want := []string{TokenBudgetExtensionURI, quote, receipt}
	if got := withExtras.ActivatedExtensions(); !slices.Equal(got, want) {
		t.Fatalf("ActivatedExtensions() = %v, want %v (token-budget first, extras sorted)", got, want)
	}
	if !withExtras.activatesExtension(quote) || withExtras.activatesExtension("https://other.example/x") {
		t.Fatal("activatesExtension did not track the configured allowlist")
	}

	// Extensions are operational config, not identity: same pin, different opaque ID.
	if !base.SameIdentity(withExtras) {
		t.Fatal("extensions changed the pinned remote identity")
	}
	if base.ID() == withExtras.ID() {
		t.Fatal("extensions did not change the routing/cache ID")
	}
	reordered, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096, []string{quote, receipt})
	if err != nil {
		t.Fatalf("NewRemoteTarget reordered: %v", err)
	}
	if reordered.ID() != withExtras.ID() {
		t.Fatal("extension ordering must not affect the target ID")
	}
}

func TestNewRemoteTargetRejectsInvalidExtensions(t *testing.T) {
	identity := testCardIdentity(t, newTestSigningKey(t))
	cases := map[string][]string{
		"empty":        {""},
		"whitespace":   {" https://partner.example/ext "},
		"token-budget": {TokenBudgetExtensionURI},
		"http-scheme":  {"http://partner.example/ext"},
		"no-host":      {"https:///ext"},
		"duplicate":    {"https://partner.example/ext", "https://partner.example/ext"},
	}
	for name, extensions := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096, extensions); err == nil {
				t.Fatalf("NewRemoteTarget accepted invalid extensions %q", extensions)
			}
		})
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
			if _, err := NewRemoteTarget(raw, identity, 4096, nil); err == nil {
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
			if _, err := NewRemoteTarget(raw, identity, 4096, nil); err != nil {
				t.Fatalf("NewRemoteTarget(%q): %v", raw, err)
			}
		})
	}
}

func TestNormalizeRemoteURLTransportPolicy(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "public HTTPS", url: "https://partner.example/a2a", want: true},
		{name: "IPv4 loopback", url: "http://127.0.0.1:8080/a2a", want: true},
		{name: "IPv6 loopback", url: "http://[::1]:8080/a2a", want: true},
		{name: "localhost subdomain", url: "http://fixture.localhost:8080/a2a", want: true},
		{name: "single-label service", url: "http://a2a-stub:8080/a2a", want: true},
		{name: "uppercase service", url: "http://A2A-STUB:8080/a2a", want: true},
		{name: "service namespace", url: "http://a2a-stub.default.svc:8080/a2a", want: true},
		{name: "cluster-local service", url: "http://a2a-stub.default.svc.cluster.local:8080/a2a", want: true},
		{name: "public cleartext", url: "http://partner.example/a2a"},
		{name: "localhost lookalike", url: "http://localhost.evil.example/a2a"},
		{name: "svc lookalike", url: "http://a2a-stub.default.svc.evil/a2a"},
		{name: "underscore service", url: "http://a2a_stub:8080/a2a"},
		{name: "numeric service", url: "http://123:8080/a2a"},
		{name: "leading hyphen", url: "http://-agent:8080/a2a"},
		{name: "trailing hyphen", url: "http://agent-.default.svc:8080/a2a"},
		{name: "long label", url: "http://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:8080/a2a"},
		{name: "missing authority", url: "https:///a2a"},
		{name: "credentials", url: "https://user:secret@partner.example/a2a"},
		{name: "query", url: "https://partner.example/a2a?tenant=other"},
		{name: "non-HTTP scheme", url: "ftp://a2a-stub/a2a"},
		{name: "leading whitespace", url: " https://partner.example/a2a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeRemoteURL(tt.url)
			if (err == nil) != tt.want {
				t.Fatalf("NormalizeRemoteURL(%q) = %q, %v; want valid=%v", tt.url, got, err, tt.want)
			}
			if tt.want && got != tt.url {
				t.Fatalf("NormalizeRemoteURL(%q) = %q, want canonical input unchanged", tt.url, got)
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
		if _, err := NewRemoteTarget("https://partner.example/a2a", identity, 4096, nil); err == nil {
			t.Errorf("case %d accepted invalid trust material", index)
		}
	}
	if _, err := NewRemoteTarget("https://partner.example/a2a", valid, 0, nil); err == nil {
		t.Fatal("zero token budget accepted")
	}
	if _, err := NewRemoteTarget("https://partner.example/a2a", valid, maxExactJSONInteger+1, nil); err == nil {
		t.Fatal("inexact JSON token budget accepted")
	}
}

func TestNewRemoteTargetKeyRotationOverlapAndRevocation(t *testing.T) {
	oldKey := newTestSigningKey(t)
	newKey := newTestSigningKey(t)

	// Overlap window: primary = old key, additional = new key. Both are currently valid.
	overlapIdentity := CardIdentity{
		Name:         "Remote contract agent",
		Organization: "Partner Org",
		KeyID:        "partner-key-old",
		PublicKeyJWK: testPublicJWK(t, oldKey, "partner-key-old"),
		AdditionalKeys: []CardKey{{
			KeyID:        "partner-key-new",
			PublicKeyJWK: testPublicJWK(t, newKey, "partner-key-new"),
		}},
	}
	overlapTarget, err := NewRemoteTarget("https://partner.example/a2a", overlapIdentity, 4096, nil)
	if err != nil {
		t.Fatalf("NewRemoteTarget overlap: %v", err)
	}
	base := validRemoteCard(overlapTarget.String())

	// A card under EITHER the old or the new key verifies during the overlap — no rotation gap.
	if _, err := verifyRemoteAgentCard(signAgentCard(t, base, oldKey, "partner-key-old", "", nil, nil), overlapTarget); err != nil {
		t.Fatalf("overlap old key: %v", err)
	}
	if _, err := verifyRemoteAgentCard(signAgentCard(t, base, newKey, "partner-key-new", "", nil, nil), overlapTarget); err != nil {
		t.Fatalf("overlap new key: %v", err)
	}

	// After retiring the old key: primary = new key, old key revoked. A card under the old key is refused
	// with the revoked reason; a card under the new key still verifies.
	retiredIdentity := CardIdentity{
		Name:          "Remote contract agent",
		Organization:  "Partner Org",
		KeyID:         "partner-key-new",
		PublicKeyJWK:  testPublicJWK(t, newKey, "partner-key-new"),
		RevokedKeyIDs: []string{"partner-key-old"},
	}
	retiredTarget, err := NewRemoteTarget("https://partner.example/a2a", retiredIdentity, 4096, nil)
	if err != nil {
		t.Fatalf("NewRemoteTarget retired: %v", err)
	}
	retiredBase := validRemoteCard(retiredTarget.String())
	if _, err := verifyRemoteAgentCard(signAgentCard(t, retiredBase, oldKey, "partner-key-old", "", nil, nil), retiredTarget); !errors.Is(err, agentcardjws.ErrRevokedKeyID) {
		t.Fatalf("retired old key: want ErrRevokedKeyID, got %v", err)
	}
	if _, err := verifyRemoteAgentCard(signAgentCard(t, retiredBase, newKey, "partner-key-new", "", nil, nil), retiredTarget); err != nil {
		t.Fatalf("new key after retirement: %v", err)
	}

	// Construction fails closed: a key ID that is both pinned and revoked.
	if _, err := NewRemoteTarget("https://partner.example/a2a", CardIdentity{
		Name: "Remote contract agent", Organization: "Partner Org",
		KeyID: "partner-key-new", PublicKeyJWK: testPublicJWK(t, newKey, "partner-key-new"),
		RevokedKeyIDs: []string{"partner-key-new"},
	}, 4096, nil); err == nil {
		t.Fatal("NewRemoteTarget accepted a key ID that is both pinned and revoked")
	}
	// Construction fails closed: a duplicate pinned key ID.
	if _, err := NewRemoteTarget("https://partner.example/a2a", CardIdentity{
		Name: "Remote contract agent", Organization: "Partner Org",
		KeyID: "partner-key-dup", PublicKeyJWK: testPublicJWK(t, oldKey, "partner-key-dup"),
		AdditionalKeys: []CardKey{{KeyID: "partner-key-dup", PublicKeyJWK: testPublicJWK(t, newKey, "partner-key-dup")}},
	}, 4096, nil); err == nil {
		t.Fatal("NewRemoteTarget accepted a duplicate pinned key ID")
	}
}
