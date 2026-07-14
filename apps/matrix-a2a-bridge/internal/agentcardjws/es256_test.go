package agentcardjws

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
)

func TestParseP256PrivateKeyPEM(t *testing.T) {
	p256 := testPrivateKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(p256)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	sec1, err := x509.MarshalECPrivateKey(p256)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey P-384: %v", err)
	}
	p384DER, err := x509.MarshalPKCS8PrivateKey(p384)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey P-384: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey RSA: %v", err)
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey RSA: %v", err)
	}

	tests := []struct {
		name    string
		pem     []byte
		wantErr string
	}{
		{name: "PKCS8", pem: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})},
		{name: "SEC1", pem: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})},
		{name: "P384", pem: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p384DER}), wantErr: "P-256"},
		{name: "RSA", pem: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER}), wantErr: "not ECDSA"},
		{name: "encrypted type", pem: pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: pkcs8}), wantErr: "unsupported"},
		{
			name: "legacy encryption headers",
			pem: pem.EncodeToMemory(&pem.Block{
				Type:    "EC PRIVATE KEY",
				Bytes:   sec1,
				Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"},
			}),
			wantErr: "encrypted or annotated",
		},
		{name: "leading data", pem: append([]byte("not pem\n"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})...), wantErr: "not a PEM block"},
		{name: "extra block", pem: append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})...), wantErr: "another block"},
		{name: "malformed", pem: []byte("-----BEGIN PRIVATE KEY-----\nnot-base64\n-----END PRIVATE KEY-----"), wantErr: "not a PEM block"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, err := ParseP256PrivateKeyPEM(test.pem)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseP256PrivateKeyPEM: %v", err)
				}
				if key.D.Cmp(p256.D) != 0 {
					t.Fatal("parsed private scalar differs")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ParseP256PrivateKeyPEM() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestSignVerifyAndPublicJWK(t *testing.T) {
	key := testPrivateKey(t)
	bundle, err := Sign(testCardJSON(t), key, "card-key-2026")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	var jwk map[string]any
	if err := json.Unmarshal(bundle.PublicJWK, &jwk); err != nil {
		t.Fatalf("Unmarshal public JWK: %v", err)
	}
	if _, exists := jwk["d"]; exists {
		t.Fatal("public JWK contains private scalar d")
	}
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" || jwk["alg"] != "ES256" || jwk["use"] != "sig" {
		t.Fatalf("public JWK = %#v", jwk)
	}
	publicKey, err := ParsePublicJWK(bundle.PublicJWK, "card-key-2026", RequirePublicJWKMetadata)
	if err != nil {
		t.Fatalf("ParsePublicJWK: %v", err)
	}
	document, err := Parse(bundle.AgentCard)
	if err != nil {
		t.Fatalf("Parse signed card: %v", err)
	}
	if err := Verify(document, publicKey, "card-key-2026"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	signatures, present := document.Signatures()
	if !present || len(signatures) != 1 {
		t.Fatalf("signatures = %d, present %v", len(signatures), present)
	}
	protectedJSON, err := base64.RawURLEncoding.Strict().DecodeString(signatures[0].Protected)
	if err != nil {
		t.Fatalf("Decode protected header: %v", err)
	}
	if string(protectedJSON) != `{"alg":"ES256","kid":"card-key-2026","typ":"JOSE"}` {
		t.Fatalf("protected header = %s", protectedJSON)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(signatures[0].Signature)
	if err != nil {
		t.Fatalf("Decode signature: %v", err)
	}
	if len(signature) != 64 {
		t.Fatalf("signature length = %d, want 64", len(signature))
	}

	tampered := decodeTestObject(t, bundle.AgentCard)
	tampered["name"] = "tampered after signing"
	tamperedDocument, err := Parse(encodeTestObject(t, tampered))
	if err != nil {
		t.Fatalf("Parse tampered card: %v", err)
	}
	if err := Verify(tamperedDocument, publicKey, "card-key-2026"); err == nil {
		t.Fatal("Verify accepted a tampered card")
	}
	if err := Verify(document, &testPrivateKey(t).PublicKey, "card-key-2026"); err == nil {
		t.Fatal("Verify accepted the wrong public key")
	}
	if err := Verify(document, publicKey, "wrong-key"); err == nil {
		t.Fatal("Verify accepted the wrong key ID")
	}
}

func TestSignRejectsExistingSignaturesAndInvalidKey(t *testing.T) {
	wire := decodeTestObject(t, testCardJSON(t))
	wire["signatures"] = nil
	if _, err := Sign(encodeTestObject(t, wire), testPrivateKey(t), "card-key"); err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("Sign existing signatures error = %v", err)
	}

	key := testPrivateKey(t)
	key.X = new(big.Int).Add(key.X, big.NewInt(1))
	if _, err := Sign(testCardJSON(t), key, "card-key"); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("Sign inconsistent key error = %v", err)
	}
}

