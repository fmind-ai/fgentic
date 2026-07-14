package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
)

type tracingA2AClient struct {
	callContext trace.SpanContext
}

func (c *tracingA2AClient) Call(ctx context.Context, _ a2aclient.Target, _, _ string, _ []a2aclient.InboundFile) (a2aclient.Result, error) {
	c.callContext = trace.SpanContextFromContext(ctx)
	return a2aclient.Result{Text: "traced reply", Terminal: true}, nil
}

func (c *tracingA2AClient) Continue(ctx context.Context, target a2aclient.Target, text, contextID, _ string) (a2aclient.Result, error) {
	return c.Call(ctx, target, text, contextID, nil)
}

func (*tracingA2AClient) PollTask(context.Context, a2aclient.Target, string) (a2aclient.Result, error) {
	return a2aclient.Result{}, fmt.Errorf("unexpected task poll")
}

func (*tracingA2AClient) CancelTask(context.Context, a2aclient.Target, string) error {
	return fmt.Errorf("unexpected task cancel")
}

func (*tracingA2AClient) ResolveAgentCard(context.Context, a2aclient.Target) (*a2a.AgentCard, error) {
	return nil, fmt.Errorf("unexpected AgentCard resolution")
}

func (*tracingA2AClient) IsReady(target a2aclient.Target) bool {
	return !target.IsRemote()
}

func (*tracingA2AClient) QuoteAdmission(a2aclient.Target, uint64) a2aclient.QuoteVerdict {
	return a2aclient.QuoteNotApplicable
}

func TestDispatchEmitsContentFreeDelegationSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	client := &tracingA2AClient{}
	b, _, evt, ref, _ := pollingHarness(t, client)
	b.tracer = provider.Tracer(tracerName)
	b.dispatchWithDedupVerdict(
		t.Context(),
		evt,
		ref,
		"agent-k8s",
		"sensitive prompt body",
		b.agents.IdentifySender(evt.Sender),
		dedupVerdictAccepted,
	)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "fgentic.delegation" {
		t.Errorf("span name = %q", span.Name)
	}
	if !client.callContext.IsValid() || client.callContext.TraceID() != span.SpanContext.TraceID() {
		t.Errorf("A2A call context = %s, want trace %s", client.callContext.TraceID(), span.SpanContext.TraceID())
	}

	attributes := attributeMap(span.Attributes)
	for key, want := range map[string]any{
		"matrix.room_id":                evt.RoomID.String(),
		"matrix.event_id":               evt.ID.String(),
		"matrix.sender":                 evt.Sender.String(),
		"fgentic.sender_origin_kind":    string(senderOriginMatrix),
		"fgentic.sender_origin_network": matrixOriginNetwork,
		"fgentic.ghost":                 "agent-k8s",
		"a2a.agent_path":                "/api/a2a/kagent/k8s-agent",
		"fgentic.outcome":               outcomeOK,
		"fgentic.rate_limited":          false,
		"fgentic.dedup_skipped":         false,
	} {
		if got := attributes[key]; got != want {
			t.Errorf("span attribute %s = %#v, want %#v", key, got, want)
		}
	}

	events := make([]string, 0, len(span.Events))
	for _, event := range span.Events {
		events = append(events, event.Name)
	}
	for _, want := range []string{"queue.dequeued", "a2a.message.send", "a2a.message.result", "matrix.reply.post"} {
		if !slices.Contains(events, want) {
			t.Errorf("span events = %v, missing %q", events, want)
		}
	}
	if serialized := fmt.Sprint(span.Attributes, span.Events); strings.Contains(serialized, "sensitive prompt body") {
		t.Fatal("trace contains Matrix message content")
	}
}

