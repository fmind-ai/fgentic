// Package identity implements FEP-c390 identity proofs binding an agent's ActivityPub actor to its
// A2A AgentCard signing key via a P-256 `did:key` (issue #218). The key — not the hostname — is the
// sovereign anchor, so the binding survives a domain move: a verifier who pinned the DID recognizes
// the same principal after the actor URI changes. Go's DID tooling is thin, so this leans on stdlib
// crypto/ecdsa plus a minimal did:key + Data Integrity (ecdsa-jcs-2019) implementation rather than a
// heavy framework (docs/fediverse.md §5).
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"fmt"

	"github.com/mr-tron/base58"
)

// p256Multicodec is the unsigned-varint multicodec prefix for a P-256 public key (code 0x1200 →
// varint 0x80 0x24). A did:key wraps the COMPRESSED point with it under multibase base58btc,
// canonically "did:key:zDn…".
var p256Multicodec = []byte{0x80, 0x24}

const didKeyPrefix = "did:key:z"

// EncodeP256DIDKey renders a P-256 public key as a did:key (multicodec + compressed point,
// multibase base58btc).
func EncodeP256DIDKey(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", fmt.Errorf("identity: public key is not P-256")
	}
	compressed := elliptic.MarshalCompressed(elliptic.P256(), pub.X, pub.Y)
	wrapped := make([]byte, 0, len(p256Multicodec)+len(compressed))
	wrapped = append(wrapped, p256Multicodec...)
	wrapped = append(wrapped, compressed...)
	return didKeyPrefix + base58.Encode(wrapped), nil
}

// DecodeP256DIDKey parses a P-256 did:key back into a public key, rejecting any other method or curve.
func DecodeP256DIDKey(did string) (*ecdsa.PublicKey, error) {
	rest, ok := trimPrefix(did, didKeyPrefix)
	if !ok {
		return nil, fmt.Errorf("identity: not a base58btc did:key")
	}
	raw, err := base58.Decode(rest)
	if err != nil {
		return nil, fmt.Errorf("identity: base58 decode did:key: %w", err)
	}
	if len(raw) < len(p256Multicodec) || raw[0] != p256Multicodec[0] || raw[1] != p256Multicodec[1] {
		return nil, fmt.Errorf("identity: did:key is not a P-256 multicodec key")
	}
	x, y := elliptic.UnmarshalCompressed(elliptic.P256(), raw[len(p256Multicodec):])
	if x == nil {
		return nil, fmt.Errorf("identity: invalid P-256 point in did:key")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

func trimPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}
