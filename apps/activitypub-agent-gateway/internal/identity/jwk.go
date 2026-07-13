package identity

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"fmt"
	"math/big"
)

// PublicKeyJWK renders a P-256 public key as an EC JWK (RFC 7518) for publication in the A2A
// AgentCard, so a verifier can confirm the card's key equals the actor's did:key.
func PublicKeyJWK(pub *ecdsa.PublicKey) (map[string]any, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("identity: public key is not P-256")
	}
	size := (pub.Curve.Params().BitSize + 7) / 8
	return map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(leftPad(pub.X.Bytes(), size)),
		"y":   base64.RawURLEncoding.EncodeToString(leftPad(pub.Y.Bytes(), size)),
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
	// Validate the point is on the curve via crypto/ecdh (the modern, non-deprecated check): an
	// uncompressed SEC1 point 0x04||X||Y is rejected by NewPublicKey if it is not on P-256.
	xb, yb := leftPad(x, 32), leftPad(y, 32)
	uncompressed := append([]byte{0x04}, append(xb, yb...)...)
	if _, err := ecdh.P256().NewPublicKey(uncompressed); err != nil {
		return nil, fmt.Errorf("identity: JWK point is not on the P-256 curve: %w", err)
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
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
