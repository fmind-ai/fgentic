package httpsig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sign adds a Cavage-draft HTTP Message Signature — the format Mastodon and go-ap emit and accept —
// to an OUTBOUND request, covering (request-target), host, date, and a SHA-256 body digest with an
// Ed25519 key. It is the symmetric counterpart of Verifier: keyID must resolve (via the sender's
// actor document `publicKey.publicKeyPem`) to the matching Ed25519 key so a remote peer can verify
// the delivery. Signing outbound fan-out is what lets a sovereign Group actor authenticate to
// followers' inboxes (issue #217).
func Sign(req *http.Request, body []byte, keyID string, priv ed25519.PrivateKey, now time.Time) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("httpsig: signing key is not Ed25519")
	}
	if keyID == "" {
		return fmt.Errorf("httpsig: keyID is required")
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	req.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	sum := sha256.Sum256(body)
	req.Header.Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(sum[:]))

	components := []string{"(request-target)", "host", "date", "digest"}
	lines := []string{
		"(request-target): " + strings.ToLower(req.Method) + " " + req.URL.RequestURI(),
		"host: " + req.Host,
		"date: " + req.Header.Get("Date"),
		"digest: " + req.Header.Get("Digest"),
	}
	signature := ed25519.Sign(priv, []byte(strings.Join(lines, "\n")))
	req.Header.Set("Signature", strings.Join([]string{
		`keyId="` + keyID + `"`,
		`algorithm="ed25519"`,
		`headers="` + strings.Join(components, " ") + `"`,
		`signature="` + base64.StdEncoding.EncodeToString(signature) + `"`,
	}, ","))
	return nil
}

// EncodePublicKeyPEM encodes an Ed25519 public key as an SPKI PEM block for an actor's
// `publicKey.publicKeyPem`, which a remote peer fetches to verify our HTTP signatures.
func EncodePublicKeyPEM(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("httpsig: marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}
