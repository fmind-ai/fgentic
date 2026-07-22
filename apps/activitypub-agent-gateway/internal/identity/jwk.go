package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"fmt"
)

// PublicKeyJWK renders a P-256 public key as an EC JWK (RFC 7518) for publication in the A2A
// AgentCard, so a verifier can confirm the card's key equals the actor's did:key.
func PublicKeyJWK(pub *ecdsa.PublicKey) (map[string]any, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("identity: public key is not P-256")
	}
	// Derive the coordinate halves from the SEC 1 uncompressed encoding (PublicKey.Bytes) rather
	// than the Go 1.26-deprecated X/Y big.Int fields; Bytes fails for an invalid point.
	point, err := pub.Bytes()
	if err != nil {
		return nil, fmt.Errorf("identity: encode public key: %w", err)
	}
	size := (pub.Curve.Params().BitSize + 7) / 8
	return map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(point[1 : 1+size]),
		"y":   base64.RawURLEncoding.EncodeToString(point[1+size:]),
	}, nil
}

// JWKToPublicKey parses an EC P-256 JWK back into a public key.
func JWKToPublicKey(jwk map[string]any) (*ecdsa.PublicKey, error) {
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		return nil, fmt.Errorf("identity: JWK is not an EC P-256 key")
	}
	xStr, _ := jwk["x"].(string)
	yStr, _ := jwk["y"].(string)
	x, err := base64.RawURLEncoding.DecodeString(xStr)
	if err != nil {
		return nil, fmt.Errorf("identity: decode JWK x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(yStr)
	if err != nil {
		return nil, fmt.Errorf("identity: decode JWK y: %w", err)
	}
	// ParseUncompressedPublicKey validates the SEC 1 point 0x04||X||Y is on P-256 (rejecting
	// off-curve/identity) and returns the key without touching the deprecated X/Y fields.
	xb, yb := leftPad(x, 32), leftPad(y, 32)
	uncompressed := append([]byte{0x04}, append(xb, yb...)...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), uncompressed)
	if err != nil {
		return nil, fmt.Errorf("identity: JWK point is not on the P-256 curve: %w", err)
	}
	return pub, nil
}

// leftPad zero-extends b to size bytes (fixed-width coordinate encoding).
func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}
