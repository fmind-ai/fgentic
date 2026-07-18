// Package httpsig verifies inbound ActivityPub HTTP Message Signatures at the federation border.
// It supports both the legacy Cavage draft that Mastodon still emits and RFC 9421, using only the
// Go standard library's crypto (no AGPL dependency, docs/fediverse.md §3).
//
// Verification is the AP twin of Synapse's authenticated-fetch border: an inbound activity is
// accepted only if it carries a signature that verifies against the signing actor's fetched key,
// its body digest matches, and it is not stale. The signing actor's identity (the key owner) is
// returned so the caller can enforce the allowlist and bind it to the activity's actor. Any defect
// fails closed — untrusted remotes never reach an agent (prompt injection is threat #1).
package httpsig

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors let the caller emit content-free evidence with a stable reason.
var (
	ErrNoSignature          = errors.New("no http signature present")
	ErrMalformedSignature   = errors.New("malformed http signature")
	ErrUnsupportedAlgorithm = errors.New("unsupported signature algorithm")
	ErrSignatureInvalid     = errors.New("http signature verification failed")
	ErrDigestMismatch       = errors.New("body digest does not match signed digest")
	ErrStale                = errors.New("http signature timestamp outside the accepted window")
	ErrTimestampRequired    = errors.New("http signature must cover a timestamp")
	ErrMissingCoverage      = errors.New("http signature does not cover the request target")
	ErrKeyResolution        = errors.New("could not resolve the signing key")
)

// MaximumFutureSkew is the production ceiling for tolerating a signer clock ahead of the gateway.
const MaximumFutureSkew = 5 * time.Minute

// PublicKey is a resolved signing key and the actor URI that controls it.
type PublicKey struct {
	Key   crypto.PublicKey
	Owner string
}

// KeyResolver fetches the public key named by a signature keyId.
type KeyResolver interface {
	Resolve(ctx context.Context, keyID string) (PublicKey, error)
}

// Result is a successful verification outcome.
type Result struct {
	KeyID  string
	Owner  string
	Scheme string // "cavage" or "rfc9421"
}

// Verifier checks inbound signatures. maxAge bounds replay of old requests while futureSkew is a
// deliberately smaller allowance for an ahead-of-gateway signer clock; now is test-injectable.
type Verifier struct {
	resolver   KeyResolver
	maxAge     time.Duration
	futureSkew time.Duration
	now        func() time.Time
}

// NewVerifier builds a Verifier with at most five minutes of future clock tolerance. maxAge
// defaults to 12h when non-positive.
func NewVerifier(resolver KeyResolver, maxAge time.Duration) *Verifier {
	return NewVerifierWithFutureSkew(resolver, maxAge, MaximumFutureSkew)
}

// NewVerifierWithFutureSkew builds a Verifier with an explicit future-clock allowance.
func NewVerifierWithFutureSkew(resolver KeyResolver, maxAge, futureSkew time.Duration) *Verifier {
	if maxAge <= 0 {
		maxAge = 12 * time.Hour
	}
	if futureSkew <= 0 || futureSkew > MaximumFutureSkew {
		futureSkew = MaximumFutureSkew
	}
	return &Verifier{resolver: resolver, maxAge: maxAge, futureSkew: futureSkew, now: time.Now}
}

// Verify authenticates req (whose body was already read into body) and returns the signing key's
// owner actor. It fails closed on any missing, malformed, stale, or invalid signature.
func (v *Verifier) Verify(ctx context.Context, req *http.Request, body []byte) (Result, error) {
	var sig *parsedSignature
	var err error
	scheme := "cavage"
	switch {
	case req.Header.Get("Signature-Input") != "":
		scheme = "rfc9421"
		sig, err = parseRFC9421(req.Header)
	case req.Header.Get("Signature") != "":
		sig, err = parseCavage(req.Header.Get("Signature"))
	default:
		return Result{}, ErrNoSignature
	}
	if err != nil {
		return Result{}, err
	}
	if err := checkCoveredComponents(sig); err != nil {
		return Result{}, err
	}
	if err := v.checkFreshness(req, sig); err != nil {
		return Result{}, err
	}
	if err := verifyDigest(req, body, sig, scheme); err != nil {
		return Result{}, err
	}

	pub, err := v.resolver.Resolve(ctx, sig.keyID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrKeyResolution, err)
	}

	signingString, err := sig.signingString(req)
	if err != nil {
		return Result{}, err
	}
	if err := verifySignature(sig.algorithm, pub.Key, []byte(signingString), sig.signature); err != nil {
		return Result{}, err
	}
	return Result{KeyID: sig.keyID, Owner: pub.Owner, Scheme: scheme}, nil
}

