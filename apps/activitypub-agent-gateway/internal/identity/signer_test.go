package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func p256PEM(t *testing.T, priv *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestLoadP256FromPEM(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	loaded, err := LoadP256FromPEM(p256PEM(t, priv))
	if err != nil {
		t.Fatalf("LoadP256FromPEM: %v", err)
	}
	if !loaded.PublicKey.Equal(&priv.PublicKey) {
		t.Errorf("loaded key mismatch")
	}

	if _, err := LoadP256FromPEM([]byte("not a pem")); err == nil {
		t.Errorf("non-PEM input must be rejected")
	}
	// A wrong-curve key (P-384) must be rejected.
	wrong, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := LoadP256FromPEM(p256PEM(t, wrong)); err == nil {
		t.Errorf("a non-P-256 curve must be rejected")
	}
	// A well-formed PEM whose bytes are not a key must be rejected.
	bad := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-der")})
	if _, err := LoadP256FromPEM(bad); err == nil {
		t.Errorf("non-parseable key must be rejected")
	}
}

func TestLoadSignerFromFile(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	path := filepath.Join(t.TempDir(), "identity.pem")
	if err := os.WriteFile(path, p256PEM(t, priv), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	signer, err := LoadSignerFromFile(path)
	if err != nil {
		t.Fatalf("LoadSignerFromFile: %v", err)
	}
	want, _ := EncodeP256DIDKey(&priv.PublicKey)
	if signer.DID() != want {
		t.Errorf("DID = %q, want %q", signer.DID(), want)
	}
	if _, err := LoadSignerFromFile(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Errorf("missing file must error")
	}
}

func TestSignerJWKAndStatement(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := NewSigner(priv)
	jwk, err := signer.JWK()
	if err != nil {
		t.Fatalf("JWK: %v", err)
	}
	if jwk["crv"] != "P-256" {
		t.Errorf("jwk crv = %v", jwk["crv"])
	}
	stmt, err := signer.Statement("https://x/actor")
	if err != nil {
		t.Fatalf("Statement: %v", err)
	}
	if _, _, err := VerifyStatement(stmt, "https://x/actor"); err != nil {
		t.Errorf("signer statement must verify: %v", err)
	}
}

func TestVerifyStatementRejectsMalformed(t *testing.T) {
	cases := map[string]map[string]any{
		"wrong type": {"type": "Note", "subject": goldenDID, "alsoKnownAs": "a"},
		"no subject": {"type": StatementType, "alsoKnownAs": "a"},
		"bad did":    {"type": StatementType, "subject": "did:key:zNope", "alsoKnownAs": "a"},
		"no proof":   {"type": StatementType, "subject": goldenDID, "alsoKnownAs": "a"},
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := VerifyStatement(stmt, "a"); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}
