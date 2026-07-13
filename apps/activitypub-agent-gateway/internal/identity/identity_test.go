package identity

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"
)

// seedKey derives the fixed P-256 public key used to pin the did:key encoding against the Python
// `cryptography` reference (scalar 0x1234…cdef → goldenDID), via crypto/ecdh (not the deprecated
// elliptic low-level API).
func seedKey(t *testing.T) *ecdsa.PublicKey {
	t.Helper()
	scalar, err := hex.DecodeString("1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	if err != nil {
		t.Fatalf("decode scalar: %v", err)
	}
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	raw := priv.PublicKey().Bytes() // uncompressed 0x04||X||Y
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(raw[1:33]), Y: new(big.Int).SetBytes(raw[33:65])}
}

// A did:key is a PUBLIC key, not a secret; the marker keeps the app-level gitleaks (default config)
// from mistaking its high entropy for a credential.
const goldenDID = "did:key:zDnaeVDYyKXvYsYHvLkN2MsDo2A59hpRrDDoCrxeWCofZCEgj" //gitleaks:allow

func TestDIDKeyMatchesReference(t *testing.T) {
	did, err := EncodeP256DIDKey(seedKey(t))
	if err != nil {
		t.Fatalf("EncodeP256DIDKey: %v", err)
	}
	if did != goldenDID {
		t.Fatalf("did:key = %q, want %q (P-256 multicodec drift)", did, goldenDID)
	}
	decoded, err := DecodeP256DIDKey(goldenDID)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !decoded.Equal(seedKey(t)) {
		t.Errorf("decoded did:key does not round-trip")
	}
	if _, err := DecodeP256DIDKey("did:key:zNotAKey"); err == nil {
		t.Errorf("garbage did:key must be rejected")
	}
}

func TestJWKRoundTrip(t *testing.T) {
	pub := seedKey(t)
	jwk, err := PublicKeyJWK(pub)
	if err != nil {
		t.Fatalf("PublicKeyJWK: %v", err)
	}
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		t.Errorf("jwk = %v", jwk)
	}
	back, err := JWKToPublicKey(jwk)
	if err != nil {
		t.Fatalf("JWKToPublicKey: %v", err)
	}
	if !back.Equal(pub) {
		t.Errorf("JWK round-trip mismatch")
	}
}

func TestStatementSignVerifyAndTamper(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	const actor = "https://fgentic.localhost/ap/agents/agent-docs-qa"
	stmt, err := signer.Statement(actor)
	if err != nil {
		t.Fatalf("Statement: %v", err)
	}

	did, _, err := VerifyStatement(stmt, actor)
	if err != nil {
		t.Fatalf("VerifyStatement: %v", err)
	}
	if did != signer.DID() {
		t.Errorf("verified did = %q, want %q", did, signer.DID())
	}

	// Binding to a different actor must fail.
	if _, _, err := VerifyStatement(stmt, "https://evil.example/actor"); err == nil {
		t.Errorf("statement must not verify against a different actor")
	}

	// Tampering the bound actor breaks the proof.
	tampered := cloneStatement(stmt)
	tampered["alsoKnownAs"] = "https://evil.example/actor"
	if _, _, err := VerifyStatement(tampered, "https://evil.example/actor"); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("tampered statement err = %v, want ErrProofInvalid", err)
	}
}

func TestVerifyBindingFailsClosedOnKeyMismatch(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := NewSigner(priv)
	const actor = "https://fgentic.localhost/ap/agents/agent-docs-qa"
	stmt, _ := signer.Statement(actor)

	// Matching card key → binding confirmed.
	jwk, _ := signer.JWK()
	did, err := VerifyBinding(stmt, jwk, actor)
	if err != nil || did != signer.DID() {
		t.Fatalf("VerifyBinding = (%q, %v), want the signer did", did, err)
	}

	// A DIFFERENT card key → fail closed (the two faces are not the same principal).
	otherPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherJWK, _ := PublicKeyJWK(&otherPriv.PublicKey)
	if _, err := VerifyBinding(stmt, otherJWK, actor); !errors.Is(err, ErrBindingMismatch) {
		t.Errorf("mismatched key err = %v, want ErrBindingMismatch", err)
	}

	// A missing statement fails closed.
	if _, err := VerifyBinding(map[string]any{"type": StatementType, "subject": signer.DID(), "alsoKnownAs": actor}, jwk, actor); !errors.Is(err, ErrNoProof) {
		t.Errorf("missing proof err = %v, want ErrNoProof", err)
	}
}

// TestBindingSurvivesDomainMove is the sovereignty property: the key, not the hostname, is the
// anchor. Re-issuing the statement for a new actor URI (a domain move) verifies unchanged and yields
// the SAME did — a verifier who pinned the did still recognizes the principal.
func TestBindingSurvivesDomainMove(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := NewSigner(priv)
	jwk, _ := signer.JWK()

	oldActor := "https://old.example/ap/agents/agent-docs-qa"
	newActor := "https://new.example/ap/agents/agent-docs-qa"

	oldStmt, _ := BuildStatement(priv, signer.DID(), oldActor, time.Unix(1_700_000_000, 0))
	newStmt, _ := BuildStatement(priv, signer.DID(), newActor, time.Unix(1_800_000_000, 0))

	didOld, err := VerifyBinding(oldStmt, jwk, oldActor)
	if err != nil {
		t.Fatalf("old binding: %v", err)
	}
	didNew, err := VerifyBinding(newStmt, jwk, newActor)
	if err != nil {
		t.Fatalf("new binding after domain move: %v", err)
	}
	if didOld != didNew {
		t.Errorf("did changed across domain move: %q != %q (the key must be the anchor)", didOld, didNew)
	}
}

func cloneStatement(s map[string]any) map[string]any {
	out := make(map[string]any, len(s))
	for k, v := range s {
		if m, ok := v.(map[string]any); ok {
			mc := make(map[string]any, len(m))
			for mk, mv := range m {
				mc[mk] = mv
			}
			out[k] = mc
			continue
		}
		out[k] = v
	}
	return out
}

func TestErrorPaths(t *testing.T) {
	// did:key wrong multibase prefix / wrong curve marker.
	if _, err := DecodeP256DIDKey("did:key:xNope"); err == nil {
		t.Errorf("non-z multibase must be rejected")
	}
	// JWK with a bad base64 coordinate.
	if _, err := JWKToPublicKey(map[string]any{"kty": "EC", "crv": "P-256", "x": "!!", "y": "AA"}); err == nil {
		t.Errorf("bad JWK x must be rejected")
	}
	if _, err := JWKToPublicKey(map[string]any{"kty": "RSA"}); err == nil {
		t.Errorf("non-EC JWK must be rejected")
	}
	// proof with non-multibase / wrong-length signature.
	doc := map[string]any{"type": "Note", "proof": map[string]any{
		"type": ProofType, "cryptosuite": Cryptosuite, "verificationMethod": "did:key:zX#zX", "proofValue": "znotasig",
	}}
	pub := seedKey(t)
	if err := verifyProof(doc, pub); err == nil {
		t.Errorf("short signature must be rejected")
	}
	// EncodeP256DIDKey rejects a non-P256 key.
	if _, err := EncodeP256DIDKey(nil); err == nil {
		t.Errorf("nil key must be rejected")
	}
}
