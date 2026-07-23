package a2aclient

import (
	"context"
	"math"
	"net/http"
	"reflect"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/modelcatalog"
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
		apiKey:          "workload-key",
		localPolicyData: true,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			got = req.Header.Clone()
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	ctx := WithDataClassification(
		WithUser(context.Background(), "@alice:matrix.example"),
		modelcatalog.ClassificationRestricted,
	)
	ctx = trace.ContextWithSpanContext(ctx, spanContext)
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
	if value := got.Get(DataClassificationHeader); value != "restricted" {
		t.Errorf("%s = %q", DataClassificationHeader, value)
	}
	if value := got.Get("traceparent"); value != "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01" {
		t.Errorf("traceparent = %q", value)
	}
}

func TestUserTransportDefaultsMissingClassificationToRegulated(t *testing.T) {
	var got http.Header
	transport := &userTransport{
		localPolicyData: true,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			got = req.Header.Clone()
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://agent.example/a2a", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if value := got.Get(DataClassificationHeader); value != "regulated" {
		t.Errorf("%s = %q, want fail-closed regulated", DataClassificationHeader, value)
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
			name: "completed task carries kagent token usage (#99)",
			in: &a2a.Task{
				ID:        "task-usage",
				ContextID: "ctx-usage",
				Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
				Metadata: map[string]any{
					"kagent_usage_metadata": map[string]any{
						"promptTokenCount":     float64(10),
						"candidatesTokenCount": float64(2),
						"totalTokenCount":      float64(12),
					},
				},
				Artifacts: []*a2a.Artifact{
					{Parts: a2a.ContentParts{a2a.NewTextPart("answer")}},
				},
			},
			want: Result{
				Text:        "answer",
				ContextID:   "ctx-usage",
				TaskID:      "task-usage",
				Terminal:    true,
				TotalTokens: 12,
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
		{
			name: "input-required task carries the agent's question (#116)",
			in: &a2a.Task{
				ID:        "task-4",
				ContextID: "ctx-4",
				Status: a2a.TaskStatus{
					State:   a2a.TaskStateInputRequired,
					Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("which namespace?")),
				},
			},
			want: Result{Text: "which namespace?", ContextID: "ctx-4", TaskID: "task-4", InputRequired: true},
		},
		{
			name: "auth-required task is non-terminal and flagged (#116)",
			in:   &a2a.Task{ID: "task-5", Status: a2a.TaskStatus{State: a2a.TaskStateAuthRequired}},
			want: Result{TaskID: "task-5", AuthRequired: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toResult(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("toResult() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestKagentTotalTokens(t *testing.T) {
	usage := func(total any) map[string]any {
		return map[string]any{"kagent_usage_metadata": map[string]any{"totalTokenCount": total}}
	}
	tests := []struct {
		name     string
		metadata map[string]any
		want     int
	}{
		{name: "nil metadata is not attributable", metadata: nil, want: 0},
		{name: "missing usage key", metadata: map[string]any{"kagent_app_name": "x"}, want: 0},
		{name: "usage present", metadata: usage(float64(42)), want: 42},
		{name: "zero total is not attributable", metadata: usage(float64(0)), want: 0},
		{name: "negative total is rejected", metadata: usage(float64(-5)), want: 0},
		{name: "NaN total is rejected", metadata: usage(math.NaN()), want: 0},
		{name: "inf total is rejected", metadata: usage(math.Inf(1)), want: 0},
		{name: "wrong type total is ignored", metadata: usage("12"), want: 0},
		{name: "over-range total saturates", metadata: usage(math.MaxFloat64), want: math.MaxInt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kagentTotalTokens(tt.metadata); got != tt.want {
				t.Errorf("kagentTotalTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}
