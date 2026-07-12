package integrity

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mr-tron/base58"
)

// goldenPKCS8PEM builds the seed-00..1f key in PKCS#8 PEM (the on-disk SOPS format) at runtime, so
// no private-key literal is committed. It is the exact material LoadPrivateKeyPEM must accept.
func goldenPKCS8PEM(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(goldenKey(t))
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestSignRejectsEmptyVerificationMethod(t *testing.T) {
	doc := goldenDoc(t)
	delete(doc, "proof")
	if err := Sign(doc, goldenKey(t), "", time.Time{}); err == nil {
		t.Errorf("empty verificationMethod must be rejected")
	}
}

func TestLoadPrivateKeyPEM(t *testing.T) {
	priv, err := LoadPrivateKeyPEM(goldenPKCS8PEM(t))
	if err != nil {
		t.Fatalf("LoadPrivateKeyPEM: %v", err)
	}
	if !priv.Equal(goldenKey(t)) {
		t.Errorf("loaded key does not match the golden seed key")
	}

	if _, err := LoadPrivateKeyPEM([]byte("not a pem")); err == nil {
		t.Errorf("expected error for non-PEM input")
	}
	// A well-formed PEM block whose bytes are not a valid PKCS#8 key must be rejected.
	badPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-der")})
	if _, err := LoadPrivateKeyPEM(badPEM); err == nil {
		t.Errorf("expected error for non-parseable key")
	}
}

func TestLoadSignerFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ed25519.pem")
	if err := os.WriteFile(path, goldenPKCS8PEM(t), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	signer, err := LoadSignerFromFile(path, "ed25519-key")
	if err != nil {
		t.Fatalf("LoadSignerFromFile: %v", err)
	}
	if signer.PublicKeyMultibase() != goldenPublicKeyMultibase {
		t.Errorf("PublicKeyMultibase = %q", signer.PublicKeyMultibase())
	}
	if _, err := LoadSignerFromFile(filepath.Join(t.TempDir(), "missing.pem"), "k"); err == nil {
		t.Errorf("expected error for a missing key file")
	}
}

func TestNewSignerValidation(t *testing.T) {
	priv := goldenKey(t)
	if _, err := NewSigner(priv, ""); err == nil {
		t.Errorf("empty fragment must be rejected")
	}
	if _, err := NewSigner(ed25519.PrivateKey{1, 2, 3}, "k"); err == nil {
		t.Errorf("non-Ed25519 key must be rejected")
	}
	s, err := NewSigner(priv, "ed25519-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if s.KeyFragment() != "ed25519-key" {
		t.Errorf("KeyFragment = %q", s.KeyFragment())
	}
	if vm := s.VerificationMethod("https://x/actor"); vm != "https://x/actor#ed25519-key" {
		t.Errorf("VerificationMethod = %q", vm)
	}
}

func TestSignerSignActivityRoundTrips(t *testing.T) {
	signer, err := NewSigner(goldenKey(t), "ed25519-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	doc := goldenDoc(t)
	delete(doc, "proof")
	if err := signer.SignActivity(doc, "https://fgentic.localhost/ap/agents/agent-docs-qa"); err != nil {
		t.Fatalf("SignActivity: %v", err)
	}
	vm, err := Verify(doc, goldenKey(t).Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vm != "https://fgentic.localhost/ap/agents/agent-docs-qa#ed25519-key" {
		t.Errorf("verificationMethod = %q", vm)
	}
}

func TestMultibaseErrors(t *testing.T) {
	if _, err := DecodePublicKeyMultibase("Xnotz"); err == nil {
		t.Errorf("non-z prefix must be rejected")
	}
	if _, err := DecodePublicKeyMultibase("z" + "0O"); err == nil {
		t.Errorf("invalid base58 must be rejected")
	}
	// A base58btc value whose multicodec is not ed25519-pub must be rejected.
	wrongCodec := "z" + base58.Encode(append([]byte{0x12, 0x00}, make([]byte, 32)...))
	if _, err := DecodePublicKeyMultibase(wrongCodec); err == nil {
		t.Errorf("wrong multicodec must be rejected")
	}
	if _, err := decodeProofValue("Xabc"); err == nil {
		t.Errorf("non-z proofValue must be rejected")
	}
	if _, err := decodeProofValue("z0O"); err == nil {
		t.Errorf("invalid base58 proofValue must be rejected")
	}
}