func TestDispatchRedactsFailureContentFromDelegationSpan(t *testing.T) {
	tests := []struct {
		name           string
		client         func(error) *scriptedA2AClient
		configure      func(*Bridge, error)
		wantOutcome    string
		wantStage      string
		wantReason     string
		wantEvent      string
		wantEventCount int
	}{
		{
			name: "message send error",
			client: func(sentinel error) *scriptedA2AClient {
				return &scriptedA2AClient{callErr: sentinel}
			},
			wantOutcome:    outcomeError,
			wantStage:      "message_send",
			wantReason:     "a2a_call_failed",
			wantEvent:      traceEventA2AMessageSendError,
			wantEventCount: 1,
		},
		{
			name: "task timeout",
			client: func(error) *scriptedA2AClient {
				return &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-timeout", Terminal: false}}
			},
			configure: func(b *Bridge, sentinel error) {
				b.pollWait = func(context.Context, time.Duration) error { return sentinel }
			},
			wantOutcome:    outcomeTimeout,
			wantStage:      "task_poll",
			wantReason:     "task_timeout",
			wantEvent:      traceEventA2ATaskTimeout,
			wantEventCount: 1,
		},
		{
			name: "task poll error",
			client: func(sentinel error) *scriptedA2AClient {
				return &scriptedA2AClient{
					callResult: a2aclient.Result{TaskID: "task-poll", Terminal: false},
					polls: []scriptedPoll{
						{err: sentinel},
						{err: sentinel},
						{err: sentinel},
					},
				}
			},
			configure: func(b *Bridge, _ error) {
				b.pollWait = func(context.Context, time.Duration) error { return nil }
			},
			wantOutcome:    outcomeLost,
			wantStage:      "task_poll",
			wantReason:     "task_poll_failed",
			wantEvent:      traceEventA2ATaskPollError,
			wantEventCount: pollErrorBudget,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sentinelText := "sensitive remote failure body: " + test.name
			sentinel := errors.New(sentinelText)
			client := test.client(sentinel)
			b, _, evt, ref, _ := pollingHarness(t, client)
			if test.configure != nil {
				test.configure(b, sentinel)
			}

			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown tracer provider: %v", err)
				}
			})
			b.tracer = provider.Tracer(tracerName)

			b.dispatchWithDedupVerdict(
				t.Context(),
				evt,
				ref,
				"agent-k8s",
				"sensitive prompt body",
				b.agents.IdentifySender(evt.Sender),
				dedupVerdictAccepted,
			)

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("exported spans = %d, want 1", len(spans))
			}
			span := spans[0]
			if !span.SpanContext.IsValid() {
				t.Fatal("exported span context is invalid")
			}
			attributes := attributeMap(span.Attributes)
			for key, want := range map[string]any{
				"fgentic.outcome":         test.wantOutcome,
				"fgentic.terminal_stage":  test.wantStage,
				"fgentic.terminal_reason": test.wantReason,
			} {
				if got := attributes[key]; got != want {
					t.Errorf("span attribute %s = %#v, want %#v", key, got, want)
				}
			}
			if span.Status.Code != codes.Error || span.Status.Description != test.wantOutcome {
				t.Errorf("span status = (%s, %q), want (%s, %q)", span.Status.Code, span.Status.Description, codes.Error, test.wantOutcome)
			}

			eventCount := 0
			for _, event := range span.Events {
				if event.Name == "exception" {
					t.Error("trace contains an exception event")
				}
				if event.Name == test.wantEvent {
					eventCount++
					if len(event.Attributes) != 0 {
						t.Errorf("event %q attributes = %v, want none", event.Name, event.Attributes)
					}
				}
			}
			if eventCount != test.wantEventCount {
				t.Errorf("event %q count = %d, want %d", test.wantEvent, eventCount, test.wantEventCount)
			}
			assertSpanOmitsSentinel(t, span, sentinelText)
		})
	}
}

func assertSpanOmitsSentinel(t *testing.T, span tracetest.SpanStub, sentinel string) {
	t.Helper()
	fields := map[string]any{
		"name":                  span.Name,
		"resource":              span.Resource,
		"attributes":            span.Attributes,
		"events":                span.Events,
		"status":                span.Status,
		"links":                 span.Links,
		"instrumentation scope": span.InstrumentationScope,
	}
	for name, value := range fields {
		if strings.Contains(fmt.Sprint(value), sentinel) {
			t.Errorf("span %s contains error content", name)
		}
	}
	serialized, err := json.Marshal(span)
	if err != nil {
		t.Fatalf("marshal span: %v", err)
	}
	if strings.Contains(string(serialized), sentinel) {
		t.Fatal("serialized span contains error content")
	}
}

func attributeMap(values []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(values))
	for _, value := range values {
		out[string(value.Key)] = value.Value.AsInterface()
	}
	return out
}
