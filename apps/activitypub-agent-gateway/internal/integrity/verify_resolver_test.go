package integrity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/testhttp"
)

// staticResolver returns a fixed key regardless of the verificationMethod.
type staticResolver struct {
	key        ed25519.PublicKey
	controller string
	err        error
}

func (s staticResolver) Resolve(context.Context, string) (ResolvedKey, error) {
	if s.err != nil {
		return ResolvedKey{}, s.err
	}
	return ResolvedKey{Key: s.key, Controller: s.controller}, nil
}

func signedGolden(t *testing.T) (map[string]any, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	doc := goldenDoc(t)
	if err := Sign(doc, priv, goldenVerificationMethod, time.Now()); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return doc, pub
}

func TestVerifierResolvesAndVerifies(t *testing.T) {
	doc, pub := signedGolden(t)
	v := NewVerifier(staticResolver{key: pub, controller: "https://fgentic.localhost/ap/agents/agent-docs-qa"})
	controller, err := v.VerifyDocument(context.Background(), doc)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	if controller != "https://fgentic.localhost/ap/agents/agent-docs-qa" {
		t.Errorf("controller = %q", controller)
	}
}

func TestVerifierFailsClosed(t *testing.T) {
	doc, pub := signedGolden(t)

	t.Run("missing proof", func(t *testing.T) {
		bare := goldenDoc(t)
		v := NewVerifier(staticResolver{key: pub})
		if _, err := v.VerifyDocument(context.Background(), bare); !errors.Is(err, ErrNoProof) {
			t.Errorf("err = %v, want ErrNoProof", err)
		}
	})

	t.Run("unresolvable key", func(t *testing.T) {
		v := NewVerifier(staticResolver{err: errors.New("boom")})
		if _, err := v.VerifyDocument(context.Background(), doc); !errors.Is(err, ErrKeyResolution) {
			t.Errorf("err = %v, want ErrKeyResolution", err)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		other, _, _ := ed25519.GenerateKey(nil)
		v := NewVerifier(staticResolver{key: other})
		if _, err := v.VerifyDocument(context.Background(), doc); !errors.Is(err, ErrProofInvalid) {
			t.Errorf("err = %v, want ErrProofInvalid", err)
		}
	})
}

func TestHTTPKeyResolverExtractsMultikey(t *testing.T) {
	pub := goldenKey(t).Public().(ed25519.PublicKey)
	actorID := "https://fgentic.localhost/ap/agents/agent-docs-qa"
	vm := actorID + "#ed25519-key"

	const host = "integrity.example.com"
	baseURL := testhttp.URL(host)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ap/agents/agent-docs-qa" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{
			"id": %q,
			"assertionMethod": [
				{"id": %q, "type": "Multikey", "controller": %q, "publicKeyMultibase": %q}
			]
		}`, actorID, vm, actorID, goldenPublicKeyMultibase)
	}))
	defer srv.Close()

	// Rewrite the fetch host to the test server while keeping the fragment logic intact.
	resolver := NewHTTPKeyResolver(testhttp.Client(t, map[string]*httptest.Server{host: srv}))
	got, err := resolver.Resolve(context.Background(), baseURL+"/ap/agents/agent-docs-qa#ed25519-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Key.Equal(pub) {
		t.Errorf("resolved key mismatch")
	}
	if got.Controller != actorID {
		t.Errorf("controller = %q", got.Controller)
	}
}

func TestHTTPKeyResolverRejectsPrivateURLsWithoutDial(t *testing.T) {
	t.Parallel()
	var dials atomic.Int64
	client := &http.Client{Transport: &http.Transport{DialContext: func(
		context.Context,
		string,
		string,
	) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}}}
	resolver := NewHTTPKeyResolver(client)
	for _, verificationMethod := range []string{
		"http://keys.example.com/actor#ed25519-key",
		"https://127.0.0.1/actor#ed25519-key",
		"https://169.254.169.254/latest/meta-data#ed25519-key",
		"https://[fd00::1]/actor#ed25519-key",
	} {
		if _, err := resolver.Resolve(context.Background(), verificationMethod); err == nil {
			t.Errorf("Resolve(%q) must fail closed", verificationMethod)
		}
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("outbound dials = %d, want 0", got)
	}
}

func TestHTTPKeyResolverErrors(t *testing.T) {
	const host = "integrity-errors.example.com"
	baseURL := testhttp.URL(host)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/no-keys":
			_, _ = fmt.Fprint(w, `{"id":"https://x/a"}`)
		case "/single":
			_, _ = fmt.Fprintf(w, `{"id":"https://x/a","assertionMethod":{"type":"Multikey","controller":"https://x/a","publicKeyMultibase":%q}}`, goldenPublicKeyMultibase)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	resolver := NewHTTPKeyResolver(testhttp.Client(t, map[string]*httptest.Server{host: srv}))

	if _, err := resolver.Resolve(context.Background(), baseURL+"/no-keys#k"); err == nil {
		t.Errorf("expected error for doc with no assertionMethod")
	}
	if _, err := resolver.Resolve(context.Background(), baseURL+"/missing#k"); err == nil {
		t.Errorf("expected error for 404")
	}
	// A sole inline key with no id is accepted (some servers omit key ids).
	if got, err := resolver.Resolve(context.Background(), baseURL+"/single#k"); err != nil {
		t.Errorf("single-key resolve: %v", err)
	} else if got.Controller != "https://x/a" {
		t.Errorf("controller = %q", got.Controller)
	}
}

func TestHTTPKeyResolverRejectsBadKeyMaterial(t *testing.T) {
	if NewHTTPKeyResolver(nil) == nil {
		t.Fatalf("nil client must fall back to a default resolver")
	}
	const host = "integrity-bad.example.com"
	baseURL := testhttp.URL(host)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad-mb":
			_, _ = fmt.Fprint(w, `{"id":"https://x/a","assertionMethod":[{"id":"https://x/a#k","type":"Multikey","controller":"https://x/a","publicKeyMultibase":"Xnotmultibase"}]}`)
		case "/no-controller":
			_, _ = fmt.Fprintf(w, `{"assertionMethod":{"type":"Multikey","publicKeyMultibase":%q}}`, goldenPublicKeyMultibase)
		case "/not-json":
			_, _ = fmt.Fprint(w, `{not json`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	resolver := NewHTTPKeyResolver(testhttp.Client(t, map[string]*httptest.Server{host: srv}))

	for _, path := range []string{"/bad-mb", "/no-controller", "/not-json"} {
		if _, err := resolver.Resolve(context.Background(), baseURL+path+"#k"); err == nil {
			t.Errorf("%s: expected error", path)
		}
	}
	if _, err := resolver.Resolve(context.Background(), "#frag-only"); err == nil {
		t.Errorf("empty document URL must be rejected")
	}
}

func TestVerifyDocumentRejectsProofWithoutMethod(t *testing.T) {
	doc := goldenDoc(t)
	doc["proof"] = map[string]any{"type": ProofType, "cryptosuite": Cryptosuite, "proofValue": goldenProofValue}
	v := NewVerifier(staticResolver{})
	if _, err := v.VerifyDocument(context.Background(), doc); !errors.Is(err, ErrMalformedProof) {
		t.Errorf("err = %v, want ErrMalformedProof", err)
	}
	doc["proof"] = "not-an-object"
	if _, err := v.VerifyDocument(context.Background(), doc); !errors.Is(err, ErrMalformedProof) {
		t.Errorf("non-object proof err = %v, want ErrMalformedProof", err)
	}
}
