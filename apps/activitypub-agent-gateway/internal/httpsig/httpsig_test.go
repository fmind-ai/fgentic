package httpsig

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/testhttp"
)

const (
	testKeyID = "https://mastodon.example/users/bob#main-key"
	testOwner = "https://mastodon.example/users/bob"
	testURL   = "https://fgentic.localhost/ap/agents/agent-docs-qa/inbox"
)

type fakeResolver struct {
	key   crypto.PublicKey
	owner string
	err   error
}

func (f fakeResolver) Resolve(context.Context, string) (PublicKey, error) {
	if f.err != nil {
		return PublicKey{}, f.err
	}
	return PublicKey{Key: f.key, Owner: f.owner}, nil
}

func b64sha256(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// newSignedRequest builds a POST request with a body and Date + Digest headers set.
func newSignedRequest(t *testing.T, body []byte, date time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, testURL, strings.NewReader(string(body)))
	req.Host = "fgentic.localhost"
	req.Header.Set("Date", date.UTC().Format(http.TimeFormat))
	req.Header.Set("Digest", "SHA-256="+b64sha256(body))
	return req
}

// cavageString builds the Cavage signing string independently of the verifier (matching the draft).
func cavageString(req *http.Request, headers []string) string {
	var lines []string
	for _, h := range headers {
		switch h {
		case "(request-target)":
			lines = append(lines, "(request-target): "+strings.ToLower(req.Method)+" "+req.URL.RequestURI())
		case "host":
			lines = append(lines, "host: "+req.Host)
		default:
			lines = append(lines, h+": "+req.Header.Get(h))
		}
	}
	return strings.Join(lines, "\n")
}

func setCavageHeader(req *http.Request, keyID, algorithm string, headers []string, sig []byte) {
	req.Header.Set("Signature", strings.Join([]string{
		`keyId="` + keyID + `"`,
		`algorithm="` + algorithm + `"`,
		`headers="` + strings.Join(headers, " ") + `"`,
		`signature="` + base64.StdEncoding.EncodeToString(sig) + `"`,
	}, ","))
}

func TestVerifyCavageRSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	body := []byte(`{"type":"Create"}`)
	req := newSignedRequest(t, body, time.Now())
	headers := []string{"(request-target)", "host", "date", "digest"}
	digest := sha256.Sum256([]byte(cavageString(req, headers)))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	setCavageHeader(req, testKeyID, "rsa-sha256", headers, sig)

	v := NewVerifier(fakeResolver{key: &key.PublicKey, owner: testOwner}, time.Hour)
	res, err := v.Verify(context.Background(), req, body)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Owner != testOwner || res.Scheme != "cavage" {
		t.Errorf("result = %+v", res)
	}
}

func signCavageEd25519(req *http.Request, priv ed25519.PrivateKey, headers []string) {
	sig := ed25519.Sign(priv, []byte(cavageString(req, headers)))
	setCavageHeader(req, testKeyID, "ed25519", headers, sig)
}

func TestVerifyCavageEd25519(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	body := []byte(`{"type":"Create"}`)
	req := newSignedRequest(t, body, time.Now())
	headers := []string{"(request-target)", "host", "date", "digest"}
	signCavageEd25519(req, priv, headers)

	v := NewVerifier(fakeResolver{key: pub, owner: testOwner}, time.Hour)
	if _, err := v.Verify(context.Background(), req, body); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	body := []byte(`{"type":"Create"}`)
	headers := []string{"(request-target)", "host", "date", "digest"}

	t.Run("unsigned", func(t *testing.T) {
		req := newSignedRequest(t, body, time.Now())
		v := NewVerifier(fakeResolver{key: pub, owner: testOwner}, time.Hour)
		if _, err := v.Verify(context.Background(), req, body); !errors.Is(err, ErrNoSignature) {
			t.Errorf("err = %v, want ErrNoSignature", err)
		}
	})

	t.Run("tampered body", func(t *testing.T) {
		req := newSignedRequest(t, body, time.Now())
		signCavageEd25519(req, priv, headers)
		v := NewVerifier(fakeResolver{key: pub, owner: testOwner}, time.Hour)
		if _, err := v.Verify(context.Background(), req, []byte(`{"type":"Delete"}`)); !errors.Is(err, ErrDigestMismatch) {
			t.Errorf("err = %v, want ErrDigestMismatch", err)
		}
	})

	t.Run("bad signature", func(t *testing.T) {
		req := newSignedRequest(t, body, time.Now())
		setCavageHeader(req, testKeyID, "ed25519", headers, []byte("not-a-real-signature-not-a-real-signature-000000000000000000000"))
		v := NewVerifier(fakeResolver{key: pub, owner: testOwner}, time.Hour)
		if _, err := v.Verify(context.Background(), req, body); !errors.Is(err, ErrSignatureInvalid) {
			t.Errorf("err = %v, want ErrSignatureInvalid", err)
		}
	})

	t.Run("stale", func(t *testing.T) {
		req := newSignedRequest(t, body, time.Now().Add(-48*time.Hour))
		signCavageEd25519(req, priv, headers)
		v := NewVerifier(fakeResolver{key: pub, owner: testOwner}, time.Hour)
		if _, err := v.Verify(context.Background(), req, body); !errors.Is(err, ErrStale) {
			t.Errorf("err = %v, want ErrStale", err)
		}
	})

	t.Run("key resolution error", func(t *testing.T) {
		req := newSignedRequest(t, body, time.Now())
		signCavageEd25519(req, priv, headers)
		v := NewVerifier(fakeResolver{err: errors.New("boom")}, time.Hour)
		if _, err := v.Verify(context.Background(), req, body); !errors.Is(err, ErrKeyResolution) {
			t.Errorf("err = %v, want ErrKeyResolution", err)
		}
	})
}

