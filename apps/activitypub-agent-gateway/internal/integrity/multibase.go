package integrity

import (
	"crypto/ed25519"
	"fmt"

	"github.com/mr-tron/base58"
)

// multibaseBase58BTC is the multibase code point for base58btc ('z'), used for both the proofValue
// and the publicKeyMultibase encodings.
const multibaseBase58BTC = 'z'

// ed25519PubMulticodec is the unsigned-varint multicodec prefix for an Ed25519 public key (0xed).
// A did:key Multikey is base58btc(0xed01 || raw32) with a leading 'z' — canonically "z6Mk…".
var ed25519PubMulticodec = []byte{0xed, 0x01}

// encodeProofValue encodes a raw signature as a multibase base58btc string (no multicodec wrap),
// matching apsig's multibase.encode(signature, "base58btc").
func encodeProofValue(sig []byte) string {
	return string(rune(multibaseBase58BTC)) + base58.Encode(sig)
}

// decodeProofValue reverses encodeProofValue, rejecting any non-base58btc multibase prefix.
func decodeProofValue(s string) ([]byte, error) {
	if len(s) == 0 || s[0] != multibaseBase58BTC {
		return nil, fmt.Errorf("proofValue must be multibase base58btc ('z' prefix)")
	}
	raw, err := base58.Decode(s[1:])
	if err != nil {
		return nil, fmt.Errorf("base58 decode proofValue: %w", err)
	}
	return raw, nil
}

// EncodePublicKeyMultibase encodes an Ed25519 public key as a did:key Multikey (multicodec
// ed25519-pub, multibase base58btc) — the publicKeyMultibase a remote FEP-8b32 verifier consumes
// from the actor's assertionMethod.
func EncodePublicKeyMultibase(pub ed25519.PublicKey) string {
	wrapped := make([]byte, 0, len(ed25519PubMulticodec)+len(pub))
	wrapped = append(wrapped, ed25519PubMulticodec...)
	wrapped = append(wrapped, pub...)
	return string(rune(multibaseBase58BTC)) + base58.Encode(wrapped)
}

// DecodePublicKeyMultibase parses a Multikey back into an Ed25519 public key, rejecting any other
// multibase encoding or multicodec.
func DecodePublicKeyMultibase(s string) (ed25519.PublicKey, error) {
	if len(s) == 0 || s[0] != multibaseBase58BTC {
		return nil, fmt.Errorf("publicKeyMultibase must be base58btc ('z' prefix)")
	}
	raw, err := base58.Decode(s[1:])
	if err != nil {
		return nil, fmt.Errorf("base58 decode publicKeyMultibase: %w", err)
	}
	if len(raw) != len(ed25519PubMulticodec)+ed25519.PublicKeySize ||
		raw[0] != ed25519PubMulticodec[0] || raw[1] != ed25519PubMulticodec[1] {
		return nil, fmt.Errorf("publicKeyMultibase is not an ed25519-pub multicodec key")
	}
	return ed25519.PublicKey(raw[len(ed25519PubMulticodec):]), nil
}
