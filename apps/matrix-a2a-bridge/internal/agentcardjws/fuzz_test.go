package agentcardjws

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// fuzzKey returns a deterministic-per-run P-256 key and its verification key ID. The tamper and
// no-panic properties below are key-independent, so a fresh key per fuzz run is safe and any real
// defect reproduces regardless of the key.
func fuzzKey(tb testing.TB) (*ecdsa.PrivateKey, string) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("GenerateKey: %v", err)
	}
	return key, "es256:fuzz"
}

func fuzzUnsignedCard(tb testing.TB) []byte {
	tb.Helper()
	card := &a2a.AgentCard{
		Name:        "Fuzz card fixture",
		Description: "Exercises the AgentCard JWS contract under fuzzing",
		Provider:    &a2a.AgentProvider{Org: "Fgentic tests", URL: "https://fgentic.example"},
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface("https://agents.example/a2a/docs", a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       a2a.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID: "docs", Name: "Docs", Description: "Answers one prompt", Tags: []string{"documentation"},
		}},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		tb.Fatalf("Marshal AgentCard: %v", err)
	}
	return raw
}

// FuzzParse asserts the AgentCard parser never panics on arbitrary input and that a successful
// parse yields internally consistent accessors that also never panic.
func FuzzParse(f *testing.F) {
	key, keyID := fuzzKey(f)
	unsigned := fuzzUnsignedCard(f)
	f.Add(unsigned)
	if bundle, err := Sign(unsigned, key, keyID); err == nil {
		f.Add([]byte(bundle.AgentCard))
	}
	for _, seed := range []string{"", "{}", "[]", "null", "{\"name\":1}", "{\"signatures\":[]}", "{not json"} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		document, err := Parse(raw)
		if err != nil {
			if document != nil {
				t.Fatalf("Parse returned a document alongside error %v", err)
			}
			return
		}
		if document == nil {
			t.Fatal("Parse returned nil document and nil error")
		}
		// Accessors on a parsed document must never panic.
		_, _ = document.Card()
		_, _ = document.Signatures()
	})
}

// FuzzParsePublicJWK asserts the public-JWK parser never panics under either metadata policy.
func FuzzParsePublicJWK(f *testing.F) {
	key, keyID := fuzzKey(f)
	if jwk, err := EncodePublicJWK(&key.PublicKey, keyID); err == nil {
		f.Add(jwk, keyID)
	}
	for _, seed := range []string{
		"", "{}", "null", "[]",
		"{\"kty\":\"EC\",\"crv\":\"P-256\",\"x\":\"\",\"y\":\"\"}",
		"{\"kty\":\"RSA\"}",
		"{\"kty\":\"EC\",\"crv\":\"P-256\",\"x\":\"AA\",\"y\":\"AA\",\"d\":\"AA\"}",
	} {
		f.Add([]byte(seed), "es256:fuzz")
	}

	f.Fuzz(func(_ *testing.T, raw []byte, keyID string) {
		// Both policies must fail closed with an error rather than panic on hostile input.
		_, _ = ParsePublicJWK(raw, keyID, RequirePublicJWKMetadata)
		_, _ = ParsePublicJWK(raw, keyID, AllowOptionalJWKMetadata)
	})
}

// FuzzVerifyTamper is the trust-boundary property: any card whose canonical unsigned payload differs
// from a validly signed card must never verify under the original pinned key. It also asserts Verify
// never panics. A false accept here would be a federation trust-boundary break.
func FuzzVerifyTamper(f *testing.F) {
	key, keyID := fuzzKey(f)
	unsigned := fuzzUnsignedCard(f)
	bundle, err := Sign(unsigned, key, keyID)
	if err != nil {
		f.Fatalf("sign seed card: %v", err)
	}
	good, err := Parse(bundle.AgentCard)
	if err != nil {
		f.Fatalf("parse seed card: %v", err)
	}
	if err := Verify(good, &key.PublicKey, keyID); err != nil {
		f.Fatalf("seed card must verify: %v", err)
	}
	goodPayload := append([]byte(nil), good.payload...)

	// Seed with top-level field mutations that must all be rejected.
	f.Add([]byte("name"), []byte("Impersonated card"))
	f.Add([]byte("description"), []byte("tampered"))
	f.Add([]byte("version"), []byte("9.9.9"))
	f.Add([]byte("extra"), []byte("injected"))

	f.Fuzz(func(t *testing.T, field, value []byte) {
		fieldName := string(field)
		// Leave signatures untouched: this target proves payload tampering fails, not that a
		// forged signature is rejected (FuzzParse/verifySignature cover malformed signatures).
		if fieldName == "" || fieldName == "signatures" {
			return
		}
		object := map[string]any{}
		if err := json.Unmarshal(bundle.AgentCard, &object); err != nil {
			t.Fatalf("seed card is not an object: %v", err)
		}
		object[fieldName] = string(value)
		mutated, err := json.Marshal(object)
		if err != nil {
			return
		}
		document, err := Parse(mutated)
		if err != nil {
			return // rejected before verification is fine
		}
		verifyErr := Verify(document, &key.PublicKey, keyID)
		if bytes.Equal(document.payload, goodPayload) {
			return // mutation reproduced the exact canonical payload; verifying is correct
		}
		if verifyErr == nil {
			t.Fatalf("tampered card verified: field=%q value=%q", field, value)
		}
	})
}
