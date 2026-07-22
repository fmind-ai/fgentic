package agentcardjws

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/gowebpki/jcs"
)

const p256CoordinateBytes = 32

// Bundle is the pair of public artifacts produced from one card and signing key. Serializing the
// pair into one file lets callers publish it atomically before splitting it at their boundary.
type Bundle struct {
	AgentCard json.RawMessage `json:"agentCard"`
	PublicJWK json.RawMessage `json:"publicJwk"`
}

type protectedHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type publicJWK struct {
	KeyType   string   `json:"kty"`
	Curve     string   `json:"crv"`
	X         string   `json:"x"`
	Y         string   `json:"y"`
	KeyID     string   `json:"kid"`
	Algorithm string   `json:"alg"`
	Use       string   `json:"use"`
	KeyOps    []string `json:"key_ops"`
}

// Signature is the protected ES256 JWS material shared by Signed AgentCards and other
// repository-owned, JCS-canonical evidence artifacts.
type Signature struct {
	Protected string `json:"protected"`
	Signature string `json:"signature"`
}

// PublicJWKMetadataPolicy controls whether JOSE metadata is mandatory (published signing
// artifacts) or may be omitted (operator-pinned coordinates). Present metadata is always
// validated and private or unknown fields are always rejected.
type PublicJWKMetadataPolicy uint8

const (
	// RequirePublicJWKMetadata requires the complete metadata emitted by Sign.
	RequirePublicJWKMetadata PublicJWKMetadataPolicy = iota + 1
	// AllowOptionalJWKMetadata permits coordinate-only operator pins while validating any metadata present.
	AllowOptionalJWKMetadata
)

// ParseP256PrivateKeyPEM parses exactly one unencrypted PKCS#8 or SEC1 ECDSA P-256 private key.
func ParseP256PrivateKeyPEM(raw []byte) (*ecdsa.PrivateKey, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
		return nil, fmt.Errorf("private key is not a PEM block")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil {
		return nil, fmt.Errorf("private key is not a PEM block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("private key PEM contains trailing data or another block")
	}
	if len(block.Headers) != 0 {
		return nil, fmt.Errorf("encrypted or annotated private key PEM is unsupported")
	}

	var key *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 private key is not ECDSA")
		}
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse SEC1 private key: %w", err)
		}
		key = parsed
	default:
		return nil, fmt.Errorf("unsupported private key PEM type %q", block.Type)
	}
	if err := validatePrivateKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

