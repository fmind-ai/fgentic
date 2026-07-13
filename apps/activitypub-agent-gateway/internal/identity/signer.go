package identity

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"
)

// Signer holds the platform's P-256 sovereign key and its did:key. One key anchors both federation
// faces: it signs FEP-c390 statements on the AP actor and is published as the AgentCard's JWK.
type Signer struct {
	priv *ecdsa.PrivateKey
	did  string
	now  func() time.Time
}

// NewSigner builds a Signer from a P-256 private key, deriving its did:key.
func NewSigner(priv *ecdsa.PrivateKey) (*Signer, error) {
	did, err := EncodeP256DIDKey(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Signer{priv: priv, did: did, now: func() time.Time { return time.Now().UTC() }}, nil
}

// DID is the signer's sovereign did:key — the hostname-independent identity anchor.
func (s *Signer) DID() string { return s.did }

// JWK is the signer's public key as an EC P-256 JWK for the AgentCard.
func (s *Signer) JWK() (map[string]any, error) { return PublicKeyJWK(&s.priv.PublicKey) }

// Statement mints a FEP-c390 VerifiableIdentityStatement binding the did to actorURI.
func (s *Signer) Statement(actorURI string) (map[string]any, error) {
	return BuildStatement(s.priv, s.did, actorURI, s.now())
}

// LoadP256FromPEM parses a PKCS#8 PEM P-256 private key (the SOPS-backed on-disk format).
func LoadP256FromPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("identity: no PEM block in private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Fall back to the SEC1 EC form (openssl ecparam default).
		if ecKey, ecErr := x509.ParseECPrivateKey(block.Bytes); ecErr == nil {
			parsed = ecKey
		} else {
			return nil, fmt.Errorf("identity: parse P-256 private key: %w", err)
		}
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: private key is %T, want ECDSA P-256", parsed)
	}
	if priv.Curve.Params().Name != "P-256" {
		return nil, fmt.Errorf("identity: key curve is %s, want P-256", priv.Curve.Params().Name)
	}
	return priv, nil
}

// LoadSignerFromFile reads a PKCS#8 PEM P-256 key from path and returns a Signer.
func LoadSignerFromFile(path string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read key %s: %w", path, err)
	}
	priv, err := LoadP256FromPEM(raw)
	if err != nil {
		return nil, err
	}
	return NewSigner(priv)
}