func TestParsePublicJWKRejectsPrivateOrMismatchedMaterial(t *testing.T) {
	bundle, err := Sign(testCardJSON(t), testPrivateKey(t), "card-key")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := ParsePublicJWK(bundle.PublicJWK, "other-key", RequirePublicJWKMetadata); err == nil {
		t.Fatal("ParsePublicJWK accepted a mismatched key ID")
	}
	privateJWK := decodeTestObject(t, bundle.PublicJWK)
	privateJWK["d"] = "private"
	if _, err := ParsePublicJWK(encodeTestObject(t, privateJWK), "card-key", RequirePublicJWKMetadata); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("ParsePublicJWK private d error = %v", err)
	}
	duplicate := bytes.Replace(bundle.PublicJWK, []byte(`"kid":`), []byte(`"kid":"attacker","kid":`), 1)
	if _, err := ParsePublicJWK(duplicate, "card-key", RequirePublicJWKMetadata); err == nil || !strings.Contains(err.Error(), "canonicalizable I-JSON") {
		t.Fatalf("ParsePublicJWK duplicate kid error = %v", err)
	}
}

func TestParsePublicJWKOptionalMetadataPolicy(t *testing.T) {
	bundle, err := Sign(testCardJSON(t), testPrivateKey(t), "card-key")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	minimal := decodeTestObject(t, bundle.PublicJWK)
	delete(minimal, "kid")
	delete(minimal, "alg")
	delete(minimal, "use")
	delete(minimal, "key_ops")
	minimalRaw := encodeTestObject(t, minimal)
	if _, err := ParsePublicJWK(minimalRaw, "card-key", AllowOptionalJWKMetadata); err != nil {
		t.Fatalf("ParsePublicJWK optional metadata: %v", err)
	}
	if _, err := ParsePublicJWK(minimalRaw, "card-key", RequirePublicJWKMetadata); err == nil {
		t.Fatal("strict metadata policy accepted a coordinate-only JWK")
	}

	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "mismatched kid", field: "kid", value: "other-key"},
		{name: "wrong algorithm", field: "alg", value: "ES384"},
		{name: "wrong use", field: "use", value: "enc"},
		{name: "unsafe operation", field: "key_ops", value: []string{"sign"}},
		{name: "mixed operations", field: "key_ops", value: []string{"verify", "sign"}},
		{name: "private scalar", field: "d", value: "private"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jwk := decodeTestObject(t, minimalRaw)
			jwk[test.field] = test.value
			if _, err := ParsePublicJWK(encodeTestObject(t, jwk), "card-key", AllowOptionalJWKMetadata); err == nil {
				t.Fatalf("optional metadata policy accepted %s", test.name)
			}
		})
	}
	if _, err := ParsePublicJWK(minimalRaw, "card-key", PublicJWKMetadataPolicy(255)); err == nil {
		t.Fatal("ParsePublicJWK accepted an unknown metadata policy")
	}
}
