package delivery

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fmind/activitypub-agent-gateway/internal/httpsig"
)

// captureInbox records delivered bodies and verifies each signature against pub.
type captureInbox struct {
	mu       sync.Mutex
	bodies   [][]byte
	verifier *httpsig.Verifier
	status   int
}

type fixedResolver struct {
	key   ed25519.PublicKey
	owner string
}

func (r fixedResolver) Resolve(context.Context, string) (httpsig.PublicKey, error) {
	return httpsig.PublicKey{Key: r.key, Owner: r.owner}, nil
}

func (c *captureInbox) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, err := c.verifier.Verify(r.Context(), r, body); err != nil {
			t.Errorf("delivered request failed signature verification: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.WriteHeader(c.status)
	}
}

func TestDeliverSignsAndPosts(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const sender = "https://fgentic.localhost/ap/groups/collab"
	inbox := &captureInbox{verifier: httpsig.NewVerifier(fixedResolver{key: pub, owner: sender}, time.Hour), status: http.StatusAccepted}
	srv := httptest.NewServer(inbox.handler(t))
	defer srv.Close()

	d := New(srv.Client(), priv, slog.Default())
	body := []byte(`{"type":"Announce","actor":"` + sender + `"}`)
	if err := d.Deliver(context.Background(), srv.URL+"/inbox", sender, body); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(inbox.bodies) != 1 || string(inbox.bodies[0]) != string(body) {
		t.Errorf("inbox received %v", inbox.bodies)
	}
}

func TestDeliverErrorsOnNon2xx(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := New(srv.Client(), priv, slog.Default())
	if err := d.Deliver(context.Background(), srv.URL, "https://x/actor", []byte("{}")); err == nil {
		t.Errorf("expected error on 500 response")
	}
}

func TestFanoutIsBestEffort(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const sender = "https://fgentic.localhost/ap/groups/collab"
	inbox := &captureInbox{verifier: httpsig.NewVerifier(fixedResolver{key: pub, owner: sender}, time.Hour), status: http.StatusOK}
	good := httptest.NewServer(inbox.handler(t))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()

	d := New(good.Client(), priv, slog.Default())
	body := []byte(`{"type":"Announce"}`)
	// One good inbox, one failing: fanout delivers to the good one and reports 1.
	delivered := d.Fanout(context.Background(), []string{good.URL + "/inbox", bad.URL + "/inbox"}, sender, body)
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1 (best-effort)", delivered)
	}
}
