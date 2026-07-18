package apgateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/activitystate"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/testhttp"
)

type blockingDelegator struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingDelegator) Release() { d.once.Do(func() { close(d.release) }) }

type blockingIgnoreStore struct {
	activitystate.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingIgnoreStore) Ignore(ctx context.Context, job activitystate.Job) (activitystate.Record, bool, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return activitystate.Record{}, false, ctx.Err()
	case <-s.release:
		return s.Store.Ignore(ctx, job)
	}
}

func (d *blockingDelegator) Call(context.Context, string, string, string, string) (string, error) {
	d.calls.Add(1)
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-d.release
	return "durable reply", nil
}

func startActivityProcessor(t *testing.T, g *Gateway) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, err := g.StartActivityProcessor(ctx)
	if err != nil {
		cancel()
		t.Fatalf("StartActivityProcessor: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("activity processor: %v", err)
		}
	})
	return cancel, done
}

func TestAsyncInboxDeduplicatesBudgetAndDelegationAcrossRestart(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	policyBody := `{"version":1,"allowed_domains":["mastodon.example"],"budgets":{"reservation_tokens":1000,"domains":{"mastodon.example":6000}}}`
	shared := activitystate.NewMemory(time.Hour, 32)
	block := &blockingDelegator{started: make(chan struct{}, 1), release: make(chan struct{})}
	defer block.Release()
	g1, _, reg1 := gatewayWithBudgetBorder(t, policyBody, priv)
	g1.delegator = block
	if err := g1.UseActivityStore(shared); err != nil {
		t.Fatalf("UseActivityStore: %v", err)
	}
	cancel1, done1 := startActivityProcessor(t, g1)

	serve := func(g *Gateway) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, signedInbox(t, priv, []byte(createNote)))
		return rec
	}
	first := serve(g1)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery code = %d, want 202", first.Code)
	}
	statusPath := strings.TrimPrefix(first.Header().Get("Location"), "https://fgentic.localhost")
	if !strings.HasPrefix(statusPath, "/ap/inbox-status/") {
		t.Fatalf("first status Location = %q", first.Header().Get("Location"))
	}
	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatal("background delegation did not start")
	}
	if pending := do(t, g1, http.MethodGet, statusPath, ""); pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"state":"running"`) {
		t.Fatalf("pending status = code %d body %q", pending.Code, pending.Body.String())
	}
	if rec := serve(g1); rec.Code != http.StatusAccepted || rec.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("in-flight duplicate code = %d, want cached 202", rec.Code)
	}
	if got := block.calls.Load(); got != 1 {
		t.Fatalf("delegations before release = %d, want 1", got)
	}
	if got := reservationCount(t, reg1, "reserved"); got != 1 {
		t.Fatalf("reservations = %v, want 1", got)
	}

	block.Release()
	job := activitystate.Job{
		ActivityID: "https://mastodon.example/activities/1",
		Route:      activitystate.RouteAgent,
		Target:     "agent-docs-qa",
		ActorURI:   borderTestActor,
		Body:       []byte(createNote),
	}
	var completed activitystate.Record
	deadline := time.Now().Add(2 * time.Second)
	for completed.State != activitystate.StateSucceeded && time.Now().Before(deadline) {
		completed, _, err = shared.Enqueue(context.Background(), job)
		if err != nil {
			t.Fatalf("inspect completed activity: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.State != activitystate.StateSucceeded || completed.Location == "" {
		t.Fatalf("completed activity = %+v", completed)
	}
	result := do(t, g1, http.MethodGet, statusPath, "")
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "durable reply") {
		t.Fatalf("completed status = code %d body %q", result.Code, result.Body.String())
	}
	cancel1()
	if err := <-done1; err != nil {
		t.Fatalf("first activity processor: %v", err)
	}

	// A fresh Gateway and budget reserver reuse only the durable activity ledger. The retry returns
	// the prior outcome without touching either the new budget or A2A client.
	g2, del2, reg2 := gatewayWithBudgetBorder(t, policyBody, priv)
	if err := g2.UseActivityStore(shared); err != nil {
		t.Fatalf("UseActivityStore after restart: %v", err)
	}
	startActivityProcessor(t, g2)
	if rec := serve(g2); rec.Code != http.StatusAccepted || rec.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("post-restart duplicate = code %d location %q, want cached durable status", rec.Code, rec.Header().Get("Location"))
	}
	restartedResult := do(t, g2, http.MethodGet, statusPath, "")
	if restartedResult.Code != http.StatusOK || restartedResult.Body.String() != result.Body.String() {
		t.Fatalf("post-restart status = code %d body %q", restartedResult.Code, restartedResult.Body.String())
	}
	replyPath := strings.TrimPrefix(completed.Location, "https://fgentic.localhost")
	publicReply := do(t, g2, http.MethodGet, replyPath, "")
	if publicReply.Code != http.StatusOK || publicReply.Body.String() != result.Body.String() {
		t.Fatalf("post-restart reply IRI = code %d body %q", publicReply.Code, publicReply.Body.String())
	}
	if got := del2.callCount(); got != 0 {
		t.Fatalf("post-restart delegations = %d, want 0", got)
	}
	if got := reservationCount(t, reg2, "reserved"); got != 0 {
		t.Fatalf("post-restart reservations = %v, want 0", got)
	}
}

func TestAsyncGroupFanoutDeduplicatesActivityID(t *testing.T) {
	author := newRemotePeer(t, "async-author.example.com")
	observer := newRemotePeer(t, "async-observer.example.com")
	client := testhttp.Client(t, map[string]*httptest.Server{
		author.host: author.server, observer.host: observer.server,
	})
	block := &blockingDelegator{started: make(chan struct{}, 1), release: make(chan struct{})}
	defer block.Release()
	g := newGroupGateway(t, block, client, nil)
	if err := g.UseActivityStore(activitystate.NewMemory(time.Hour, 32)); err != nil {
		t.Fatalf("UseActivityStore: %v", err)
	}
	g.followers.add("collab", author.actor(), author.inbox())
	g.followers.add("collab", observer.actor(), observer.inbox())
	startActivityProcessor(t, g)
	body := `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + testhttp.URL(author.host) + `/activities/group-1","type":"Create","actor":"` + author.actor() + `","object":{"id":"` + testhttp.URL(author.host) + `/notes/1","type":"Note","content":"@agent-docs-qa help"}}`
	if rec := do(t, g, http.MethodPost, "/ap/groups/collab/inbox", body); rec.Code != http.StatusAccepted {
		t.Fatalf("first group delivery code = %d", rec.Code)
	}
	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatal("group delegation did not start")
	}
	if rec := do(t, g, http.MethodPost, "/ap/groups/collab/inbox", body); rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate group delivery code = %d", rec.Code)
	}
	if got := block.calls.Load(); got != 1 {
		t.Fatalf("group delegations = %d, want 1", got)
	}
	preReply := countTypes(observer.typesDelivered())["Announce"]
	block.Release()
	if preReply != 1 {
		t.Fatalf("pre-reply observer Announces = %d, want one original fanout", preReply)
	}
	deadline := time.Now().Add(time.Second)
	for countTypes(observer.typesDelivered())["Announce"] != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countTypes(observer.typesDelivered())["Announce"]; got != 2 {
		t.Fatalf("observer Announces = %d, want one post + one agent reply", got)
	}
}

