package delivery

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/testhttp"
)

// captureInbox records delivered bodies and verifies each signature against pub.
type captureInbox struct {
	mu       sync.Mutex
	bodies   [][]byte
	profiles []string
	verifier *httpsig.Verifier
	status   int
}

type fixedResolver struct {
	key   crypto.PublicKey
	owner string
}

func (r fixedResolver) Resolve(context.Context, string) (httpsig.PublicKey, error) {
	return httpsig.PublicKey{Key: r.key, Owner: r.owner}, nil
}

func (c *captureInbox) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		result, err := c.verifier.Verify(r.Context(), r, body)
		if err != nil {
			t.Errorf("delivered request failed signature verification: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.profiles = append(c.profiles, result.Scheme)
		c.mu.Unlock()
		w.WriteHeader(c.status)
	}
}

func TestDeliverSignsAndPosts(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const sender = "https://fgentic.localhost/ap/groups/collab"
	inbox := &captureInbox{verifier: httpsig.NewVerifier(fixedResolver{key: &priv.PublicKey, owner: sender}, time.Hour), status: http.StatusAccepted}
	srv := httptest.NewTLSServer(inbox.handler(t))
	defer srv.Close()
	const host = "inbox.example.com"
	baseURL := testhttp.URL(host)

	d := New(testhttp.Client(t, map[string]*httptest.Server{host: srv}), priv, slog.Default())
	body := []byte(`{"type":"Announce","actor":"` + sender + `"}`)
	if err := d.Deliver(context.Background(), baseURL+"/inbox", sender, body); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(inbox.bodies) != 1 || string(inbox.bodies[0]) != string(body) {
		t.Errorf("inbox received %v", inbox.bodies)
	}
	if len(inbox.profiles) != 1 || inbox.profiles[0] != string(httpsig.ProfileRFC9421) {
		t.Errorf("profiles = %v, want first delivery to prefer RFC 9421", inbox.profiles)
	}
}

func TestDeliverFallsBackAndRemembersProfilePerServer(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const sender = "https://fgentic.localhost/ap/groups/collab"
	verifier := httpsig.NewVerifier(fixedResolver{key: &priv.PublicKey, owner: sender}, time.Hour)
	var mu sync.Mutex
	var profiles []string

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read body: %v", readErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		result, verifyErr := verifier.Verify(r.Context(), r, body)
		if verifyErr != nil {
			t.Errorf("verify %v: %v", r.Header, verifyErr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		profiles = append(profiles, result.Scheme)
		mu.Unlock()
		if result.Scheme == string(httpsig.ProfileRFC9421) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	const host = "fallback.example.com"
	baseURL := testhttp.URL(host)

	d := New(testhttp.Client(t, map[string]*httptest.Server{host: srv}), priv, slog.Default())
	body := []byte(`{"type":"Announce"}`)
	for range 2 {
		if deliverErr := d.Deliver(context.Background(), baseURL+"/inbox", sender, body); deliverErr != nil {
			t.Fatalf("Deliver: %v", deliverErr)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{string(httpsig.ProfileRFC9421), string(httpsig.ProfileCavage), string(httpsig.ProfileCavage)}
	if len(profiles) != len(want) {
		t.Fatalf("profiles = %v, want %v", profiles, want)
	}
	for i := range want {
		if profiles[i] != want[i] {
			t.Errorf("profiles[%d] = %q, want %q", i, profiles[i], want[i])
		}
	}
}

func TestDeliverErrorsOnNon2xx(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	const host = "failure.example.com"
	baseURL := testhttp.URL(host)
	d := New(testhttp.Client(t, map[string]*httptest.Server{host: srv}), priv, slog.Default())
	if err := d.Deliver(context.Background(), baseURL, "https://x/actor", []byte("{}")); err == nil {
		t.Errorf("expected error on 500 response")
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want no fallback retry after a 500", requests.Load())
	}
}

func TestDeliverReturnsBothProfileFailures(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	const host = "unauthorized.example.com"
	baseURL := testhttp.URL(host)

	d := New(testhttp.Client(t, map[string]*httptest.Server{host: srv}), priv, slog.Default())
	if err := d.Deliver(context.Background(), baseURL, "https://x/actor", []byte("{}")); err == nil {
		t.Fatal("expected both signature profiles to fail")
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want one RFC 9421 attempt and one Cavage fallback", requests.Load())
	}
}

func TestProfileMemoryIsBounded(t *testing.T) {
	memory := profileMemory{byServer: make(map[string]httpsig.Profile)}
	for i := range maxRememberedServers + 1 {
		memory.set("server-"+strconv.Itoa(i), httpsig.ProfileCavage)
	}
	if len(memory.byServer) != maxRememberedServers {
		t.Fatalf("remembered servers = %d, want %d", len(memory.byServer), maxRememberedServers)
	}
	if got := memory.get("server-0"); got != httpsig.ProfileRFC9421 {
		t.Errorf("evicted server profile = %q, want RFC 9421 default", got)
	}
	if got := alternate(httpsig.ProfileCavage); got != httpsig.ProfileRFC9421 {
		t.Errorf("alternate(Cavage) = %q", got)
	}
}

func TestDeliverRejectsPlainHTTP(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := New(http.DefaultClient, priv, slog.Default())
	if err := d.Deliver(context.Background(), "http://remote.example/inbox", "https://sender.example/actor", nil); err == nil {
		t.Fatal("plain HTTP delivery must be rejected before any request")
	}
}

func TestDeliverRejectsPrivateInboxWithoutDial(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var dials atomic.Int64
	client := &http.Client{Transport: &http.Transport{DialContext: func(
		context.Context,
		string,
		string,
	) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}}}
	d := New(client, priv, slog.Default())
	if err := d.Deliver(
		context.Background(),
		"https://169.254.169.254/latest/meta-data",
		"https://sender.example/actor",
		nil,
	); err == nil {
		t.Fatal("metadata inbox must be rejected")
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("outbound dials = %d, want 0", got)
	}
}

func TestFanoutIsBestEffort(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const sender = "https://fgentic.localhost/ap/groups/collab"
	inbox := &captureInbox{verifier: httpsig.NewVerifier(fixedResolver{key: pub, owner: sender}, time.Hour), status: http.StatusOK}
	good := httptest.NewTLSServer(inbox.handler(t))
	defer good.Close()
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	const goodHost = "good.example.com"
	const badHost = "bad.example.com"

	client := testhttp.Client(t, map[string]*httptest.Server{goodHost: good, badHost: bad})
	d := New(client, priv, slog.Default())
	body := []byte(`{"type":"Announce"}`)
	// One good inbox, one failing: fanout delivers to the good one and reports 1.
	delivered := d.Fanout(context.Background(), []string{
		testhttp.URL(goodHost) + "/inbox",
		testhttp.URL(badHost) + "/inbox",
	}, sender, body)
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1 (best-effort)", delivered)
	}
}
