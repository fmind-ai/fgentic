package httpsig

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// rsaPublicPEM returns a fresh RSA public key and its PKIX/SPKI PEM.
func rsaPublicPEM(t *testing.T) (*rsa.PublicKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIX: %v", err)
	}
	return &key.PublicKey, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// spyResolver records whether it was invoked, so a test can prove a pinned actor NEVER hits the
// network fallback.
type spyResolver struct {
	called bool
	result PublicKey
	err    error
}

func (s *spyResolver) Resolve(context.Context, string) (PublicKey, error) {
	s.called = true
	if s.err != nil {
		return PublicKey{}, s.err
	}
	return s.result, nil
}

// jsonPins renders the on-disk {"pins":{actor:pem}} shape, JSON-encoding the PEM newlines.
func jsonPins(m map[string]string) (string, error) {
	data, err := json.Marshal(pinnedKeyFile{Pins: m})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writePins(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write pins: %v", err)
	}
	return path
}

// (a) A pinned actor resolves from the map with ZERO fallback calls.
func TestPinnedResolverPinnedActorSkipsNetwork(t *testing.T) {
	pub, pemText := rsaPublicPEM(t)
	const actor = "https://interop-peer.fgentic.localhost/actor"
	pins, err := jsonPins(map[string]string{actor: pemText})
	if err != nil {
		t.Fatalf("build pins: %v", err)
	}
	spy := &spyResolver{err: errors.New("fallback must not be called for a pinned actor")}
	resolver, err := NewPinnedResolver(writePins(t, pins), spy)
	if err != nil {
		t.Fatalf("NewPinnedResolver: %v", err)
	}
	if resolver.Count() != 1 {
		t.Fatalf("Count = %d, want 1", resolver.Count())
	}
	got, err := resolver.Resolve(context.Background(), actor+"#main-key")
	if err != nil {
		t.Fatalf("Resolve pinned: %v", err)
	}
	if spy.called {
		t.Fatal("fallback resolver was called for a PINNED actor (network fetch must be skipped)")
	}
	if got.Owner != actor {
		t.Fatalf("Owner = %q, want the exact actor URI %q", got.Owner, actor)
	}
	gotKey, ok := got.Key.(*rsa.PublicKey)
	if !ok || !gotKey.Equal(pub) {
		t.Fatal("Resolve returned a key other than the pinned key")
	}
}

// (b) An UNPINNED actor still goes through the guarded HTTPS resolver, and a private/loopback keyId
// is still REJECTED — proving the #320 SSRF guard is intact for anything not explicitly pinned.
func TestPinnedResolverUnpinnedActorStaysGuarded(t *testing.T) {
	_, pemText := rsaPublicPEM(t)
	pins, err := jsonPins(map[string]string{"https://interop-peer.fgentic.localhost/actor": pemText})
	if err != nil {
		t.Fatalf("build pins: %v", err)
	}
	// The REAL guarded HTTP resolver is the fallback here — not a spy — so any unpinned actor is
	// subject to the exact production SSRF guard.
	guarded := NewHTTPKeyResolver(&http.Client{})
	resolver, err := NewPinnedResolver(writePins(t, pins), guarded)
	if err != nil {
		t.Fatalf("NewPinnedResolver: %v", err)
	}
	for _, keyID := range []string{
		"http://10.0.0.5/actor#main-key",          // private, plaintext
		"https://127.0.0.1/actor#main-key",        // loopback
		"https://192.168.1.10/actor#main-key",     // private
		"http://peer.activitypub-interop.svc/a#k", // in-cluster service, plaintext
	} {
		if _, err := resolver.Resolve(context.Background(), keyID); err == nil {
			t.Fatalf("unpinned in-cluster keyId %q was resolved; the SSRF guard must reject it", keyID)
		}
	}
}

// (c) A malformed pinned key fails closed at construction rather than silently falling through to
// the network for a key the operator believed was pinned.
func TestPinnedResolverMalformedKeyFailsClosed(t *testing.T) {
	cases := map[string]string{
		"not a pem":       `{"pins":{"https://peer.example/actor":"-----BEGIN PUBLIC KEY-----\nnonsense\n-----END PUBLIC KEY-----"}}`,
		"empty pem":       `{"pins":{"https://peer.example/actor":""}}`,
		"empty actor uri": `{"pins":{"":"x"}}`,
		"actor with frag": `{"pins":{"https://peer.example/actor#main-key":"x"}}`,
		"no pins":         `{"pins":{}}`,
		"malformed json":  `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			spy := &spyResolver{}
			if _, err := NewPinnedResolver(writePins(t, body), spy); err == nil {
				t.Fatal("NewPinnedResolver accepted an invalid pin; it must fail closed")
			}
		})
	}
	if _, err := NewPinnedResolver(filepath.Join(t.TempDir(), "absent.json"), &spyResolver{}); err == nil {
		t.Fatal("NewPinnedResolver accepted a missing file; it must fail closed")
	}
	if _, err := NewPinnedResolver(writePins(t, `{"pins":{"https://peer.example/actor":"x"}}`), nil); err == nil {
		t.Fatal("NewPinnedResolver accepted a nil fallback; it must fail closed")
	}
}

// (d) A pin is selected only by an EXACT actor match — no prefix/substring bypass — and it can never
// speak for a different actor (Owner is always the pinned actor URI).
func TestPinnedResolverExactActorMatchOnly(t *testing.T) {
	pub, pemText := rsaPublicPEM(t)
	const actor = "https://interop-peer.fgentic.localhost/actor"
	pins, err := jsonPins(map[string]string{actor: pemText})
	if err != nil {
		t.Fatalf("build pins: %v", err)
	}
	for _, keyID := range []string{
		actor + "extra#main-key",                                                  // suffix beyond the exact actor
		"https://interop-peer.fgentic.localhost/actor2#k",                         // sibling path
		"https://evil.example/actor#https://interop-peer.fgentic.localhost/actor", // actor only in fragment
	} {
		spy := &spyResolver{result: PublicKey{Key: pub, Owner: "spoofed"}}
		resolver, err := NewPinnedResolver(writePins(t, pins), spy)
		if err != nil {
			t.Fatalf("NewPinnedResolver: %v", err)
		}
		if _, err := resolver.Resolve(context.Background(), keyID); err != nil {
			t.Fatalf("Resolve %q: %v", keyID, err)
		}
		if !spy.called {
			t.Fatalf("keyId %q matched a pin by prefix/substring; only an exact actor may select a pin", keyID)
		}
	}
	// The exact keyId resolves to the pin and binds Owner to the pinned actor, not the caller.
	spy := &spyResolver{err: errors.New("must not be called")}
	resolver, err := NewPinnedResolver(writePins(t, pins), spy)
	if err != nil {
		t.Fatalf("NewPinnedResolver: %v", err)
	}
	got, err := resolver.Resolve(context.Background(), actor+"#main-key")
	if err != nil {
		t.Fatalf("Resolve exact: %v", err)
	}
	if got.Owner != actor {
		t.Fatalf("Owner = %q, want %q", got.Owner, actor)
	}
}
