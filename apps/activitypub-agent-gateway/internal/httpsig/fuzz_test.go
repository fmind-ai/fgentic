package httpsig

import "testing"

// FuzzParseCavage fuzzes the Cavage HTTP-signature header parser, which decodes a signature header
// from an unauthenticated remote peer at the ActivityPub inbound border. It asserts the parser never
// panics and that any accepted signature has a non-empty keyID and signature (a partially-parsed
// signature must fail closed, never reach verification with missing fields).
func FuzzParseCavage(f *testing.F) {
	for _, seed := range []string{
		"",
		`keyId="https://peer.example/actor#main-key",algorithm="rsa-sha256",headers="(request-target) host date",signature="AAAA"`,
		`keyId="k",headers="date",signature="` + "aGVsbG8=" + `"`,
		`signature="AA"`,
		`keyId="k"`,
		`keyId="k",created="1700000000",signature="AA"`,
		`keyId=,,,="`,
		`headers="",signature=""`,
		"keyId=\x00,signature=\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, header string) {
		// splitSignatureParams and parseCavage both consume the raw header; neither may panic. A
		// split or parse failure is a fine, fail-closed outcome.
		_, _ = splitSignatureParams(header)
		sig, err := parseCavage(header)
		if err != nil {
			if sig != nil {
				t.Fatalf("parseCavage returned a signature alongside error %v", err)
			}
			return
		}
		if sig.keyID == "" {
			t.Fatalf("accepted a Cavage signature with an empty keyID: %q", header)
		}
		if len(sig.signature) == 0 {
			t.Fatalf("accepted a Cavage signature with empty signature bytes: %q", header)
		}
	})
}
