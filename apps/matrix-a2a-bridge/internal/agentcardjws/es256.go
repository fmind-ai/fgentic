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

// ParsePublicJWK parses the public artifact emitted by Sign and binds it to expectedKeyID.
// Unknown or private fields are rejected so verification never accepts a broader key contract.
func ParsePublicJWK(raw []byte, expectedKeyID string) (*ecdsa.PublicKey, error) {
	if err := validateKeyID(expectedKeyID); err != nil {
		return nil, err
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
	if jwk.KeyID != expectedKeyID {
		return nil, fmt.Errorf("public JWK kid does not match expected key ID")
	}
	if jwk.Algorithm != "ES256" || jwk.Use != "sig" {
		return nil, fmt.Errorf("public JWK must be an ES256 signature key")
	}
	if len(jwk.KeyOps) != 1 || jwk.KeyOps[0] != "verify" {
		return nil, fmt.Errorf("public JWK key_ops must contain only verify")
	}
	xBytes, err := base64.RawURLEncoding.Strict().DecodeString(jwk.X)
	if err != nil || len(xBytes) != p256CoordinateBytes {
		return nil, fmt.Errorf("public JWK x must be a 32-byte base64url coordinate")
	}
	yBytes, err := base64.RawURLEncoding.Strict().DecodeString(jwk.Y)
	if err != nil || len(yBytes) != p256CoordinateBytes {
		return nil, fmt.Errorf("public JWK y must be a 32-byte base64url coordinate")
	}
	key := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
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

	protectedJSON, err := json.Marshal(protectedHeader{
		Algorithm: "ES256",
		KeyID:     keyID,
		Type:      "JOSE",
	})
	if err != nil {
		return Bundle{}, fmt.Errorf("encode JWS protected header: %w", err)
	}
	protected := base64.RawURLEncoding.EncodeToString(protectedJSON)
	encodedPayload := base64.RawURLEncoding.EncodeToString(document.payload)
	digest := sha256.Sum256([]byte(protected + "." + encodedPayload))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return Bundle{}, fmt.Errorf("sign AgentCard: %w", err)
	}
	signature := make([]byte, p256CoordinateBytes*2)
	r.FillBytes(signature[:p256CoordinateBytes])
	s.FillBytes(signature[p256CoordinateBytes:])

	signedCard, err := document.marshalWithSignatures([]a2a.AgentCardSignature{{
		Protected: protected,
		Signature: base64.RawURLEncoding.EncodeToString(signature),
	}})
	if err != nil {
		return Bundle{}, err
	}
	jwk, err := encodePublicJWK(&key.PublicKey, keyID)
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

// Verify accepts the document when at least one signature is a valid ES256 signature made by
// key under the exact protected key ID. Unsupported signatures are ignored so key rotation can
// publish a multi-signature card without weakening the pinned trust decision.
func Verify(document *Document, key *ecdsa.PublicKey, keyID string) error {
	if document == nil {
		return fmt.Errorf("card document is nil")
	}
	if err := validatePublicKey(key); err != nil {
		return err
	}
	signatures, present := document.Signatures()
	if !present || len(signatures) == 0 {
		return fmt.Errorf("card is unsigned")
	}
	for _, signature := range signatures {
		matches, err := verifySignature(document.payload, signature, keyID, key)
		if err == nil && matches {
			return nil
		}
	}
	return fmt.Errorf("card has no valid ES256 signature for pinned key ID %q", keyID)
}

func verifySignature(
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

func encodePublicJWK(key *ecdsa.PublicKey, keyID string) ([]byte, error) {
	if err := validatePublicKey(key); err != nil {
		return nil, err
	}
	x := base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, p256CoordinateBytes)))
	y := base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, p256CoordinateBytes)))
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
	if key == nil || key.D == nil {
		return fmt.Errorf("private key is missing")
	}
	if err := validatePublicKey(&key.PublicKey); err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	if key.D.Sign() <= 0 || key.D.BitLen() > p256CoordinateBytes*8 {
		return fmt.Errorf("private key scalar is outside the P-256 range")
	}
	privateBytes := key.D.FillBytes(make([]byte, p256CoordinateBytes))
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
	if key == nil || key.Curve != elliptic.P256() || key.X == nil || key.Y == nil {
		return fmt.Errorf("public key must be ECDSA P-256")
	}
	if _, err := encodePublicKey(key); err != nil {
		return err
	}
	return nil
}

func encodePublicKey(key *ecdsa.PublicKey) ([]byte, error) {
	if key == nil || key.X == nil || key.Y == nil ||
		key.X.Sign() < 0 || key.Y.Sign() < 0 ||
		key.X.BitLen() > p256CoordinateBytes*8 || key.Y.BitLen() > p256CoordinateBytes*8 {
		return nil, fmt.Errorf("public key point is not on P-256")
	}
	encoded := make([]byte, 1+p256CoordinateBytes*2)
	encoded[0] = 4 // SEC 1 uncompressed point encoding.
	key.X.FillBytes(encoded[1 : 1+p256CoordinateBytes])
	key.Y.FillBytes(encoded[1+p256CoordinateBytes:])
	if _, err := ecdh.P256().NewPublicKey(encoded); err != nil {
		return nil, fmt.Errorf("public key point is not on P-256")
	}
	return encoded, nil
}
