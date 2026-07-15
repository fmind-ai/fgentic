package a2aclient

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func TestCallWithMessageIDPlacesSuppliedIDOnWireAndCallGeneratesID(t *testing.T) {
	recorder := &contractRecorder{}
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		recorder.recordExecution(execCtx)
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("ack")), nil)
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), recorder)
	target := contractTarget(t)

	const suppliedID = "delegation-7f93f726"
	result, err := client.CallWithMessageID(t.Context(), target, suppliedID, "durable prompt", "", nil)
	if err != nil {
		t.Fatalf("CallWithMessageID: %v", err)
	}
	if !result.Terminal || result.Text != "ack" {
		t.Fatalf("CallWithMessageID result = %+v, want terminal ack", result)
	}

	generated, err := client.Call(t.Context(), target, "generated-ID prompt", result.ContextID, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !generated.Terminal || generated.Text != "ack" || generated.ContextID != result.ContextID {
		t.Fatalf("Call result = %+v, want generated-ID terminal ack in context %q", generated, result.ContextID)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.messages) != 2 {
		t.Fatalf("wire messages = %d, want two", len(recorder.messages))
	}
	if got := recorder.messages[0].ID; got != suppliedID {
		t.Errorf("CallWithMessageID wire ID = %q, want %q", got, suppliedID)
	}
	if got := recorder.messages[1].ID; got == "" || got == suppliedID {
		t.Errorf("Call wire ID = %q, want a fresh generated ID", got)
	}
}

func TestResumeTaskFetchesKnownTaskWithoutResending(t *testing.T) {
	recorder := &contractRecorder{}
	store := taskstore.NewInMemory(nil)
	task := &a2a.Task{
		ID:        "persisted-task-42",
		ContextID: "persisted-context-42",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	if _, err := store.Create(t.Context(), task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	executor := executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {
			t.Error("ResumeTask invoked SendMessage")
		}
	})
	client := contractServer(t, executor, store, recorder)

	result, err := client.ResumeTask(t.Context(), contractTarget(t), string(task.ID))
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if result.TaskID != string(task.ID) || result.ContextID != task.ContextID || result.Terminal {
		t.Fatalf("ResumeTask result = %+v, want known working task", result)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.messages) != 0 {
		t.Fatalf("SendMessage executions = %d, want zero", len(recorder.messages))
	}
}

func TestContinueWithMessageIDPlacesSuppliedTaskContinuationOnWire(t *testing.T) {
	recorder := &contractRecorder{}
	store := taskstore.NewInMemory(nil)
	task := &a2a.Task{
		ID:        "input-task-42",
		ContextID: "input-context-42",
		Status:    a2a.TaskStatus{State: a2a.TaskStateInputRequired},
	}
	if _, err := store.Create(t.Context(), task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		recorder.recordExecution(execCtx)
		return func(yield func(a2a.Event, error) bool) {
			yield(
				a2a.NewStatusUpdateEvent(
					execCtx,
					a2a.TaskStateCompleted,
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("continued")),
				),
				nil,
			)
		}
	})
	client := contractServer(t, executor, store, recorder)

	const messageID = "delegation-continuation-42"
	result, err := client.ContinueWithMessageID(
		t.Context(),
		contractTarget(t),
		messageID,
		"the namespace is production",
		task.ContextID,
		string(task.ID),
	)
	if err != nil {
		t.Fatalf("ContinueWithMessageID: %v", err)
	}
	if !result.Terminal || result.Failed || result.TaskID != string(task.ID) {
		t.Fatalf("ContinueWithMessageID result = %+v, want completed existing task", result)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.messages) != 1 {
		t.Fatalf("wire messages = %d, want one", len(recorder.messages))
	}
	message := recorder.messages[0]
	if message.ID != messageID || message.TaskID != task.ID || message.ContextID != task.ContextID {
		t.Errorf(
			"continuation wire identity = (%q, %q, %q), want (%q, %q, %q)",
			message.ID,
			message.TaskID,
			message.ContextID,
			messageID,
			task.ID,
			task.ContextID,
		)
	}
}

