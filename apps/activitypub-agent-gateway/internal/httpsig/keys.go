package httpsig

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// maxKeyDocBytes bounds an untrusted actor/key document fetch.
const maxKeyDocBytes = 1 << 20 // 1 MiB

// LoadRSAPrivateKeyFromFile loads the dedicated HTTP-signature key. Transport authentication uses
// RSA independently from the Ed25519 object-integrity key because RSA PKCS#1 v1.5 with SHA-256 is
// the Fediverse interoperability baseline.
func LoadRSAPrivateKeyFromFile(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read HTTP-signature key %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("decode HTTP-signature key %s: no PEM block", path)
	}

	var key *rsa.PrivateKey
	parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
	if parseErr == nil {
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("parse HTTP-signature key %s: PKCS#8 key is %T, want RSA", path, parsed)
		}
	} else {
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse HTTP-signature key %s as PKCS#8 (%w) or PKCS#1: %w", path, parseErr, err)
		}
	}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("parse HTTP-signature key %s: RSA modulus is %d bits, want at least 2048", path, key.N.BitLen())
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("validate HTTP-signature key %s: %w", path, err)
	}
	return key, nil
}

// HTTPKeyResolver fetches the signing actor's public key from its keyId URL. The fetched key is
// untrusted trust material: the caller still binds the returned owner to the activity actor and
// checks the allowlist before admitting anything.
type HTTPKeyResolver struct {
	client *http.Client
}

// NewHTTPKeyResolver returns a resolver using client (which should carry sane timeouts and,
// in-cluster, an egress NetworkPolicy).
func NewHTTPKeyResolver(client *http.Client) *HTTPKeyResolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPKeyResolver{client: client}
}

// actorKeyDoc is the subset of an actor document carrying its public key (Mastodon/GTS shape).
type actorKeyDoc struct {
	ID        string `json:"id"`
	PublicKey struct {
		ID           string `json:"id"`
		Owner        string `json:"owner"`
		PublicKeyPem string `json:"publicKeyPem"`
	} `json:"publicKey"`
}

// Resolve fetches keyID (its fragment stripped to address the actor document) and returns the
// parsed public key and the actor that owns it.
func (r *HTTPKeyResolver) Resolve(ctx context.Context, keyID string) (PublicKey, error) {
	docURL, _, _ := strings.Cut(keyID, "#")
	if docURL == "" {
		return PublicKey{}, fmt.Errorf("empty keyId")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, docURL, nil)
	if err != nil {
		return PublicKey{}, fmt.Errorf("build key request: %w", err)
	}
	req.Header.Set("Accept", "application/activity+json")
	resp, err := r.client.Do(req)
	if err != nil {
		return PublicKey{}, fmt.Errorf("fetch key %s: %w", docURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return PublicKey{}, fmt.Errorf("fetch key %s: status %d", docURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyDocBytes))
	if err != nil {
		return PublicKey{}, fmt.Errorf("read key %s: %w", docURL, err)
	}
	var doc actorKeyDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return PublicKey{}, fmt.Errorf("decode key %s: %w", docURL, err)
	}
	key, err := ParsePublicKeyPEM(doc.PublicKey.PublicKeyPem)
	if err != nil {
		return PublicKey{}, err
	}
	owner := doc.PublicKey.Owner
	if owner == "" {
		owner = doc.ID
	}
	if owner == "" {
		return PublicKey{}, fmt.Errorf("key %s has no owner", docURL)
	}
	return PublicKey{Key: key, Owner: owner}, nil
}

// ParsePublicKeyPEM decodes a PKIX/SPKI public key PEM block (RSA, Ed25519, or ECDSA).
func ParsePublicKeyPEM(pemText string) (crypto.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("no PEM block in publicKeyPem")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	return key, nil
}
