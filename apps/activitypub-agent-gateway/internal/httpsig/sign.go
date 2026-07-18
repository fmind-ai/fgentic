package httpsig

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Profile identifies one outbound HTTP-signature wire format.
type Profile string

const (
	// ProfileCavage is the legacy draft profile retained for Fediverse compatibility.
	ProfileCavage Profile = "cavage"
	// ProfileRFC9421 is the standards-track profile preferred for new outbound requests.
	ProfileRFC9421 Profile = "rfc9421"
)

// Sign adds the legacy Cavage signature retained as the Fediverse compatibility fallback.
func Sign(req *http.Request, body []byte, keyID string, signer crypto.Signer, now time.Time) error {
	return SignWithProfile(req, body, keyID, signer, now, ProfileCavage)
}

// SignWithProfile authenticates an outbound request with either RFC 9421 or Cavage. RSA keys use
// RSASSA-PKCS1-v1_5/SHA-256, the Mastodon interoperability baseline; Ed25519 remains supported for
// the gateway's existing sovereign actor key. keyID must resolve to signer's public key.
func SignWithProfile(
	req *http.Request,
	body []byte,
	keyID string,
	signer crypto.Signer,
	now time.Time,
	profile Profile,
) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("httpsig: request and URL are required")
	}
	if signer == nil {
		return fmt.Errorf("httpsig: signing key is required")
	}
	if key, ok := signer.(ed25519.PrivateKey); ok && len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("httpsig: signing key is not a valid Ed25519 private key")
	}
	if keyID == "" || strings.ContainsAny(keyID, "\"\\\r\n") {
		return fmt.Errorf("httpsig: keyID is empty or unsafe")
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	switch profile {
	case ProfileCavage:
		return signCavage(req, body, keyID, signer, now)
	case ProfileRFC9421:
		return signRFC9421(req, body, keyID, signer, now)
	default:
		return fmt.Errorf("httpsig: unsupported signing profile %q", profile)
	}
}

func signCavage(req *http.Request, body []byte, keyID string, signer crypto.Signer, now time.Time) error {
	req.Header.Del("Signature-Input")
	req.Header.Del("Content-Digest")
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
	algorithm, signature, err := signMessage(signer, []byte(strings.Join(lines, "\n")), ProfileCavage)
	if err != nil {
		return err
	}
	req.Header.Set("Signature", strings.Join([]string{
		`keyId="` + keyID + `"`,
		`algorithm="` + algorithm + `"`,
		`headers="` + strings.Join(components, " ") + `"`,
		`signature="` + base64.StdEncoding.EncodeToString(signature) + `"`,
	}, ","))
	return nil
}

func signRFC9421(req *http.Request, body []byte, keyID string, signer crypto.Signer, now time.Time) error {
	req.Header.Del("Date")
	req.Header.Del("Digest")
	sum := sha256.Sum256(body)
	req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
	components := []string{"@method", "@target-uri", "content-digest"}
	algorithm, err := signingAlgorithm(signer, ProfileRFC9421)
	if err != nil {
		return err
	}
	params := `("@method" "@target-uri" "content-digest");created=` +
		fmt.Sprintf("%d", now.UTC().Unix()) + `;keyid="` + keyID + `";alg="` + algorithm + `"`
	parsed := parsedSignature{scheme: "rfc9421", components: components, signatureParams: params}
	signingString, err := parsed.signingStringRFC9421(req)
	if err != nil {
		return fmt.Errorf("httpsig: build RFC 9421 signing string: %w", err)
	}
	_, signature, err := signMessage(signer, []byte(signingString), ProfileRFC9421)
	if err != nil {
		return err
	}
	req.Header.Set("Signature-Input", "sig1="+params)
	req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(signature)+":")
	return nil
}

func signMessage(signer crypto.Signer, message []byte, profile Profile) (string, []byte, error) {
	algorithm, err := signingAlgorithm(signer, profile)
	if err != nil {
		return "", nil, err
	}
	var input []byte
	var opts crypto.SignerOpts
	switch signer.Public().(type) {
	case *rsa.PublicKey:
		digest := sha256.Sum256(message)
		input = digest[:]
		opts = crypto.SHA256
	default:
		input = message
		opts = crypto.Hash(0)
	}
	signature, err := signer.Sign(rand.Reader, input, opts)
	if err != nil {
		return "", nil, fmt.Errorf("httpsig: sign %s request: %w", profile, err)
	}
	return algorithm, signature, nil
}

func signingAlgorithm(signer crypto.Signer, profile Profile) (string, error) {
	switch signer.Public().(type) {
	case *rsa.PublicKey:
		if profile == ProfileRFC9421 {
			return "rsa-v1_5-sha256", nil
		}
		return "rsa-sha256", nil
	case ed25519.PublicKey:
		return "ed25519", nil
	default:
		return "", fmt.Errorf("httpsig: unsupported signing key %T", signer.Public())
	}
}

// EncodePublicKeyPEM encodes a supported public key as an SPKI PEM block for an actor's
// `publicKey.publicKeyPem`, which a remote peer fetches to verify our HTTP signatures.
func EncodePublicKeyPEM(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("httpsig: marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}