func TestAsyncIgnoredActivityIsNeverClaimable(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	policyBody := `{"version":1,"allowed_domains":["mastodon.example"],"budgets":{"reservation_tokens":1000,"domains":{"mastodon.example":6000}}}`
	g, del, reg := gatewayWithBudgetBorder(t, policyBody, priv)
	store := &blockingIgnoreStore{
		Store:   activitystate.NewMemory(time.Hour, 1),
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	if err := g.UseActivityStore(store); err != nil {
		t.Fatalf("UseActivityStore: %v", err)
	}
	startActivityProcessor(t, g)
	body := []byte(strings.ReplaceAll(createNote, "agent-docs-qa", "someone-else"))
	request := signedInbox(t, priv, body)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, request)
		response <- rec
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("ignored activity did not reach atomic store operation")
	}
	// Widen the old enqueue-then-complete race across several processor polls. No pending row exists.
	time.Sleep(3 * inboxPollInterval)
	if got := del.callCount(); got != 0 {
		t.Fatalf("ignored activity delegations = %d, want 0", got)
	}
	if got := reservationCount(t, reg, "reserved"); got != 0 {
		t.Fatalf("ignored activity reservations = %v, want 0", got)
	}
	close(store.release)
	rec := <-response
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Header().Get("Location"), "/ap/inbox-status/") {
		t.Fatalf("ignored response = code %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	statusPath := strings.TrimPrefix(rec.Header().Get("Location"), "https://fgentic.localhost")
	if status := do(t, g, http.MethodGet, statusPath, ""); status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"state":"ignored"`) {
		t.Fatalf("ignored status = code %d body %q", status.Code, status.Body.String())
	}
	if missing := do(t, g, http.MethodGet, "/ap/inbox-status/not-a-capability", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown status code = %d, want 404", missing.Code)
	}
}

func TestAsyncQueueCapacityBackpressuresBeforeSecondBudget(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	policyBody := `{"version":1,"allowed_domains":["mastodon.example"],"budgets":{"reservation_tokens":1000,"domains":{"mastodon.example":6000}}}`
	g, _, reg := gatewayWithBudgetBorder(t, policyBody, priv)
	block := &blockingDelegator{started: make(chan struct{}, 1), release: make(chan struct{})}
	defer block.Release()
	g.delegator = block
	if err := g.UseActivityStore(activitystate.NewMemory(time.Hour, 1)); err != nil {
		t.Fatalf("UseActivityStore: %v", err)
	}
	startActivityProcessor(t, g)
	serve := func(body []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, signedInbox(t, priv, body))
		return rec
	}
	if rec := serve([]byte(createNote)); rec.Code != http.StatusAccepted {
		t.Fatalf("first activity = %d", rec.Code)
	}
	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatal("first delegation did not start")
	}
	second := []byte(strings.Replace(createNote, "activities/1", "activities/2", 1))
	rec := serve(second)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("full queue = code %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := block.calls.Load(); got != 1 {
		t.Fatalf("delegations while full = %d, want 1", got)
	}
	if got := reservationCount(t, reg, "reserved"); got != 1 {
		t.Fatalf("reservations while full = %v, want 1", got)
	}
	block.Release()
}
