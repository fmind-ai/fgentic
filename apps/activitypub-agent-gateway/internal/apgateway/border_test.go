package apgateway

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/policy"
)

type staticResolver struct {
	key   crypto.PublicKey
	owner string
}

func (s staticResolver) Resolve(context.Context, string) (httpsig.PublicKey, error) {
	return httpsig.PublicKey{Key: s.key, Owner: s.owner}, nil
}

func writePolicyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

// borderTestActor is the fixed signing actor the border-test helpers issue signatures for; it
// matches the actor embedded in createNote, so the same signed request drives the gateway tests.
const borderTestActor = "https://mastodon.example/users/bob"

// signedInbox builds an Ed25519 Cavage-signed inbox request for borderTestActor.
func signedInbox(t *testing.T, priv ed25519.PrivateKey, body []byte) *http.Request {
	actor := borderTestActor
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://fgentic.localhost/ap/agents/agent-docs-qa/inbox", strings.NewReader(string(body)))
	req.Host = "fgentic.localhost"
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	sum := sha256.Sum256(body)
	req.Header.Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(sum[:]))
	headers := []string{"(request-target)", "host", "date", "digest"}
	lines := []string{
		"(request-target): post /ap/agents/agent-docs-qa/inbox",
		"host: " + req.Host,
		"date: " + req.Header.Get("Date"),
		"digest: " + req.Header.Get("Digest"),
	}
	sig := ed25519.Sign(priv, []byte(strings.Join(lines, "\n")))
	req.Header.Set("Signature", strings.Join([]string{
		`keyId="` + actor + `#main-key"`,
		`algorithm="ed25519"`,
		`headers="` + strings.Join(headers, " ") + `"`,
		`signature="` + base64.StdEncoding.EncodeToString(sig) + `"`,
	}, ","))
	return req
}

func newBorder(t *testing.T, pub ed25519.PublicKey, policyBody string) *Border {
	t.Helper()
	store := policy.NewStore(writePolicyFile(t, policyBody), slog.Default())
	verifier := httpsig.NewVerifier(staticResolver{key: pub, owner: borderTestActor}, time.Hour)
	return NewBorder(verifier, store, slog.Default())
}

func TestBorderAuthorize(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const actor = "https://mastodon.example/users/bob"
	body := []byte(`{"type":"Create"}`)

	t.Run("allowlisted signed actor is admitted", func(t *testing.T) {
		border := newBorder(t, pub, `{"version":1,"allowed_domains":["mastodon.example"]}`)
		req := signedInbox(t, priv, body)
		if d := border.Authorize(context.Background(), req, body, actor); !d.Allowed {
			t.Fatalf("decision = %+v, want allowed", d)
		}
	})

	t.Run("off-allowlist signed actor is denied", func(t *testing.T) {
		border := newBorder(t, pub, `{"version":1,"allowed_domains":["other.example"]}`)
		req := signedInbox(t, priv, body)
		d := border.Authorize(context.Background(), req, body, actor)
		if d.Allowed || d.Reason != "off_allowlist" {
			t.Fatalf("decision = %+v, want off_allowlist", d)
		}
	})

	t.Run("unsigned is denied", func(t *testing.T) {
		border := newBorder(t, pub, `{"version":1,"allowed_domains":["mastodon.example"]}`)
		req := httptest.NewRequest(http.MethodPost, "https://fgentic.localhost/ap/agents/agent-docs-qa/inbox", strings.NewReader(string(body)))
		req.Host = "fgentic.localhost"
		d := border.Authorize(context.Background(), req, body, actor)
		if d.Allowed || d.Reason != "unsigned" {
			t.Fatalf("decision = %+v, want unsigned", d)
		}
	})

	t.Run("actor-key mismatch is denied", func(t *testing.T) {
		// Signature verifies against bob's key, but the activity claims a different actor.
		border := newBorder(t, pub, `{"version":1,"allowed_domains":["mastodon.example"]}`)
		req := signedInbox(t, priv, body)
		d := border.Authorize(context.Background(), req, body, "https://mastodon.example/users/impostor")
		if d.Allowed || d.Reason != "actor_key_mismatch" {
			t.Fatalf("decision = %+v, want actor_key_mismatch", d)
		}
	})

	t.Run("policy reload flips allow to deny", func(t *testing.T) {
		path := writePolicyFile(t, `{"version":1,"allowed_domains":["mastodon.example"]}`)
		store := policy.NewStore(path, slog.Default())
		border := NewBorder(httpsig.NewVerifier(staticResolver{key: pub, owner: actor}, time.Hour), store, slog.Default())

		if d := border.Authorize(context.Background(), signedInbox(t, priv, body), body, actor); !d.Allowed {
			t.Fatalf("pre-reload should allow: %+v", d)
		}
		if err := os.WriteFile(path, []byte(`{"version":1,"allowed_domains":["other.example"]}`), 0o600); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		store.Reload()
		if d := border.Authorize(context.Background(), signedInbox(t, priv, body), body, actor); d.Allowed {
			t.Fatalf("post-reload should deny: %+v", d)
		}
	})
}
