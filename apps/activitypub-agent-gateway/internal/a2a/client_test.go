package a2a

import (
	"context"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

const testAgentPath = "/api/a2a/kagent/docs-qa"

type executorFunc func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error]

func (fn executorFunc) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return fn(ctx, ec)
}

func (executorFunc) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

// testServer stands up an in-process A2A agent (card + JSON-RPC) and returns a Client dialing it.
func testServer(t *testing.T, executor a2asrv.AgentExecutor) (*Client, *[]string) {
	t.Helper()
	handler := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	endpoint := a2asrv.NewJSONRPCHandler(handler)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	users := &[]string{}
	card := &a2a.AgentCard{
		Name:                "docs-qa fixture",
		Version:             "test",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(server.URL+testAgentPath, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
	}
	mux.HandleFunc(testAgentPath+a2asrv.WellKnownAgentCardPath, func(w http.ResponseWriter, req *http.Request) {
		a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, req)
	})
	mux.HandleFunc(testAgentPath, func(w http.ResponseWriter, req *http.Request) {
		*users = append(*users, req.Header.Get(userHeader))
		endpoint.ServeHTTP(w, req)
	})

	client := New(server.URL, "workload-key", 30*time.Second, time.Minute, slog.Default())
	return client, users
}

func TestCallReturnsReplyAndForwardsUser(t *testing.T) {
	var gotContext string
	executor := executorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		gotContext = ec.ContextID
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("hello from docs-qa")), nil)
		}
	})
	client, users := testServer(t, executor)

	ctx := WithUser(context.Background(), "https://mastodon.example/users/bob")
	reply, err := client.Call(ctx, "kagent", "docs-qa", "hi", "ctx-123")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply != "hello from docs-qa" {
		t.Errorf("reply = %q", reply)
	}
	if gotContext != "ctx-123" {
		t.Errorf("contextID = %q, want threaded", gotContext)
	}
	if len(*users) == 0 || (*users)[len(*users)-1] != "https://mastodon.example/users/bob" {
		t.Errorf("X-User-Id not forwarded: %v", *users)
	}

	// Second call reuses the cached SDK client (no extra card resolution error paths).
	if _, err := client.Call(ctx, "kagent", "docs-qa", "again", "ctx-123"); err != nil {
		t.Fatalf("second Call: %v", err)
	}
}

// captureRT records the request it receives so header injection can be asserted directly.
type captureRT struct{ req *http.Request }

func (c *captureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.req = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestAttributionHeaders(t *testing.T) {
	cap := &captureRT{}
	tr := &userTransport{base: cap, apiKey: "workload-key"}

	const actor = "https://mastodon.example/users/bob"
	ctx := WithAttribution(context.Background(), actor, Origin{Kind: "activitypub", Network: "mastodon.example"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://gw/x", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	h := cap.req.Header
	if got := h.Get("X-User-Id"); got != actor {
		t.Errorf("X-User-Id = %q, want the full un-truncated actor URI", got)
	}
	if got := h.Get("X-Origin-Kind"); got != "activitypub" {
		t.Errorf("X-Origin-Kind = %q", got)
	}
	if got := h.Get("X-Origin-Network"); got != "mastodon.example" {
		t.Errorf("X-Origin-Network = %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer workload-key" {
		t.Errorf("Authorization = %q", got)
	}

	// WithUser sets the identity but no origin headers.
	cap2 := &captureRT{}
	tr2 := &userTransport{base: cap2}
	req2, _ := http.NewRequestWithContext(WithUser(context.Background(), actor), http.MethodPost, "http://gw/x", nil)
	if _, err := tr2.RoundTrip(req2); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if cap2.req.Header.Get("X-User-Id") != actor {
		t.Errorf("WithUser must still set X-User-Id")
	}
	if cap2.req.Header.Get("X-Origin-Kind") != "" {
		t.Errorf("WithUser must not set an origin kind")
	}
}

func TestCallRejectsEmptyTarget(t *testing.T) {
	client, _ := testServer(t, executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	}))
	if _, err := client.Call(context.Background(), "", "docs-qa", "hi", ""); err == nil {
		t.Errorf("expected error for empty namespace")
	}
}

func TestCallResolveFailure(t *testing.T) {
	client := New("http://127.0.0.1:0", "", time.Second, 2*time.Second, slog.Default())
	if _, err := client.Call(context.Background(), "kagent", "missing", "hi", ""); err == nil {
		t.Errorf("expected resolve error against a dead endpoint")
	}
}

func TestTextHelpers(t *testing.T) {
	tests := map[string]struct {
		task *a2a.Task
		want string
	}{
		"artifacts win": {
			task: &a2a.Task{
				Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
				Artifacts: []*a2a.Artifact{
					{Parts: a2a.ContentParts{a2a.NewTextPart("part one")}},
					{Parts: a2a.ContentParts{a2a.NewTextPart("part two")}},
				},
			},
			want: "part one\n\npart two",
		},
		"status message fallback": {
			task: &a2a.Task{Status: a2a.TaskStatus{
				State:   a2a.TaskStateCompleted,
				Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("from status")),
			}},
			want: "from status",
		},
		"history fallback": {
			task: &a2a.Task{
				Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
				History: []*a2a.Message{
					a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("prompt")),
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("from history")),
				},
			},
			want: "from history",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := taskText(tc.task); got != tc.want {
				t.Errorf("taskText = %q, want %q", got, tc.want)
			}
		})
	}

	empty := &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}
	if got := taskText(empty); got == "" {
		t.Errorf("taskText fallback must be non-empty")
	}
	if got := partsText(a2a.ContentParts{a2a.NewTextPart(" hi "), a2a.NewTextPart("there")}); got != "hi there" {
		t.Errorf("partsText = %q", got)
	}
}
