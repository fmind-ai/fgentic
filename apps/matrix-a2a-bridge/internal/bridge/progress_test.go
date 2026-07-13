package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
)

// runLongTask drives awaitTask synchronously for a scripted non-terminal task whose polls advance
// with no wait, so the working-state progress path (#118) runs deterministically.
func runLongTask(t *testing.T, b *Bridge, intent *appservice.IntentAPI, evt *event.Event, ref *AgentRef, res a2aclient.Result) delegationAuditResult {
	t.Helper()
	b.pollWait = func(context.Context, time.Duration) error { return nil }
	a2aCtx := a2aclient.WithUser(t.Context(), evt.Sender.String())
	return b.awaitTask(t.Context(), a2aCtx, intent, evt, ref, "agent-k8s", res)
}

func TestThreadedProgressPostsBoundedDedupedUpdates(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"},
		polls: []scriptedPoll{
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 1"}},
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 1"}}, // duplicate — deduped
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 2"}},
			{result: a2aclient.Result{TaskID: "task-1", Text: "done", Terminal: true}},
		},
	}
	b, intent, evt, ref, recorder := pollingHarness(t, client)
	b.cfg.MaxTaskProgressPosts = 3

	if audit := runLongTask(t, b, intent, evt, ref, client.callResult); audit.outcome != outcomeOK {
		t.Fatalf("outcome = %q, want ok", audit.outcome)
	}

	events := recorder.snapshot()
	if len(events) != 4 {
		t.Fatalf("Matrix events = %d, want placeholder + 2 progress + final edit", len(events))
	}
	placeholder := id.EventID("$reply-1")
	for i, want := range []string{"step 1", "step 2"} {
		p := events[1+i]
		if p.Body != want {
			t.Errorf("progress[%d] body = %q, want %q", i, p.Body, want)
		}
		if p.RelatesTo == nil || p.RelatesTo.Type != event.RelThread || p.RelatesTo.EventID != placeholder {
			t.Errorf("progress[%d] relation = %+v, want m.thread -> %s", i, p.RelatesTo, placeholder)
		}
	}
	// The final answer stays the m.replace edit of the root placeholder so thread previews are correct.
	final := events[3]
	if final.RelatesTo == nil || final.RelatesTo.GetReplaceID() != placeholder {
		t.Fatalf("final relation = %+v, want m.replace of %s", final.RelatesTo, placeholder)
	}
	if final.NewContent == nil || final.NewContent.Body != "done" {
		t.Fatalf("final new content = %+v, want body 'done'", final.NewContent)
	}

	// Exact wire relation for a threaded progress event (the mixin-stripping typed decode above can't
	// prove byte shape): m.relates_to must be exactly {rel_type: m.thread, event_id: <placeholder>}.
	raw := recorder.rawSnapshot(t)
	rel, ok := raw[1]["m.relates_to"].(map[string]any)
	if !ok || rel["rel_type"] != string(event.RelThread) || rel["event_id"] != string(placeholder) {
		t.Fatalf("progress relation JSON = %v, want {rel_type: m.thread, event_id: %s}", raw[1]["m.relates_to"], placeholder)
	}
}

func TestThreadedProgressRespectsBound(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"},
		polls: []scriptedPoll{
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 1"}},
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 2"}},
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 3"}},
			{result: a2aclient.Result{TaskID: "task-1", Text: "done", Terminal: true}},
		},
	}
	b, intent, evt, ref, recorder := pollingHarness(t, client)
	b.cfg.MaxTaskProgressPosts = 1 // hard cap regardless of how many distinct updates arrive

	runLongTask(t, b, intent, evt, ref, client.callResult)

	threaded := 0
	for _, e := range recorder.snapshot() {
		if e.RelatesTo != nil && e.RelatesTo.Type == event.RelThread {
			threaded++
		}
	}
	if threaded != 1 {
		t.Fatalf("threaded progress posts = %d, want exactly the bound of 1", threaded)
	}
}

func TestThreadedProgressDisabled(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"},
		polls: []scriptedPoll{
			{result: a2aclient.Result{TaskID: "task-1", Text: "step 1"}},
			{result: a2aclient.Result{TaskID: "task-1", Text: "done", Terminal: true}},
		},
	}
	b, intent, evt, ref, recorder := pollingHarness(t, client)
	b.cfg.MaxTaskProgressPosts = 0 // disabled

	runLongTask(t, b, intent, evt, ref, client.callResult)

	if events := recorder.snapshot(); len(events) != 2 {
		t.Fatalf("Matrix events = %d, want only placeholder + final edit when progress is disabled", len(events))
	}
}

