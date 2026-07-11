package bridge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
)

type tracingA2AClient struct {
	callContext trace.SpanContext
}

func (c *tracingA2AClient) Call(ctx context.Context, _, _, _ string) (a2aclient.Result, error) {
	c.callContext = trace.SpanContextFromContext(ctx)
	return a2aclient.Result{Text: "traced reply", Terminal: true}, nil
}

func (*tracingA2AClient) PollTask(context.Context, string, string) (a2aclient.Result, error) {
	return a2aclient.Result{}, fmt.Errorf("unexpected task poll")
}

func (*tracingA2AClient) ResolveAgentCard(context.Context, string) (*a2a.AgentCard, error) {
	return nil, fmt.Errorf("unexpected AgentCard resolution")
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

func attributeMap(values []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(values))
	for _, value := range values {
		out[string(value.Key)] = value.Value.AsInterface()
	}
	return out
}
