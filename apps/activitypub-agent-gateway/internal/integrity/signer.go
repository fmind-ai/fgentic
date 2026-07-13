package integrity

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

// Signer signs outbound activities with a single platform Ed25519 key. Each agent actor publishes
// that key under its OWN assertionMethod, so the verificationMethod is per-actor (an actor asserts
// the key as its own) even though the key material is shared platform-wide. A per-ghost keyring is a
// clean future extension; a shared key keeps SOPS secret management to one key and is cryptographically
// sound because the platform controls every ghost.
type Signer struct {
	priv        ed25519.PrivateKey
	keyFragment string
	now         func() time.Time
}

// NewSigner builds a Signer from an Ed25519 private key and the fragment used to name the key on
// each actor (verificationMethod = <actorID>#<keyFragment>).
func NewSigner(priv ed25519.PrivateKey, keyFragment string) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("integrity: signing key is not Ed25519")
	}
	if keyFragment == "" {
		return nil, fmt.Errorf("integrity: key fragment is required")
	}
	return &Signer{priv: priv, keyFragment: keyFragment, now: func() time.Time { return time.Now().UTC() }}, nil
}

// KeyFragment is the actor-relative fragment naming the signing key (e.g. "ed25519-key").
func (s *Signer) KeyFragment() string { return s.keyFragment }

// VerificationMethod is the fully-qualified key id for an actor: <actorID>#<keyFragment>.
func (s *Signer) VerificationMethod(actorID string) string {
	return actorID + "#" + s.keyFragment
}

// PublicKeyMultibase is the signer's public key as a did:key Multikey for the actor's
// assertionMethod / publicKeyMultibase.
func (s *Signer) PublicKeyMultibase() string {
	return EncodePublicKeyMultibase(s.priv.Public().(ed25519.PublicKey))
}

// PublicKey is the signer's Ed25519 public key, used to publish the HTTP-Signature `publicKeyPem`
// and to seed the outbound deliverer with the matching private key.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// PrivateKey is the signer's Ed25519 private key, shared with the outbound deliverer so HTTP
// signatures and object proofs use one platform identity.
func (s *Signer) PrivateKey() ed25519.PrivateKey { return s.priv }

// SignActivity attaches a proof to doc in place, using actorID to derive the verificationMethod and
// the signer's clock for the created timestamp.
func (s *Signer) SignActivity(doc map[string]any, actorID string) error {
	return Sign(doc, s.priv, s.VerificationMethod(actorID), s.now())
}

// LoadPrivateKeyPEM parses a PKCS#8 PEM Ed25519 private key from raw bytes. This is the SOPS-backed
// on-disk format (openssl genpkey -algorithm ed25519); the plaintext key never leaves the secret.
func LoadPrivateKeyPEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("integrity: no PEM block in private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("integrity: parse PKCS8 private key: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("integrity: private key is %T, want Ed25519", parsed)
	}
	return priv, nil
}

// LoadSignerFromFile reads a PKCS#8 PEM Ed25519 key from path and returns a Signer.
func LoadSignerFromFile(path, keyFragment string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("integrity: read signing key %s: %w", path, err)
	}
	priv, err := LoadPrivateKeyPEM(raw)
	if err != nil {
		return nil, fmt.Errorf("integrity: %s: %w", strings.TrimSpace(path), err)
	}
	return NewSigner(priv, keyFragment)
}