// ParsePublicJWK parses an ES256 public key and binds any present metadata to expectedKeyID.
// Unknown or private fields are rejected so verification never accepts a broader key contract.
func ParsePublicJWK(
	raw []byte,
	expectedKeyID string,
	metadataPolicy PublicJWKMetadataPolicy,
) (*ecdsa.PublicKey, error) {
	if err := validateKeyID(expectedKeyID); err != nil {
		return nil, err
	}
	if metadataPolicy != RequirePublicJWKMetadata && metadataPolicy != AllowOptionalJWKMetadata {
		return nil, fmt.Errorf("unsupported public JWK metadata policy %d", metadataPolicy)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var jwk publicJWK
	if err := decoder.Decode(&jwk); err != nil {
		return nil, fmt.Errorf("decode public JWK: %w", err)
	}
	if err := expectJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode public JWK trailing data: %w", err)
	}
	if _, err := jcs.Transform(raw); err != nil {
		return nil, fmt.Errorf("public JWK is not valid canonicalizable I-JSON")
	}
	if jwk.KeyType != "EC" || jwk.Curve != "P-256" {
		return nil, fmt.Errorf("public JWK must be an EC P-256 key")
	}
	switch metadataPolicy {
	case RequirePublicJWKMetadata:
		if jwk.KeyID != expectedKeyID {
			return nil, fmt.Errorf("public JWK kid does not match expected key ID")
		}
		if jwk.Algorithm != "ES256" || jwk.Use != "sig" {
			return nil, fmt.Errorf("public JWK must be an ES256 signature key")
		}
		if len(jwk.KeyOps) != 1 || jwk.KeyOps[0] != "verify" {
			return nil, fmt.Errorf("public JWK key_ops must contain only verify")
		}
	case AllowOptionalJWKMetadata:
		if jwk.KeyID != "" && jwk.KeyID != expectedKeyID {
			return nil, fmt.Errorf("public JWK kid does not match expected key ID")
		}
		if jwk.Algorithm != "" && jwk.Algorithm != "ES256" {
			return nil, fmt.Errorf("public JWK alg %q is not ES256", jwk.Algorithm)
		}
		if jwk.Use != "" && jwk.Use != "sig" {
			return nil, fmt.Errorf("public JWK use %q is not sig", jwk.Use)
		}
		for _, operation := range jwk.KeyOps {
			if operation != "verify" {
				return nil, fmt.Errorf("public JWK key_ops contains unsupported operation %q", operation)
			}
		}
	}
	xBytes, err := base64.RawURLEncoding.Strict().DecodeString(jwk.X)
	if err != nil || len(xBytes) != p256CoordinateBytes {
		return nil, fmt.Errorf("public JWK x must be a 32-byte base64url coordinate")
	}
	yBytes, err := base64.RawURLEncoding.Strict().DecodeString(jwk.Y)
	if err != nil || len(yBytes) != p256CoordinateBytes {
		return nil, fmt.Errorf("public JWK y must be a 32-byte base64url coordinate")
	}
	// Build the SEC 1 uncompressed point and parse it: ParseUncompressedPublicKey rejects an
	// off-curve or identity point (Go 1.26 deprecated raw big.Int coordinate assignment on
	// ecdsa.PublicKey; parsing the encoded point is the supported, on-curve-checked path).
	encoded := make([]byte, 1+p256CoordinateBytes*2)
	encoded[0] = 4
	copy(encoded[1:1+p256CoordinateBytes], xBytes)
	copy(encoded[1+p256CoordinateBytes:], yBytes)
	// The coordinate lengths are already validated above, so ParseUncompressedPublicKey can only
	// fail here for an off-curve or identity point — keep the original error message for that case.
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), encoded)
	if err != nil {
		return nil, fmt.Errorf("public key point is not on P-256")
	}
	if err := validatePublicKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

// Sign signs an unsigned AgentCard and returns the signed card with its public verification JWK.
func Sign(raw []byte, key *ecdsa.PrivateKey, keyID string) (Bundle, error) {
	if err := validateKeyID(keyID); err != nil {
		return Bundle{}, err
	}
	if err := validatePrivateKey(key); err != nil {
		return Bundle{}, err
	}
	document, err := Parse(raw)
	if err != nil {
		return Bundle{}, err
	}
	if _, present := document.Signatures(); present {
		return Bundle{}, fmt.Errorf("card already contains a signatures field")
	}
	if _, err := document.Card(); err != nil {
		return Bundle{}, err
	}

	signature, err := SignCanonicalPayload(document.payload, key, keyID)
	if err != nil {
		return Bundle{}, fmt.Errorf("sign AgentCard: %w", err)
	}

	signedCard, err := document.marshalWithSignatures([]a2a.AgentCardSignature{{
		Protected: signature.Protected,
		Signature: signature.Signature,
	}})
	if err != nil {
		return Bundle{}, err
	}
	jwk, err := EncodePublicJWK(&key.PublicKey, keyID)
	if err != nil {
		return Bundle{}, err
	}

	// Verify the fully assembled artifact through the same parser/verifier used by consumers.
	signedDocument, err := Parse(signedCard)
	if err != nil {
		return Bundle{}, fmt.Errorf("validate signed AgentCard: %w", err)
	}
	if err := Verify(signedDocument, &key.PublicKey, keyID); err != nil {
		return Bundle{}, fmt.Errorf("validate signed AgentCard: %w", err)
	}
	return Bundle{AgentCard: signedCard, PublicJWK: jwk}, nil
}

// PinnedKey is one currently-valid AgentCard signing key in a rotation overlap window: the protected
// key ID and the ES256 public key that must have produced the signature carrying that ID.
type PinnedKey struct {
	KeyID string
	Key   *ecdsa.PublicKey
}

// ErrRevokedKeyID reports that a card's only recognizable signatures were made under an explicitly
// revoked key ID. It is distinguished from a generic untrusted card so the retired-key case can be
// audited deterministically.
var ErrRevokedKeyID = errors.New("card signed only with a revoked key ID")

