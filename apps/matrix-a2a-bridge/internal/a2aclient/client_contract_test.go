package a2aclient

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

const (
	contractAgentPath = "/api/a2a/kagent/contract-agent"

	// a2a-go v2.3.1, pinned to kagent, negotiates the v1.0 wire protocol. Keep this
	// literal independent of a2a.Version so an SDK default flip breaks the contract test.
	pinnedKagentWireVersion = "1.0"
)

type executorFunc func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error]

func (fn executorFunc) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return fn(ctx, execCtx)
}

func (executorFunc) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

type contractRecorder struct {
	mu           sync.Mutex
	cardRequests int
	cardHeaders  []http.Header
	headers      []http.Header
	messages     []*a2a.Message
	contexts     []string
}

func (r *contractRecorder) recordRequest(header http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers = append(r.headers, header.Clone())
}

func (r *contractRecorder) recordExecution(execCtx *a2asrv.ExecutorContext) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, execCtx.Message)
	r.contexts = append(r.contexts, execCtx.ContextID)
}

func contractServer(
	t *testing.T,
	executor a2asrv.AgentExecutor,
	store taskstore.Store,
	recorder *contractRecorder,
) *Client {
	t.Helper()

	handler := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(store))
	endpoint := a2asrv.NewJSONRPCHandler(handler)
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	card := &a2a.AgentCard{
		Name:                "Fgentic contract fixture",
		Description:         "In-process A2A wire fixture",
		Version:             "test",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(server.URL+contractAgentPath, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{},
		Skills:              []a2a.AgentSkill{},
	}
	mux.HandleFunc(contractAgentPath+a2asrv.WellKnownAgentCardPath, func(w http.ResponseWriter, req *http.Request) {
		recorder.mu.Lock()
		recorder.cardRequests++
		recorder.cardHeaders = append(recorder.cardHeaders, req.Header.Clone())
		recorder.mu.Unlock()
		a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, req)
	})
	mux.HandleFunc(contractAgentPath, func(w http.ResponseWriter, req *http.Request) {
		recorder.recordRequest(req.Header)
		endpoint.ServeHTTP(w, req)
	})

	return New(server.URL, "contract-api-key", slog.Default())
}

func TestResolveAgentCardUsesGatewayCredentialAndBypassesClientCache(t *testing.T) {
	recorder := &contractRecorder{}
	client := contractServer(t, executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	}), taskstore.NewInMemory(nil), recorder)

	for range 2 {
		card, err := client.ResolveAgentCard(t.Context(), contractAgentPath)
		if err != nil {
			t.Fatalf("ResolveAgentCard: %v", err)
		}
		if card.Name != "Fgentic contract fixture" || card.Description != "In-process A2A wire fixture" {
			t.Fatalf("AgentCard = (%q, %q)", card.Name, card.Description)
		}
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.cardRequests != 2 {
		t.Fatalf("AgentCard requests = %d, want two uncached refreshes", recorder.cardRequests)
	}
	for i, header := range recorder.cardHeaders {
		if got := header.Get("Authorization"); got != "Bearer contract-api-key" {
			t.Errorf("request %d Authorization = %q, want bridge workload credential", i+1, got)
		}
	}
}

func TestClientContract_MessageContextAttributionAndWireVersion(t *testing.T) {
	recorder := &contractRecorder{}
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		recorder.recordExecution(execCtx)
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("ack")), nil)
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), recorder)
	ctx := WithUser(t.Context(), "@alice:fgentic.example")

	first, err := client.Call(ctx, contractAgentPath, "first prompt", "")
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if !first.Terminal || first.Text != "ack" || first.ContextID == "" {
		t.Fatalf("first Call result = %+v, want terminal ack with a context ID", first)
	}
	second, err := client.Call(ctx, contractAgentPath, "second prompt", first.ContextID)
	if err != nil {
		t.Fatalf("second Call: %v", err)
	}
	if second.ContextID != first.ContextID {
		t.Fatalf("context ID = %q, want round-trip %q", second.ContextID, first.ContextID)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.cardRequests != 1 {
		t.Fatalf("AgentCard requests = %d, want one cached resolution", recorder.cardRequests)
	}
	if len(recorder.headers) != 2 || len(recorder.messages) != 2 {
		t.Fatalf("wire requests = %d, executions = %d, want two each", len(recorder.headers), len(recorder.messages))
	}
	for i, header := range recorder.headers {
		if got := header.Get(userHeader); got != "@alice:fgentic.example" {
			t.Errorf("request %d %s = %q, want Matrix sender", i+1, userHeader, got)
		}
		if got := header.Get("Authorization"); got != "Bearer contract-api-key" {
			t.Errorf("request %d Authorization = %q, want bridge workload credential", i+1, got)
		}
		if got := header.Get(a2a.SvcParamVersion); got != pinnedKagentWireVersion {
			t.Fatalf(
				"request %d %s = %q, want %q; the A2A negotiated default changed — review D10 and the pinned kagent wire contract",
				i+1, a2a.SvcParamVersion, got, pinnedKagentWireVersion,
			)
		}
	}
	if got := partsText(recorder.messages[0].Parts); got != "first prompt" {
		t.Errorf("first prompt = %q", got)
	}
	if got := partsText(recorder.messages[1].Parts); got != "second prompt" {
		t.Errorf("second prompt = %q", got)
	}
	if recorder.contexts[1] != recorder.contexts[0] {
		t.Errorf("executor context = %q, want %q", recorder.contexts[1], recorder.contexts[0])
	}
}

