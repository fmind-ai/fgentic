package identity

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/gowebpki/jcs"
	"github.com/mr-tron/base58"
)

// FEP-c390 / W3C Data Integrity constants. The cryptosuite is ecdsa-jcs-2019 (P-256 over JCS), the
// ECDSA sibling of the FEP-8b32 eddsa-jcs-2022 path.
const (
	ProofType    = "DataIntegrityProof"
	Cryptosuite  = "ecdsa-jcs-2019"
	ProofPurpose = "assertionMethod"
)

// Sentinel errors let a caller map a verification failure to a stable reason without leaking content.
var (
	ErrNoProof          = errors.New("identity: document has no proof")
	ErrMalformedProof   = errors.New("identity: malformed proof")
	ErrUnsupportedProof = errors.New("identity: unsupported proof type or cryptosuite")
	ErrProofInvalid     = errors.New("identity: proof signature does not verify")
)

// signProof attaches an ecdsa-jcs-2019 DataIntegrityProof to doc in place, signed by priv.
func signProof(doc map[string]any, priv *ecdsa.PrivateKey, verificationMethod string, created time.Time) error {
	proof := map[string]any{
		"type":               ProofType,
		"cryptosuite":        Cryptosuite,
		"proofPurpose":       ProofPurpose,
		"verificationMethod": verificationMethod,
		"created":            created.UTC().Format(time.RFC3339),
	}
	if ctx, ok := doc["@context"]; ok {
		proof["@context"] = ctx
	}
	hash, err := hashData(doc, proof)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(hash)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return fmt.Errorf("identity: ecdsa sign: %w", err)
	}
	proof["proofValue"] = encodeSignature(r, s)
	doc["proof"] = proof
	return nil
}

// verifyProof validates the embedded ecdsa-jcs-2019 proof on doc against pub.
func verifyProof(doc map[string]any, pub *ecdsa.PublicKey) error {
	proof, ok := doc["proof"].(map[string]any)
	if !ok {
		if _, present := doc["proof"]; present {
			return ErrMalformedProof
		}
		return ErrNoProof
	}
	if proof["type"] != ProofType || proof["cryptosuite"] != Cryptosuite {
		return ErrUnsupportedProof
	}
	proofValue, ok := proof["proofValue"].(string)
	if !ok || proofValue == "" {
		return fmt.Errorf("%w: missing proofValue", ErrMalformedProof)
	}
	r, s, err := decodeSignature(proofValue)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedProof, err)
	}
	hash, err := hashData(without(doc, "proof"), without(proof, "proofValue"))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(hash)
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return ErrProofInvalid
	}
	return nil
}

// hashData is the eddsa/ecdsa hashing step: SHA-256(canonical proof config) followed by
// SHA-256(canonical document), proof-config first.
func hashData(unsecured, proofConfig map[string]any) ([]byte, error) {
	docCanon, err := canonicalize(unsecured)
	if err != nil {
		return nil, fmt.Errorf("canonicalize document: %w", err)
	}
	cfgCanon, err := canonicalize(proofConfig)
	if err != nil {
		return nil, fmt.Errorf("canonicalize proof config: %w", err)
	}
	cfgHash := sha256.Sum256(cfgCanon)
	docHash := sha256.Sum256(docCanon)
	return append(cfgHash[:], docHash[:]...), nil
}

func canonicalize(doc map[string]any) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

// encodeSignature encodes an ECDSA (r, s) as multibase base58btc of the fixed-width 64-byte r||s.
func encodeSignature(r, s *big.Int) string {
	raw := make([]byte, 64)
	copy(raw[32-len(r.Bytes()):32], r.Bytes())
	copy(raw[64-len(s.Bytes()):], s.Bytes())
	return "z" + base58.Encode(raw)
}

func decodeSignature(proofValue string) (r, s *big.Int, err error) {
	if len(proofValue) == 0 || proofValue[0] != 'z' {
		return nil, nil, fmt.Errorf("proofValue must be multibase base58btc")
	}
	raw, err := base58.Decode(proofValue[1:])
	if err != nil {
		return nil, nil, fmt.Errorf("base58 decode proofValue: %w", err)
	}
	if len(raw) != 64 {
		return nil, nil, fmt.Errorf("signature is %d bytes, want 64", len(raw))
	}
	return new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:]), nil
}

func without(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}