// checkCoveredComponents requires the signature to bind the method, path, and authority before
// any key fetch. RFC 9421's @target-uri is accepted as the stronger combined path+authority form.
func checkCoveredComponents(sig *parsedSignature) error {
	switch sig.scheme {
	case "rfc9421":
		targetBound := sig.covers("@target-uri") || (sig.covers("@path") && sig.covers("@authority"))
		if !sig.covers("@method") || !targetBound {
			return ErrMissingCoverage
		}
	case "cavage":
		if !sig.covers("(request-target)") || !sig.covers("host") {
			return ErrMissingCoverage
		}
	default:
		return fmt.Errorf("%w: unknown signature scheme", ErrMalformedSignature)
	}
	return nil
}

// checkFreshness enforces the replay window using the signature's created param (RFC 9421) or the
// Date header (Cavage), whichever the signature covers.
func (v *Verifier) checkFreshness(req *http.Request, sig *parsedSignature) error {
	var ts time.Time
	switch {
	case sig.scheme == "rfc9421" && sig.createdSet:
		ts = sig.created
	case sig.scheme == "cavage" && sig.covers("(created)") && sig.createdSet:
		ts = sig.created
	case sig.covers("date"):
		parsed, err := http.ParseTime(req.Header.Get("Date"))
		if err != nil {
			return fmt.Errorf("%w: unparseable Date header", ErrMalformedSignature)
		}
		ts = parsed
	default:
		return ErrTimestampRequired
	}
	age := v.now().Sub(ts)
	if age > v.maxAge || age < -v.futureSkew {
		return ErrStale
	}
	return nil
}

// verifyDigest requires that a request carrying a body covers a matching body digest, so the
// signed headers cannot be replayed over a swapped payload.
func verifyDigest(req *http.Request, body []byte, sig *parsedSignature, scheme string) error {
	if len(body) == 0 {
		return nil
	}
	if scheme == "rfc9421" {
		return verifyContentDigest(req.Header.Get("Content-Digest"), body, sig)
	}
	return verifyCavageDigest(req.Header.Get("Digest"), body, sig)
}

func verifyCavageDigest(header string, body []byte, sig *parsedSignature) error {
	if !sig.covers("digest") {
		return fmt.Errorf("%w: request body not covered by a signed Digest", ErrDigestMismatch)
	}
	want := "SHA-256=" + base64.StdEncoding.EncodeToString(sha256Sum(body))
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(header)), []byte(want)) != 1 {
		return ErrDigestMismatch
	}
	return nil
}

func verifyContentDigest(header string, body []byte, sig *parsedSignature) error {
	if !sig.covers("content-digest") {
		return fmt.Errorf("%w: request body not covered by a signed Content-Digest", ErrDigestMismatch)
	}
	want := "sha-256=:" + base64.StdEncoding.EncodeToString(sha256Sum(body)) + ":"
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(header)), []byte(want)) != 1 {
		return ErrDigestMismatch
	}
	return nil
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// verifySignature checks signingString against sig using the declared algorithm. RSA PKCS1v15
// (Mastodon), RSA-PSS-SHA512, and Ed25519 are supported; hs2019 is inferred from the key type.
func verifySignature(algorithm string, key crypto.PublicKey, signingString, signature []byte) error {
	alg := strings.ToLower(strings.TrimSpace(algorithm))
	if alg == "hs2019" || alg == "" {
		alg = inferAlgorithm(key)
	}
	switch alg {
	case "rsa-sha256", "rsa-v1_5-sha256":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: rsa algorithm with non-rsa key", ErrUnsupportedAlgorithm)
		}
		digest := sha256.Sum256(signingString)
		if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, digest[:], signature); err != nil {
			return fmt.Errorf("%w: %w", ErrSignatureInvalid, err)
		}
		return nil
	case "rsa-pss-sha512":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: rsa-pss algorithm with non-rsa key", ErrUnsupportedAlgorithm)
		}
		digest := sha512.Sum512(signingString)
		if err := rsa.VerifyPSS(rsaKey, crypto.SHA512, digest[:], signature, nil); err != nil {
			return fmt.Errorf("%w: %w", ErrSignatureInvalid, err)
		}
		return nil
	case "ed25519":
		edKey, ok := key.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("%w: ed25519 algorithm with non-ed25519 key", ErrUnsupportedAlgorithm)
		}
		if !ed25519.Verify(edKey, signingString, signature) {
			return ErrSignatureInvalid
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, algorithm)
	}
}

func inferAlgorithm(key crypto.PublicKey) string {
	switch key.(type) {
	case *rsa.PublicKey:
		return "rsa-pss-sha512"
	case ed25519.PublicKey:
		return "ed25519"
	default:
		return ""
	}
}