// Verify accepts the document when at least one signature is a valid ES256 signature made by key
// under the exact protected key ID. It is the single-key form of VerifySet, preserved for callers
// that pin exactly one key.
func Verify(document *Document, key *ecdsa.PublicKey, keyID string) error {
	return VerifySet(document, []PinnedKey{{KeyID: keyID, Key: key}}, nil)
}

// VerifySet accepts the document when at least one signature is a valid ES256 signature made under a
// pinned key — the multi-key rotation overlap window. During rotation both the old and new keys are
// pinned so a partner that still presents the old key ID keeps verifying; once the old key is retired
// its key ID is added to revoked (and removed from the pinned set) and a card offered under it is
// refused. A card may carry several signatures (old + new during overlap); it is trusted as soon as
// one pinned key verifies, so a stale signature alongside a valid one never blocks it.
//
// Security invariant: a revoked key ID must never also be a pinned key (this fails closed below), so
// acceptance can only ever occur via a currently-pinned, non-revoked key. The revoked set is consulted
// only to attribute the rejection reason AFTER no pinned key has matched — it can never cause acceptance.
func VerifySet(document *Document, keys []PinnedKey, revoked map[string]bool) error {
	if document == nil {
		return fmt.Errorf("card document is nil")
	}
	if len(keys) == 0 {
		return fmt.Errorf("no pinned keys to verify against")
	}
	for _, pinned := range keys {
		if err := validateKeyID(pinned.KeyID); err != nil {
			return err
		}
		if err := validatePublicKey(pinned.Key); err != nil {
			return err
		}
		if revoked[pinned.KeyID] {
			return fmt.Errorf("pinned key ID %q is also marked revoked", pinned.KeyID)
		}
	}
	signatures, present := document.Signatures()
	if !present || len(signatures) == 0 {
		return fmt.Errorf("card is unsigned")
	}
	for _, signature := range signatures {
		for _, pinned := range keys {
			matches, err := verifySignature(document.payload, signature, pinned.KeyID, pinned.Key)
			if err == nil && matches {
				return nil
			}
		}
	}
	// Rejection is already decided. If any signature was offered under a revoked key ID, attribute the
	// revoked reason; this lenient key-ID read is post-decision and cannot affect acceptance.
	for _, signature := range signatures {
		if keyID, ok := signatureKeyID(signature); ok && revoked[keyID] {
			return ErrRevokedKeyID
		}
	}
	return fmt.Errorf("card has no valid ES256 signature for any pinned key ID")
}

// signatureKeyID leniently reads the protected "kid" for post-rejection reason attribution only. It
// returns ("", false) for any malformed header; it is never used to decide acceptance.
func signatureKeyID(signature a2a.AgentCardSignature) (string, bool) {
	protectedJSON, err := base64.RawURLEncoding.Strict().DecodeString(signature.Protected)
	if err != nil {
		return "", false
	}
	var header struct {
		KeyID string `json:"kid"`
	}
	if json.Unmarshal(protectedJSON, &header) != nil || header.KeyID == "" {
		return "", false
	}
	return header.KeyID, true
}

func verifySignature(
	payload []byte,
	signature a2a.AgentCardSignature,
	expectedKeyID string,
	publicKey *ecdsa.PublicKey,
) (bool, error) {
	if len(signature.Header) != 0 {
		return verifySignatureWithHeader(payload, signature, expectedKeyID, publicKey)
	}
	return VerifyCanonicalPayload(payload, Signature{
		Protected: signature.Protected,
		Signature: signature.Signature,
	}, publicKey, expectedKeyID)
}