func TestCallWithMessageIDClassifiesResponseLossAsAmbiguous(t *testing.T) {
	recorder := &contractRecorder{}
	client := contractServer(t, executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	}), taskstore.NewInMemory(nil), recorder)

	transport := client.localHTTPClient.Transport.(*userTransport)
	base := transport.base
	responseLost := errors.New("provider-controlled response loss detail")
	requestCount := 0
	transport.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != contractAgentPath {
			return base.RoundTrip(req)
		}
		requestCount++
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		return nil, responseLost
	})

	const messageID = "delegation-response-loss"
	_, err := client.CallWithMessageID(
		t.Context(),
		contractTarget(t),
		messageID,
		"sensitive prompt must stay out of errors",
		"",
		nil,
	)
	if err == nil {
		t.Fatal("CallWithMessageID succeeded after response loss")
	}
	if requestCount != 1 {
		t.Fatalf("SendMessage requests = %d, want one", requestCount)
	}
	if !errors.Is(err, ErrSendAcknowledgementAmbiguous) {
		t.Fatalf("error = %v, want ErrSendAcknowledgementAmbiguous", err)
	}
	if !errors.Is(err, responseLost) {
		t.Fatalf("error does not preserve transport cause for errors.Is: %v", err)
	}
	var ambiguous *AmbiguousSendError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error type = %T, want *AmbiguousSendError", err)
	}
	if got := ambiguous.MessageID(); got != messageID {
		t.Errorf("ambiguous message ID = %q, want %q", got, messageID)
	}
	if strings.Contains(err.Error(), responseLost.Error()) || strings.Contains(err.Error(), "sensitive prompt") {
		t.Fatalf("ambiguous error leaked provider or prompt content: %q", err)
	}
}

func TestCallWithMessageIDClassifiesProtocolFailureConservatively(t *testing.T) {
	const providerDetail = "provider-controlled execution detail"
	executor := executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(nil, errors.New(providerDetail))
		}
	})
	client := contractServer(t, executor, taskstore.NewInMemory(nil), &contractRecorder{})

	_, err := client.CallWithMessageID(
		t.Context(),
		contractTarget(t),
		"delegation-protocol-error",
		"prompt",
		"",
		nil,
	)
	if err == nil {
		t.Fatal("CallWithMessageID succeeded after protocol failure")
	}
	if !errors.Is(err, ErrSendAcknowledgementAmbiguous) {
		t.Fatalf("error = %v, want conservative ambiguous classification", err)
	}
	if !errors.Is(err, a2a.ErrInternalError) {
		t.Fatalf("ambiguous error does not preserve protocol cause for errors.Is: %v", err)
	}
	if strings.Contains(err.Error(), providerDetail) {
		t.Fatalf("ambiguous error leaked provider content: %q", err)
	}
}

func TestCallWithMessageIDPreSendFailuresAreNotAmbiguous(t *testing.T) {
	recorder := &contractRecorder{}
	client := contractServer(t, executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	}), taskstore.NewInMemory(nil), recorder)

	tests := []struct {
		name      string
		target    Target
		messageID string
		want      error
	}{
		{name: "missing message ID", target: contractTarget(t), want: ErrMessageIDRequired},
		{name: "invalid target", target: Target{}, messageID: "delegation-invalid-target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.CallWithMessageID(t.Context(), tt.target, tt.messageID, "prompt", "", nil)
			if err == nil {
				t.Fatal("CallWithMessageID succeeded, want pre-send failure")
			}
			if errors.Is(err, ErrSendAcknowledgementAmbiguous) {
				t.Fatalf("pre-send error classified as ambiguous: %v", err)
			}
			var ambiguous *AmbiguousSendError
			if errors.As(err, &ambiguous) {
				t.Fatalf("pre-send error has ambiguous type: %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tt.want)
			}
		})
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.cardRequests != 0 || len(recorder.headers) != 0 {
		t.Fatalf("pre-send validation performed network I/O: card=%d calls=%d", recorder.cardRequests, len(recorder.headers))
	}
}

func TestCallWithMessageIDPreservesRemoteTrustPreflight(t *testing.T) {
	fixture, client, target := newRemoteContractFixture(t, nil, "")

	_, err := client.CallWithMessageID(t.Context(), target, "delegation-untrusted", "prompt", "", nil)
	if !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatalf("CallWithMessageID error = %v, want ErrRemoteTargetUntrusted", err)
	}
	if errors.Is(err, ErrSendAcknowledgementAmbiguous) {
		t.Fatalf("untrusted preflight error classified as ambiguous: %v", err)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.cardHeaders) != 0 || len(fixture.callHeaders) != 0 {
		t.Fatal("CallWithMessageID performed implicit remote discovery or delegation")
	}
}