func TestVerifyRFC9421Ed25519(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	body := []byte(`{"type":"Create"}`)
	created := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, testURL, strings.NewReader(string(body)))
	req.Host = "fgentic.localhost"
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Content-Digest", "sha-256=:"+b64sha256(body)+":")

	params := `("@method" "@path" "@authority" "content-digest" "date");created=` +
		strconv.FormatInt(created, 10) + `;keyid="` + testKeyID + `";alg="ed25519"`
	signingString := strings.Join([]string{
		`"@method": POST`,
		`"@path": /ap/agents/agent-docs-qa/inbox`,
		`"@authority": fgentic.localhost`,
		`"content-digest": ` + req.Header.Get("Content-Digest"),
		`"date": ` + req.Header.Get("Date"),
		`"@signature-params": ` + params,
	}, "\n")
	sig := ed25519.Sign(priv, []byte(signingString))
	req.Header.Set("Signature-Input", "sig1="+params)
	req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(sig)+":")

	v := NewVerifier(fakeResolver{key: pub, owner: testOwner}, time.Hour)
	res, err := v.Verify(context.Background(), req, body)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Scheme != "rfc9421" || res.Owner != testOwner {
		t.Errorf("result = %+v", res)
	}
}

// TestCavageSigningStringExact guards the reconstruction against a hand-written expected string.
func TestCavageSigningStringExact(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, testURL, nil)
	req.Host = "fgentic.localhost"
	req.Header.Set("Date", "Wed, 01 Jan 2025 00:00:00 GMT")
	req.Header.Set("Digest", "SHA-256=abc")
	sig := &parsedSignature{scheme: "cavage", components: []string{"(request-target)", "host", "date", "digest"}}
	got, err := sig.signingString(req)
	if err != nil {
		t.Fatalf("signingString: %v", err)
	}
	want := "(request-target): post /ap/agents/agent-docs-qa/inbox\n" +
		"host: fgentic.localhost\n" +
		"date: Wed, 01 Jan 2025 00:00:00 GMT\n" +
		"digest: SHA-256=abc"
	if got != want {
		t.Errorf("signing string mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestHTTPKeyResolver(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIX: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	const host = "keys.example.com"
	baseURL := testhttp.URL(host)
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	mux.HandleFunc("/users/bob", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + baseURL + `/users/bob","publicKey":{"id":"` + baseURL +
			`/users/bob#main-key","owner":"` + baseURL + `/users/bob","publicKeyPem":` + strconv.Quote(pemText) + `}}`))
	})

	client := testhttp.Client(t, map[string]*httptest.Server{host: server})
	resolver := NewHTTPKeyResolver(client)
	pub, err := resolver.Resolve(context.Background(), baseURL+"/users/bob#main-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pub.Owner != baseURL+"/users/bob" {
		t.Errorf("owner = %q", pub.Owner)
	}
	if _, ok := pub.Key.(*rsa.PublicKey); !ok {
		t.Errorf("key type = %T, want *rsa.PublicKey", pub.Key)
	}

	if _, err := resolver.Resolve(context.Background(), "#frag-only"); err == nil {
		t.Errorf("expected error for empty document URL")
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
	for _, keyID := range []string{
		"http://keys.example.com/actor#main-key",
		"https://127.0.0.1/actor#main-key",
		"https://169.254.169.254/latest/meta-data#main-key",
		"https://[fd00::1]/actor#main-key",
	} {
		if _, err := resolver.Resolve(context.Background(), keyID); err == nil {
			t.Errorf("Resolve(%q) must fail closed", keyID)
		}
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("outbound dials = %d, want 0", got)
	}
}

func TestLoadRSAPrivateKeyFromFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "rsa.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	loaded, err := LoadRSAPrivateKeyFromFile(path)
	if err != nil {
		t.Fatalf("LoadRSAPrivateKeyFromFile: %v", err)
	}
	if loaded.N.Cmp(key.N) != 0 {
		t.Error("loaded RSA key does not match source key")
	}
}

func TestLoadRSAPrivateKeyRejectsWeakKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "weak.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := LoadRSAPrivateKeyFromFile(path); err == nil {
		t.Fatal("weak RSA key must be rejected")
	}
}

func TestParsePublicKeyPEMRejectsGarbage(t *testing.T) {
	if _, err := ParsePublicKeyPEM("not a pem"); err == nil {
		t.Errorf("expected error for non-PEM input")
	}
}
