package integrity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"
)

// FuzzVerifyIntegrityTamper is the object-integrity trust boundary: any activity document whose
// canonical unsigned form differs from a validly signed one must never verify under the signer's
// key. Verify must also never panic on a hostile document. A false accept here would let an
// unauthenticated peer forge a FEP-8b32 object proof.
func FuzzVerifyIntegrityTamper(f *testing.F) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		f.Fatalf("GenerateKey: %v", err)
	}
	signed := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Note",
		"id":       "https://peer.example/notes/1",
		"content":  "hello",
	}
	if err := Sign(signed, priv, "https://peer.example/actor#key", time.Unix(1_700_000_000, 0)); err != nil {
		f.Fatalf("Sign seed doc: %v", err)
	}
	signedJSON, err := json.Marshal(signed)
	if err != nil {
		f.Fatalf("marshal seed doc: %v", err)
	}
	origCanonical, err := canonicalize(without(signed, "proof"))
	if err != nil {
		f.Fatalf("canonicalize seed doc: %v", err)
	}

	f.Add("content", "tampered")
	f.Add("id", "https://peer.example/notes/evil")
	f.Add("type", "Delete")
	f.Add("injected", "value")

	f.Fuzz(func(t *testing.T, field, value string) {
		// Leave proof alone: this target proves payload tampering fails, not forged-proof parsing.
		if field == "" || field == "proof" {
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(signedJSON, &doc); err != nil {
			t.Fatalf("seed doc is not an object: %v", err)
		}
		doc[field] = value
		_, verifyErr := Verify(doc, pub)
		mutatedCanonical, canonErr := canonicalize(without(doc, "proof"))
		if canonErr == nil && bytes.Equal(mutatedCanonical, origCanonical) {
			return // mutation reproduced the exact signed payload; verifying is correct
		}
		if verifyErr == nil {
			t.Fatalf("tampered object verified: field=%q value=%q", field, value)
		}
	})
}

// FuzzDecodeProofValue asserts the multibase proof-value decoder never panics and never returns a
// value alongside an error on hostile input.
func FuzzDecodeProofValue(f *testing.F) {
	for _, seed := range []string{"", "z", "z0", "z11111", "f00", "not-multibase", "z\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		out, err := decodeProofValue(encoded)
		if err != nil && out != nil {
			t.Fatalf("decodeProofValue returned bytes alongside error for %q", encoded)
		}
	})
}
