package httpsig

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"
)

// signResolver returns a fixed key/owner for the signer's keyID.
type signResolver struct {
	key   crypto.PublicKey
	owner string
}

func (r signResolver) Resolve(context.Context, string) (PublicKey, error) {
	return PublicKey{Key: r.key, Owner: r.owner}, nil
}

// TestSignRoundTripsThroughVerifier proves an outbound signature this package produces is accepted
// by this package's own Verifier — the wire contract fan-out delivery relies on.
func TestSignRoundTripsThroughVerifier(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const owner = "https://fgentic.localhost/ap/groups/collab"
	body := []byte(`{"type":"Announce"}`)

	req, err := http.NewRequest(http.MethodPost, "https://remote.example/users/bob/inbox", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := Sign(req, body, owner+"#main-key", priv, time.Now()); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	verifier := NewVerifier(signResolver{key: pub, owner: owner}, time.Hour)
	res, err := verifier.Verify(context.Background(), req, body)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Owner != owner {
		t.Errorf("owner = %q", res.Owner)
	}

	// A tampered body must break the digest binding.
	if _, err := verifier.Verify(context.Background(), req, []byte(`{"type":"Delete"}`)); err == nil {
		t.Errorf("verifier must reject a body that does not match the signed digest")
	}
}

func TestSignRejectsBadKey(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://remote.example/inbox", nil)
	if err := Sign(req, nil, "k", ed25519.PrivateKey{1, 2, 3}, time.Now()); err == nil {
		t.Errorf("Sign must reject a non-Ed25519 key")
	}
}

func TestEncodePublicKeyPEMRoundTrips(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pemText, err := EncodePublicKeyPEM(pub)
	if err != nil {
		t.Fatalf("EncodePublicKeyPEM: %v", err)
	}
	parsed, err := ParsePublicKeyPEM(pemText)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}
	if !parsed.(ed25519.PublicKey).Equal(pub) {
		t.Errorf("round-trip key mismatch")
	}
}