func verifySignatureWithHeader(
	payload []byte,
	signature a2a.AgentCardSignature,
	expectedKeyID string,
	publicKey *ecdsa.PublicKey,
) (bool, error) {
	protectedJSON, err := base64.RawURLEncoding.Strict().DecodeString(signature.Protected)
	if err != nil {
		return false, fmt.Errorf("JWS protected header is not valid base64url")
	}
	decoder := json.NewDecoder(bytes.NewReader(protectedJSON))
	decoder.UseNumber()
	var protected map[string]any
	if err := decoder.Decode(&protected); err != nil {
		return false, fmt.Errorf("JWS protected header is not a JSON object")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return false, fmt.Errorf("JWS protected header has trailing data")
	}
	if _, err := jcs.Transform(protectedJSON); err != nil {
		return false, fmt.Errorf("JWS protected header is not valid I-JSON")
	}
	if _, exists := signature.Header["crit"]; exists {
		return false, fmt.Errorf("JWS unprotected header contains crit")
	}
	if _, exists := signature.Header["b64"]; exists {
		return false, fmt.Errorf("JWS unprotected header contains b64")
	}
	for _, protectedName := range []string{"alg", "kid", "jku", "typ"} {
		if _, exists := signature.Header[protectedName]; exists {
			return false, fmt.Errorf("JWS parameter must be protected")
		}
	}
	for name := range signature.Header {
		if _, exists := protected[name]; exists {
			return false, fmt.Errorf("JWS protected and unprotected headers overlap")
		}
	}
	if _, exists := protected["crit"]; exists {
		return false, fmt.Errorf("JWS critical extensions are unsupported")
	}
	if _, exists := protected["b64"]; exists {
		return false, fmt.Errorf("JWS b64 mode is unsupported")
	}
	algorithm, ok := protected["alg"].(string)
	if !ok || algorithm != "ES256" {
		return false, fmt.Errorf("JWS alg is not ES256")
	}
	keyID, ok := protected["kid"].(string)
	if !ok || keyID != expectedKeyID {
		return false, fmt.Errorf("JWS kid does not match pinned key ID")
	}
	if typ, exists := protected["typ"]; exists {
		typString, ok := typ.(string)
		if !ok || typString != "JOSE" {
			return false, fmt.Errorf("JWS typ is not JOSE")
		}
	}

	signatureBytes, err := base64.RawURLEncoding.Strict().DecodeString(signature.Signature)
	if err != nil {
		return false, fmt.Errorf("JWS signature is not valid base64url")
	}
	if len(signatureBytes) != p256CoordinateBytes*2 {
		return false, fmt.Errorf("ES256 signature is %d bytes, want 64", len(signatureBytes))
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signature.Protected + "." + encodedPayload))
	r := signatureBytes[:p256CoordinateBytes]
	s := signatureBytes[p256CoordinateBytes:]
	if !ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(r), new(big.Int).SetBytes(s)) {
		return false, fmt.Errorf("ES256 signature verification failed")
	}
	return true, nil
}

// SignCanonicalPayload signs already JCS-canonical JSON bytes with protected ES256 JWS metadata.
func SignCanonicalPayload(payload []byte, key *ecdsa.PrivateKey, keyID string) (Signature, error) {
	if err := validateKeyID(keyID); err != nil {
		return Signature{}, err
	}
	if err := validatePrivateKey(key); err != nil {
		return Signature{}, err
	}
	canonical, err := jcs.Transform(payload)
	if err != nil {
		return Signature{}, fmt.Errorf("payload is not valid canonicalizable I-JSON: %w", err)
	}
	if !bytes.Equal(payload, canonical) {
		return Signature{}, fmt.Errorf("payload is not JCS-canonical")
	}
	protectedJSON, err := json.Marshal(protectedHeader{
		Algorithm: "ES256",
		KeyID:     keyID,
		Type:      "JOSE",
	})
	if err != nil {
		return Signature{}, fmt.Errorf("encode JWS protected header: %w", err)
	}
	protected := base64.RawURLEncoding.EncodeToString(protectedJSON)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(protected + "." + encodedPayload))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return Signature{}, fmt.Errorf("sign canonical payload: %w", err)
	}
	rawSignature := make([]byte, p256CoordinateBytes*2)
	r.FillBytes(rawSignature[:p256CoordinateBytes])
	s.FillBytes(rawSignature[p256CoordinateBytes:])
	return Signature{
		Protected: protected,
		Signature: base64.RawURLEncoding.EncodeToString(rawSignature),
	}, nil
}

// VerifyCanonicalPayload validates protected ES256 JWS material over already JCS-canonical JSON.
func VerifyCanonicalPayload(
	payload []byte,
	signature Signature,
	publicKey *ecdsa.PublicKey,
	expectedKeyID string,
) (bool, error) {
	if err := validatePublicKey(publicKey); err != nil {
		return false, err
	}
	canonical, err := jcs.Transform(payload)
	if err != nil {
		return false, fmt.Errorf("payload is not valid canonicalizable I-JSON: %w", err)
	}
	if !bytes.Equal(payload, canonical) {
		return false, fmt.Errorf("payload is not JCS-canonical")
	}
	return verifySignatureWithHeader(payload, a2a.AgentCardSignature{
		Protected: signature.Protected,
		Signature: signature.Signature,
	}, expectedKeyID, publicKey)
}

