package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
)

type deadManSchedule struct {
	roomID      id.RoomID
	placeholder id.EventID
	txnID       string
	delay       time.Duration
	taskID      string
}

type fakeDeadManClient struct {
	supported    bool
	supportedErr error
	scheduleErr  error
	restartErr   error
	restartErrs  []error
	cancelErr    error
	schedules    []deadManSchedule
	restarts     []id.DelayID
	cancels      []id.DelayID
}

func (f *fakeDeadManClient) Supported(context.Context) (bool, error) {
	return f.supported, f.supportedErr
}

func (f *fakeDeadManClient) Schedule(
	_ context.Context,
	_ *appservice.IntentAPI,
	roomID id.RoomID,
	placeholder id.EventID,
	txnID string,
	delay time.Duration,
	taskID string,
) (id.DelayID, error) {
	f.schedules = append(f.schedules, deadManSchedule{
		roomID: roomID, placeholder: placeholder, txnID: txnID, delay: delay, taskID: taskID,
	})
	if f.scheduleErr != nil {
		return "", f.scheduleErr
	}
	return id.DelayID("delay-" + txnID), nil
}

func (f *fakeDeadManClient) Restart(_ context.Context, _ *appservice.IntentAPI, delayID id.DelayID) error {
	f.restarts = append(f.restarts, delayID)
	if len(f.restartErrs) > 0 {
		err := f.restartErrs[0]
		f.restartErrs = f.restartErrs[1:]
		return err
	}
	return f.restartErr
}

func (f *fakeDeadManClient) Cancel(_ context.Context, _ *appservice.IntentAPI, delayID id.DelayID) error {
	f.cancels = append(f.cancels, delayID)
	return f.cancelErr
}

func TestDeadManProbeFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		client    *fakeDeadManClient
		wantArmed bool
	}{
		{name: "supported", client: &fakeDeadManClient{supported: true}, wantArmed: true},
		{name: "unsupported", client: &fakeDeadManClient{}},
		{name: "probe failure", client: &fakeDeadManClient{supportedErr: errors.New("unavailable")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := testBridge(t)
			b.cfg.DeadManSwitchDelay = 2 * time.Minute
			b.deadMan = test.client
			b.probeDeadMan(t.Context())
			if b.deadManEnabled != test.wantArmed {
				t.Fatalf("deadManEnabled = %t, want %t", b.deadManEnabled, test.wantArmed)
			}
		})
	}
}

func TestAwaitTaskKeepsDeadManArmedUntilTerminalReply(t *testing.T) {
	client := &scriptedA2AClient{polls: []scriptedPoll{
		{result: a2aclient.Result{TaskID: "task-dead-man"}},
		{result: a2aclient.Result{TaskID: "task-dead-man", Text: "finished", Terminal: true}},
	}}
	b, intent, evt, ref, _ := pollingHarness(t, client)
	fake := &fakeDeadManClient{supported: true}
	b.deadMan = fake
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	b.deadManNow = func() time.Time { return now }
	b.pollWait = func(context.Context, time.Duration) error {
		now = now.Add(time.Minute)
		return nil
	}

	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-k8s", a2aclient.Result{
		TaskID: "task-dead-man",
	}, "")

	if audit.outcome != outcomeOK {
		t.Fatalf("await task outcome = %q, want %q", audit.outcome, outcomeOK)
	}
	if len(fake.schedules) != 1 || fake.schedules[0].delay != 2*time.Minute ||
		fake.schedules[0].roomID != evt.RoomID || fake.schedules[0].placeholder == "" {
		t.Fatalf("dead-man schedules = %+v", fake.schedules)
	}
	if len(fake.restarts) != 2 {
		t.Fatalf("dead-man restarts = %v, want two coarse refreshes", fake.restarts)
	}
	if len(fake.cancels) != 1 || fake.cancels[0] != fake.restarts[0] {
		t.Fatalf("dead-man cancels = %v, restarts = %v", fake.cancels, fake.restarts)
	}
}

func TestMatrixDeadManClientUsesPinnedSynapseContract(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests = append(requests, req.Method+" "+req.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/_matrix/client/versions":
			_, _ = w.Write([]byte(`{"versions":["v1.1"],"unstable_features":{"org.matrix.msc4140":true}}`))
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/send/m.room.message/dead-man-txn"):
			if req.URL.Query().Get("org.matrix.msc4140.delay") != "120000" {
				t.Errorf("delayed send query = %q", req.URL.RawQuery)
			}
			var content map[string]any
			if err := json.NewDecoder(req.Body).Decode(&content); err != nil {
				t.Errorf("decode delayed notice: %v", err)
			}
			assertDelayedNoticeContent(t, content)
			_, _ = w.Write([]byte(`{"delay_id":"delay-1"}`))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/delay-1/restart"):
			_, _ = w.Write([]byte(`{}`))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/delay-1/cancel"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"already finalised"}`))
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
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
	intent := as.Intent(id.NewUserID("agent-k8s", ownServer))
	intent.Registered = true
	if err := as.StateStore.SetMembership(
		t.Context(), "!room:"+ownServer, intent.UserID, event.MembershipJoin,
	); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	client := &matrixDeadManClient{as: as}

	supported, err := client.Supported(t.Context())
	if err != nil || !supported {
		t.Fatalf("Supported = (%t, %v), want (true, nil)", supported, err)
	}
	delayID, err := client.Schedule(
		t.Context(), intent, "!room:"+ownServer, "$placeholder", "dead-man-txn", 2*time.Minute, "task-dead",
	)
	if err != nil || delayID != "delay-1" {
		t.Fatalf("Schedule = (%q, %v), want (delay-1, nil)", delayID, err)
	}
	if err := client.Restart(t.Context(), intent, delayID); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := client.Cancel(t.Context(), intent, delayID); err != nil {
		t.Fatalf("idempotent Cancel after finalisation: %v", err)
	}
	if len(requests) != 4 {
		t.Fatalf("Matrix requests = %v, want versions, schedule, restart, cancel", requests)
	}
}

func assertDelayedNoticeContent(t *testing.T, content map[string]any) {
	t.Helper()
	if content["msgtype"] != "m.notice" || content["body"] != deadManNoticeText || content[automatedMixinKey] != true {
		t.Fatalf("delayed notice content = %#v", content)
	}
	relates, ok := content["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatalf("delayed notice relation = %#v", content["m.relates_to"])
	}
	reply, ok := relates["m.in_reply_to"].(map[string]any)
	if !ok || reply["event_id"] != "$placeholder" {
		t.Fatalf("delayed notice reply relation = %#v", relates)
	}
	// The homeserver-fired stale-task notice is terminal (the bridge lost the task), so it carries the
	// versioned ai.fgentic.a2a block with outcome=lost, the full ghost MXID, and the delegation task id.
	block, ok := content[resultMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("delayed notice missing %s block: %#v", resultMetadataKey, content)
	}
	if block["outcome"] != outcomeLost || block["agent"] != "@agent-k8s:"+ownServer ||
		block["task_id"] != "task-dead" || int(block["v"].(float64)) != resultMetadataVersion {
		t.Fatalf("delayed notice block = %#v", block)
	}
}
