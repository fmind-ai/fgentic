package httpsig

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"testing"
	"time"
)

// signResolver returns a fixed key/owner for the signer's keyID.
type signResolver struct {
	key   crypto.PublicKey
	owner string
}

func TestSignWithProfileRoundTripsRSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const owner = "https://fgentic.localhost/ap/groups/collab"
	body := []byte(`{"type":"Announce"}`)

	for _, profile := range []Profile{ProfileRFC9421, ProfileCavage} {
		t.Run(string(profile), func(t *testing.T) {
			req, reqErr := http.NewRequest(http.MethodPost, "https://remote.example/users/bob/inbox", bytes.NewReader(body))
			if reqErr != nil {
				t.Fatalf("NewRequest: %v", reqErr)
			}
			if signErr := SignWithProfile(req, body, owner+"#main-key", priv, time.Now(), profile); signErr != nil {
				t.Fatalf("SignWithProfile: %v", signErr)
			}

			if profile == ProfileRFC9421 {
				if req.Header.Get("Signature-Input") == "" || req.Header.Get("Content-Digest") == "" {
					t.Fatalf("RFC 9421 headers missing: %v", req.Header)
				}
			} else if req.Header.Get("Digest") == "" || req.Header.Get("Date") == "" {
				t.Fatalf("Cavage headers missing: %v", req.Header)
			}

			verifier := NewVerifier(signResolver{key: &priv.PublicKey, owner: owner}, time.Hour)
			res, verifyErr := verifier.Verify(context.Background(), req, body)
			if verifyErr != nil {
				t.Fatalf("Verify: %v", verifyErr)
			}
			if res.Scheme != string(profile) {
				t.Errorf("scheme = %q, want %q", res.Scheme, profile)
			}
		})
	}
}

func TestSignWithProfileReplacesOtherProfileHeaders(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	req, _ := http.NewRequest(http.MethodPost, "https://remote.example/inbox", nil)
	if err := SignWithProfile(req, nil, "https://sender.example#main-key", priv, time.Now(), ProfileRFC9421); err != nil {
		t.Fatalf("sign RFC 9421: %v", err)
	}
	if err := SignWithProfile(req, nil, "https://sender.example#main-key", priv, time.Now(), ProfileCavage); err != nil {
		t.Fatalf("sign Cavage: %v", err)
	}
	if req.Header.Get("Signature-Input") != "" || req.Header.Get("Content-Digest") != "" {
		t.Errorf("Cavage request retained RFC 9421 headers: %v", req.Header)
	}
	if err := SignWithProfile(req, nil, "https://sender.example#main-key", priv, time.Now(), ProfileRFC9421); err != nil {
		t.Fatalf("re-sign RFC 9421: %v", err)
	}
	if req.Header.Get("Date") != "" || req.Header.Get("Digest") != "" {
		t.Errorf("RFC 9421 request retained Cavage headers: %v", req.Header)
	}
}

func TestRFC9421TargetURIUsesRequestScheme(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://remote.example/inbox?shared=true", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	parsed := parsedSignature{
		components:      []string{"@target-uri"},
		signatureParams: `("@target-uri");created=1;keyid="key";alg="ed25519"`,
	}
	got, err := parsed.signingStringRFC9421(req)
	if err != nil {
		t.Fatalf("signingStringRFC9421: %v", err)
	}
	want := "\"@target-uri\": http://remote.example/inbox?shared=true\n" +
		"\"@signature-params\": " + parsed.signatureParams
	if got != want {
		t.Errorf("signing string = %q, want %q", got, want)
	}
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

func TestSignRejectsUnsafeInput(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := SignWithProfile(nil, nil, "key", priv, time.Now(), ProfileRFC9421); err == nil {
		t.Errorf("SignWithProfile must reject a missing request")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://remote.example/inbox", nil)
	if err := SignWithProfile(req, nil, "bad\"key", priv, time.Now(), ProfileRFC9421); err == nil {
		t.Errorf("SignWithProfile must reject an unsafe keyID")
	}
	if err := SignWithProfile(req, nil, "key", priv, time.Now(), Profile("unknown")); err == nil {
		t.Errorf("SignWithProfile must reject an unknown profile")
	}
	if err := SignWithProfile(req, nil, "key", nil, time.Now(), ProfileRFC9421); err == nil {
		t.Errorf("SignWithProfile must reject a missing signer")
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := SignWithProfile(req, nil, "key", ecdsaKey, time.Now(), ProfileRFC9421); err == nil {
		t.Errorf("SignWithProfile must reject an unsupported signing key")
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
