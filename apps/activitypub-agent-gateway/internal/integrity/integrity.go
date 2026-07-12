// Package integrity implements FEP-8b32 Object Integrity Proofs with the eddsa-jcs-2022
// cryptosuite: an Ed25519 DataIntegrityProof over the JCS-canonicalized (RFC 8785) document. It is
// the ActivityPub twin of the Signed AgentCard's ES256/JCS integrity path (docs/security.md §7,
// docs/fediverse.md §3, issue #212).
//
// HTTP Message Signatures (internal/httpsig) authenticate the transport HOP; a relayed or cached
// activity loses that provenance. An object integrity proof travels WITH the object, so any remote
// verifier can confirm a sovereign agent authored a reply, and an inbound object that was tampered
// after signing fails closed — untrusted room content cannot be laundered through a trusted actor
// (prompt injection is threat #1, docs/security.md).
//
// The algorithm is byte-compatible with the apsig reference implementation (proof-config hash first,
// base58btc multibase proofValue, the proof's @context copied from the document); TestGoldenVector
// pins that interop against an apsig-produced vector. Uses only stdlib crypto/ed25519 — no
// hand-rolled crypto beyond the canonicalization and multibase glue.
package integrity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gowebpki/jcs"
)

// FEP-8b32 / W3C Data Integrity constants. proofPurpose is assertionMethod: the actor asserts it
// authored the object (not, say, key agreement).
const (
	ProofType    = "DataIntegrityProof"
	Cryptosuite  = "eddsa-jcs-2022"
	ProofPurpose = "assertionMethod"

	// ActivityStreamsContext is the base AS2 context; DataIntegrityContext defines the proof and
	// proofValue terms. Signed activities carry both so the proof is a defined term downstream.
	ActivityStreamsContext = "https://www.w3.org/ns/activitystreams"
	DataIntegrityContext   = "https://w3id.org/security/data-integrity/v1"
)

// Sentinel errors let the federation border map a verification failure to a stable, content-free
// reason label without inspecting the activity or leaking its content.
var (
	// ErrNoProof means the document carries no proof property.
	ErrNoProof = errors.New("integrity: document has no proof")
	// ErrMalformedProof means the proof exists but is structurally invalid (bad shape, bad
	// proofValue encoding, wrong signature length, or a missing required field).
	ErrMalformedProof = errors.New("integrity: malformed proof")
	// ErrUnsupportedProof means the proof is not an eddsa-jcs-2022 DataIntegrityProof.
	ErrUnsupportedProof = errors.New("integrity: unsupported proof type or cryptosuite")
	// ErrProofInvalid means canonicalization succeeded but the Ed25519 signature did not verify —
	// the object was tampered with after signing, or signed by a different key.
	ErrProofInvalid = errors.New("integrity: proof signature does not verify")
)

// Sign attaches an eddsa-jcs-2022 DataIntegrityProof to doc in place. doc must not already carry a
// proof. verificationMethod is the URL a verifier dereferences to the signer's Multikey; created is
// stamped as an RFC 3339 (UTC, second-precision) timestamp. The document's @context is copied into
// the proof so proof-config canonicalization matches on both sides.
func Sign(doc map[string]any, priv ed25519.PrivateKey, verificationMethod string, created time.Time) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("integrity: signing key is not Ed25519")
	}
	if verificationMethod == "" {
		return fmt.Errorf("integrity: verificationMethod is required")
	}
	if _, exists := doc["proof"]; exists {
		return fmt.Errorf("integrity: document already carries a proof")
	}

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
	proof["proofValue"] = encodeProofValue(ed25519.Sign(priv, hash))
	doc["proof"] = proof
	return nil
}

// Verify validates the embedded eddsa-jcs-2022 proof on doc against pub and returns the proof's
// verificationMethod. doc is not mutated. The caller (the border) binds the resolved key's owner to
// the activity actor before trusting the object.
func Verify(doc map[string]any, pub ed25519.PublicKey) (verificationMethod string, err error) {
	proof, ok := doc["proof"].(map[string]any)
	if !ok {
		if _, present := doc["proof"]; present {
			return "", ErrMalformedProof
		}
		return "", ErrNoProof
	}
	if proof["type"] != ProofType || proof["cryptosuite"] != Cryptosuite {
		return "", ErrUnsupportedProof
	}
	vm, _ := proof["verificationMethod"].(string)
	if vm == "" {
		return "", fmt.Errorf("%w: missing verificationMethod", ErrMalformedProof)
	}
	proofValue, ok := proof["proofValue"].(string)
	if !ok || proofValue == "" {
		return "", fmt.Errorf("%w: missing proofValue", ErrMalformedProof)
	}
	sig, err := decodeProofValue(proofValue)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrMalformedProof, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: signature is %d bytes, want %d", ErrMalformedProof, len(sig), ed25519.SignatureSize)
	}
	if len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: verifying key is not Ed25519", ErrMalformedProof)
	}

	// Reconstruct exactly what was signed: the document without its proof, and the proof config
	// without its proofValue.
	hash, err := hashData(without(doc, "proof"), without(proof, "proofValue"))
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(pub, hash, sig) {
		return "", ErrProofInvalid
	}
	return vm, nil
}

// hashData is the eddsa-jcs-2022 hashing step: SHA-256(canonical proof config) followed by
// SHA-256(canonical document). The proof-config hash comes first, matching the W3C cryptosuite and
// the apsig reference implementation.
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

// canonicalize serializes a document and applies RFC 8785 JSON Canonicalization.
func canonicalize(doc map[string]any) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

// without returns a shallow copy of m with key removed. Nested values are shared (read-only here).
func without(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}
