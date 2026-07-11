package a2aclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUserTransportInjectsAttributionAndTraceContext(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	otel.SetTextMapPropagator(propagation.TraceContext{})

	var got http.Header
	transport := &userTransport{
		apiKey: "workload-key",
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			got = req.Header.Clone()
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	ctx := trace.ContextWithSpanContext(WithUser(context.Background(), "@alice:matrix.example"), spanContext)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent.example/a2a", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if value := got.Get(userHeader); value != "@alice:matrix.example" {
		t.Errorf("%s = %q", userHeader, value)
	}
	if value := got.Get("Authorization"); value != "Bearer workload-key" {
		t.Errorf("Authorization = %q", value)
	}
	if value := got.Get("traceparent"); value != "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01" {
		t.Errorf("traceparent = %q", value)
	}
}

// The result mapping is the one genuinely fiddly piece of glue (Task vs Message sum type,
// terminal vs still-running tasks). These tests pin its behaviour without a live agent.
func TestToResult(t *testing.T) {
	message := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("hello from agent"))
	message.ContextID = "ctx-0"
	multipart := a2a.NewMessage(
		a2a.MessageRoleAgent,
		a2a.NewTextPart("Hello"),
		a2a.NewTextPart(", world!"),
	)

	tests := []struct {
		name string
		in   a2a.SendMessageResult
		want Result
	}{
		{
			name: "message",
			in:   message,
			want: Result{Text: "hello from agent", ContextID: "ctx-0", Terminal: true},
		},
		{
			name: "multi-part message",
			in:   multipart,
			want: Result{Text: "Hello, world!", Terminal: true},
		},
		{
			name: "task artifact",
			in: &a2a.Task{
				ID:        "task-1",
				ContextID: "ctx-1",
				Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
				Artifacts: []*a2a.Artifact{
					{Parts: a2a.ContentParts{a2a.NewTextPart("the pod is OOMKilled")}},
				},
			},
			want: Result{
				Text:      "the pod is OOMKilled",
				ContextID: "ctx-1",
				TaskID:    "task-1",
				Terminal:  true,
			},
		},
		{
			name: "empty artifacts fall back to status message",
			in: &a2a.Task{
				ContextID: "ctx-2",
				Status: a2a.TaskStatus{
					State:   a2a.TaskStateCompleted,
					Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("done")),
				},
				Artifacts: []*a2a.Artifact{{}, {Parts: a2a.ContentParts{}}},
			},
			want: Result{Text: "done", ContextID: "ctx-2", Terminal: true},
		},
		{
			name: "working task",
			in: &a2a.Task{
				ID:        "task-3",
				ContextID: "ctx-3",
				Status: a2a.TaskStatus{
					State:   a2a.TaskStateWorking,
					Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("crunching…")),
				},
			},
			want: Result{
				Text:      "crunching…",
				ContextID: "ctx-3",
				TaskID:    "task-3",
			},
		},
		{
			name: "empty failed task gets placeholder",
			in:   &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}},
			want: Result{
				Text:     `(agent finished in state "TASK_STATE_FAILED" with no text output)`,
				Terminal: true,
				Failed:   true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toResult(tt.in); got != tt.want {
				t.Errorf("toResult() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