func TestClientContract_TerminalTask(t *testing.T) {
	recorder := &contractRecorder{}
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(&a2a.Task{
				ID:        execCtx.TaskID,
				ContextID: execCtx.ContextID,
				Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
				Artifacts: []*a2a.Artifact{{
					ID:    a2a.NewArtifactID(),
					Parts: a2a.ContentParts{a2a.NewTextPart("task artifact")},
				}},
			}, nil)
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), recorder)

	result, err := client.Call(t.Context(), contractAgentPath, "run task", "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !result.Terminal || result.Failed || result.Text != "task artifact" || result.TaskID == "" {
		t.Fatalf("Call result = %+v, want completed task artifact", result)
	}
}

func TestClientContract_WorkingTaskCanBePolledToCompletion(t *testing.T) {
	recorder := &contractRecorder{}
	store := taskstore.NewInMemory(nil)
	release := make(chan struct{})
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if !yield(task, nil) {
				return
			}
			<-release
			yield(
				a2a.NewStatusUpdateEvent(
					task,
					a2a.TaskStateCompleted,
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("finished")),
				),
				nil,
			)
		}
	})
	client := contractServer(t, executor, store, recorder)

	working, err := client.Call(t.Context(), contractAgentPath, "long task", "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if working.Terminal || working.TaskID == "" || working.ContextID == "" {
		t.Fatalf("Call result = %+v, want non-terminal task", working)
	}

	close(release)

	var result Result
	for range 100 {
		result, err = client.PollTask(t.Context(), contractAgentPath, working.TaskID)
		if err != nil {
			t.Fatalf("PollTask: %v", err)
		}
		if result.Terminal {
			break
		}
	}
	if !result.Terminal || result.Failed || result.Text != "finished" {
		t.Fatalf("PollTask result = %+v, want completed task", result)
	}
}

func TestClientContract_ExecutorErrorIsContextualized(t *testing.T) {
	recorder := &contractRecorder{}
	executor := executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(nil, errors.New("scripted executor failure"))
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), recorder)

	_, err := client.Call(t.Context(), contractAgentPath, "fail", "")
	if err == nil {
		t.Fatal("Call succeeded, want protocol error")
	}
	if want := "a2a message/send to " + contractAgentPath; !strings.Contains(err.Error(), want) {
		t.Fatalf("Call error = %q, want context %q", err, want)
	}
}

func TestClientContract_EmptyMessageCrossesWire(t *testing.T) {
	recorder := &contractRecorder{}
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx), nil)
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), recorder)

	result, err := client.Call(t.Context(), contractAgentPath, "return no content", "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !result.Terminal || result.Failed || result.Text != "" || result.ContextID == "" {
		t.Fatalf("Call result = %+v, want an empty terminal message with a context ID", result)
	}
}

func TestClientContract_CallHonorsDeadline(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	executor := executorFunc(func(ctx context.Context, _ *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			startedOnce.Do(func() { close(started) })
			<-ctx.Done()
			yield(nil, ctx.Err())
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), &contractRecorder{})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, contractAgentPath, "wait forever", "")
		callDone <- err
	}()

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("executor did not start before deadline: %v", ctx.Err())
	}

	err := <-callDone
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v, want context deadline exceeded", err)
	}
	if want := "a2a message/send to " + contractAgentPath; !strings.Contains(err.Error(), want) {
		t.Fatalf("Call error = %q, want context %q", err, want)
	}
}