// pinRecorder is a Matrix homeserver stub that tracks the room's pinned events, so tests can prove
// the placeholder is pinned while a task runs and unpinned when it ends.
type pinRecorder struct {
	mu     sync.Mutex
	pinned []string
	set    bool // whether m.room.pinned_events state exists yet (else the read 404s)
	writes int
}

func (r *pinRecorder) snapshot() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pinned...), r.writes
}

func pinningHarness(t *testing.T, client a2aClient) (*Bridge, *appservice.IntentAPI, *event.Event, *AgentRef, *pinRecorder) {
	t.Helper()
	pins := &pinRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/state/m.room.pinned_events"):
			pins.mu.Lock()
			set, current := pins.set, append([]string(nil), pins.pinned...)
			pins.mu.Unlock()
			if !set {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"errcode": "M_NOT_FOUND", "error": "no pinned events"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pinned": current})
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/state/m.room.pinned_events"):
			var body struct {
				Pinned []string `json:"pinned"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			pins.mu.Lock()
			pins.pinned, pins.set, pins.writes = body.Pinned, true, pins.writes+1
			pins.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]id.EventID{"event_id": "$pin-state"})
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/send/m.room.message/"):
			_ = json.NewEncoder(w).Encode(map[string]id.EventID{"event_id": "$reply"})
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/typing/"):
			_, _ = w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected Matrix request: %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)

	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     &appservice.Registration{AppToken: "test-token", SenderLocalpart: "a2a-bridge"},
		HomeserverDomain: ownServer,
		HomeserverURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}
	as.HTTPClient = server.Client()
	as.DefaultHTTPRetries = 0

	b := testBridge(t)
	b.as = as
	b.client = client
	b.cfg.RequestTimeout = time.Second
	b.cfg.TaskTimeout = time.Minute
	b.cfg.PinInFlightTasks = true

	evt, _ := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s inspect the pod")
	evt.ID = "$original"
	ref, ok := b.agents.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s fixture missing")
	}
	intent := as.Intent(id.NewUserID("agent-k8s", ownServer))
	intent.Registered = true
	if err := as.StateStore.SetMembership(t.Context(), evt.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	return b, intent, evt, ref, pins
}

func TestInFlightTaskIsPinnedThenUnpinned(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"},
		polls:      []scriptedPoll{{result: a2aclient.Result{TaskID: "task-1", Text: "done", Terminal: true}}},
	}
	b, intent, evt, ref, pins := pinningHarness(t, client)

	runLongTask(t, b, intent, evt, ref, client.callResult)

	// The placeholder was pinned (first read 404 -> empty) and unpinned on the terminal state: two
	// state writes, ending empty.
	final, writes := pins.snapshot()
	if writes != 2 {
		t.Fatalf("pinned-events writes = %d, want 2 (pin + unpin)", writes)
	}
	if len(final) != 0 {
		t.Fatalf("pinned events after completion = %v, want empty", final)
	}
}

// forbiddenPinIntent returns an intent whose state writes are always rejected, standing in for a
// ghost that lacks the room's state-event power level.
func forbiddenPinHarness(t *testing.T) (*Bridge, *appservice.IntentAPI, id.RoomID) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/state/m.room.pinned_events"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"errcode": "M_NOT_FOUND", "error": "none"})
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/state/m.room.pinned_events"):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"errcode": "M_FORBIDDEN", "error": "insufficient power level"})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     &appservice.Registration{AppToken: "test-token", SenderLocalpart: "a2a-bridge"},
		HomeserverDomain: ownServer,
		HomeserverURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}
	as.HTTPClient = server.Client()
	as.DefaultHTTPRetries = 0
	b := testBridge(t)
	b.as = as
	intent := as.Intent(id.NewUserID("agent-k8s", ownServer))
	intent.Registered = true
	room := id.RoomID("!room:fgentic.fmind.ai")
	if err := as.StateStore.SetMembership(t.Context(), room, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	return b, intent, room
}

func TestPinDegradesSilentlyWithoutPower(t *testing.T) {
	b, intent, room := forbiddenPinHarness(t)
	// A forbidden state write must not panic or propagate — pinning is a best-effort convenience.
	b.pinPlaceholder(t.Context(), intent, room, "$ph")
	b.unpinPlaceholder(t.Context(), intent, room, "$ph")
}