// EncodePublicJWK encodes the public half of an ES256 signing key with strict verification metadata.
func EncodePublicJWK(key *ecdsa.PublicKey, keyID string) ([]byte, error) {
	if err := validatePublicKey(key); err != nil {
		return nil, err
	}
	if err := validateKeyID(keyID); err != nil {
		return nil, err
	}
	// Derive the coordinate halves from the validated SEC 1 uncompressed encoding rather than the
	// deprecated raw X/Y big.Int fields; validatePublicKey above already proved the point is on-curve.
	point, err := encodePublicKey(key)
	if err != nil {
		return nil, err
	}
	x := base64.RawURLEncoding.EncodeToString(point[1 : 1+p256CoordinateBytes])
	y := base64.RawURLEncoding.EncodeToString(point[1+p256CoordinateBytes:])
	encoded, err := json.Marshal(publicJWK{
		KeyType:   "EC",
		Curve:     "P-256",
		X:         x,
		Y:         y,
		KeyID:     keyID,
		Algorithm: "ES256",
		Use:       "sig",
		KeyOps:    []string{"verify"},
	})
	if err != nil {
		return nil, fmt.Errorf("encode public JWK: %w", err)
	}
	return encoded, nil
}

func validateKeyID(keyID string) error {
	if keyID == "" || keyID != strings.TrimSpace(keyID) {
		return fmt.Errorf("JWS key ID must not be empty or padded with whitespace")
	}
	if !utf8.ValidString(keyID) {
		return fmt.Errorf("JWS key ID must be valid UTF-8")
	}
	if len(keyID) > 256 {
		return fmt.Errorf("JWS key ID must not exceed 256 bytes")
	}
	for _, value := range keyID {
		if value < 0x20 || value == 0x7f {
			return fmt.Errorf("JWS key ID must not contain control characters")
		}
	}
	return nil
}

func validatePrivateKey(key *ecdsa.PrivateKey) error {
	if key == nil {
		return fmt.Errorf("private key is missing")
	}
	if err := validatePublicKey(&key.PublicKey); err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	// PrivateKey.Bytes returns the fixed-length scalar and fails for a nil, zero, or out-of-range
	// scalar (Go 1.26 deprecated the raw D big.Int); ecdh.NewPrivateKey re-validates P-256 range,
	// preserving the previous Sign/BitLen + round-trip guarantee.
	privateBytes, err := key.Bytes()
	if err != nil {
		return fmt.Errorf("private key scalar is outside the P-256 range")
	}
	ecdhPrivateKey, err := ecdh.P256().NewPrivateKey(privateBytes)
	if err != nil {
		return fmt.Errorf("private key scalar is outside the P-256 range")
	}
	publicBytes, err := encodePublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	if !bytes.Equal(ecdhPrivateKey.PublicKey().Bytes(), publicBytes) {
		return fmt.Errorf("private key public point does not match its scalar")
	}
	return nil
}

func validatePublicKey(key *ecdsa.PublicKey) error {
	if key == nil || key.Curve != elliptic.P256() {
		return fmt.Errorf("public key must be ECDSA P-256")
	}
	if _, err := encodePublicKey(key); err != nil {
		return err
	}
	return nil
}

func encodePublicKey(key *ecdsa.PublicKey) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("public key point is not on P-256")
	}
	// PublicKey.Bytes returns the SEC 1 uncompressed encoding and fails for a nil, invalid,
	// off-curve, or identity point — the same guarantee the previous coordinate range check plus
	// ecdh.NewPublicKey round-trip gave, without touching the Go 1.26-deprecated X/Y fields.
	encoded, err := key.Bytes()
	if err != nil {
		return nil, fmt.Errorf("public key point is not on P-256")
	}
	if len(encoded) != 1+p256CoordinateBytes*2 || encoded[0] != 4 {
		return nil, fmt.Errorf("public key point is not on P-256")
	}
	return encoded, nil
}
