package bridge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
)

// reactionEvent builds an m.reaction annotation event pointing at target with the given key.
func reactionEvent(sender id.UserID, room id.RoomID, target id.EventID, key string) *event.Event {
	return &event.Event{
		Type:   event.EventReaction,
		Sender: sender,
		RoomID: room,
		Content: event.Content{Parsed: &event.ReactionEventContent{
			RelatesTo: event.RelatesTo{
				Type:    event.RelAnnotation,
				EventID: target,
				Key:     key,
			},
		}},
	}
}

func cancelReaction(sender id.UserID, room id.RoomID, target id.EventID) *event.Event {
	return reactionEvent(sender, room, target, cancelReactionKey)
}

// startLongTask runs awaitTask for a non-terminal task on a background goroutine, using an injected
// pollWait that only returns when the poll context ends (a cancel or the TASK_TIMEOUT). It returns
// the placeholder event ID once the task is registered, and a channel delivering the terminal audit.
func startLongTask(
	t *testing.T,
	b *Bridge,
	intent *appservice.IntentAPI,
	evt *event.Event,
	ref *AgentRef,
	res a2aclient.Result,
) (id.EventID, <-chan delegationAuditResult) {
	t.Helper()
	b.pollWait = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	a2aCtx := a2aclient.WithUser(t.Context(), evt.Sender.String())
	done := make(chan delegationAuditResult, 1)
	go func() {
		done <- b.awaitTask(t.Context(), a2aCtx, intent, evt, ref, "agent-k8s", res, "")
	}()

	placeholder := id.EventID("$reply-1") // the homeserver stub returns this for the first send
	for range 1000 {
		if _, ok := b.inflight.lookup(placeholder); ok {
			return placeholder, done
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("long task never registered for cancellation")
	return "", nil
}

func TestCancelReactionFromOriginalSenderStopsTask(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"}}
	b, intent, evt, ref, recorder := pollingHarness(t, client)

	placeholder, done := startLongTask(t, b, intent, evt, ref, client.callResult)
	b.HandleReaction(t.Context(), cancelReaction(evt.Sender, evt.RoomID, placeholder))
	audit := <-done

	if audit.outcome != outcomeCanceled {
		t.Fatalf("outcome = %q, want %q", audit.outcome, outcomeCanceled)
	}
	if audit.terminalReason != "canceled_by_room" || audit.terminalStage != "task_cancel" {
		t.Fatalf("terminal = (%q, %q), want (task_cancel, canceled_by_room)", audit.terminalStage, audit.terminalReason)
	}
	if audit.canceledBy != evt.Sender.String() {
		t.Fatalf("canceledBy = %q, want %q", audit.canceledBy, evt.Sender)
	}
	if len(client.cancelTasks) != 1 || client.cancelTasks[0] != "task-1" {
		t.Fatalf("agent-side cancels = %v, want [task-1]", client.cancelTasks)
	}
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("Matrix events = %d, want placeholder + cancel edit", len(events))
	}
	if events[1].NewContent == nil || !strings.Contains(events[1].NewContent.Body, "canceled by "+evt.Sender.String()) {
		t.Fatalf("cancel edit = %+v, want a 'canceled by' notice naming the sender", events[1].NewContent)
	}
	if _, ok := b.inflight.lookup(placeholder); ok {
		t.Fatal("task still registered after cancellation")
	}
}

func TestCancelReactionFromModeratorStopsTask(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"}}
	b, intent, evt, ref, _ := pollingHarness(t, client)
	b.cfg.CancelModeratorPowerLevel = 50
	moderator := id.NewUserID("mod", ownServer)
	if err := b.as.StateStore.SetPowerLevels(t.Context(), evt.RoomID, &event.PowerLevelsEventContent{
		Users: map[id.UserID]int{moderator: 50},
	}); err != nil {
		t.Fatalf("SetPowerLevels: %v", err)
	}

	placeholder, done := startLongTask(t, b, intent, evt, ref, client.callResult)
	b.HandleReaction(t.Context(), cancelReaction(moderator, evt.RoomID, placeholder))
	audit := <-done

	if audit.outcome != outcomeCanceled || audit.canceledBy != moderator.String() {
		t.Fatalf("audit = (%q, %q), want (canceled, %q)", audit.outcome, audit.canceledBy, moderator)
	}
	if len(client.cancelTasks) != 1 {
		t.Fatalf("agent-side cancels = %v, want exactly one", client.cancelTasks)
	}
}

func TestCancelReactionFromUnauthorizedMemberIgnored(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1"}}
	b, intent, evt, ref, _ := pollingHarness(t, client)
	b.cfg.CancelModeratorPowerLevel = 50
	// A known but empty power-level map: the non-originating reactor sits at the default level 0,
	// below the moderator threshold. Storing it avoids a network state fetch in the authz path.
	if err := b.as.StateStore.SetPowerLevels(t.Context(), evt.RoomID, &event.PowerLevelsEventContent{}); err != nil {
		t.Fatalf("SetPowerLevels: %v", err)
	}

	placeholder, done := startLongTask(t, b, intent, evt, ref, client.callResult)

	bystander := id.NewUserID("bob", ownServer)
	b.HandleReaction(t.Context(), cancelReaction(bystander, evt.RoomID, placeholder))

	// The task keeps running: an unauthorized reaction neither cancels nor calls the agent.
	task, ok := b.inflight.lookup(placeholder)
	if !ok {
		t.Fatal("task deregistered after an ignored reaction")
	}
	if got := task.canceler(); got != "" {
		t.Fatalf("unauthorized reaction canceled the task (canceler=%q)", got)
	}
	if len(client.cancelTasks) != 0 {
		t.Fatalf("unauthorized reaction triggered an agent-side cancel: %v", client.cancelTasks)
	}

	// The original sender can still cancel, proving the task was only unauthorized for the bystander.
	b.HandleReaction(t.Context(), cancelReaction(evt.Sender, evt.RoomID, placeholder))
	audit := <-done
	if audit.outcome != outcomeCanceled || audit.canceledBy != evt.Sender.String() {
		t.Fatalf("audit = (%q, %q), want (canceled, %q)", audit.outcome, audit.canceledBy, evt.Sender)
	}
}

func TestQualityReactionsOnKnownAgentReplyRecordContentFreeSignal(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		signal string
	}{
		{name: "positive", key: positiveQualityReactionKey, signal: positiveQualitySignal},
		{name: "negative", key: negativeQualityReactionKey, signal: negativeQualitySignal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown tracer provider: %v", err)
				}
			})

			client := &scriptedA2AClient{}
			b, intent, evt, ref, recorder := pollingHarness(t, client)
			b.tracer = provider.Tracer(tracerName)
			var output strings.Builder
			setBridgeLogOutput(b, &output)
			replyID, _, _ := b.deliverReply(t.Context(), intent, evt, "", "agent-k8s", ref,
				a2aclient.Result{Text: "sensitive reply body", Terminal: true})
			if replyID == "" {
				t.Fatal("terminal m.notice reply was not projected")
			}
			events := recorder.snapshot()
			if len(events) != 1 || events[0].MsgType != event.MsgNotice {
				t.Fatalf("projected events = %+v, want one m.notice", events)
			}

			metric := agentReplyQualitySignals.WithLabelValues("agent-k8s", tt.signal)
			before := counterValue(t, metric)
			reaction := reactionEvent(id.NewUserID("reviewer", ownServer), evt.RoomID, replyID, tt.key)
			reaction.ID = id.EventID("$quality-" + tt.signal)
			b.HandleReaction(t.Context(), reaction)

			if got := counterValue(t, metric); got != before+1 {
				t.Errorf("quality metric = %v, want %v", got, before+1)
			}
			if client.callCount != 0 {
				t.Fatalf("quality reaction made %d A2A calls", client.callCount)
			}
			if got := len(recorder.snapshot()); got != 1 {
				t.Fatalf("quality reaction emitted %d additional Matrix events", got-1)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 || spans[0].Name != "fgentic.agent_reply.quality_signal" {
				t.Fatalf("quality spans = %+v, want one normalized signal span", spans)
			}
			attributes := attributeMap(spans[0].Attributes)
			for key, want := range map[string]any{
				"matrix.room_id":         evt.RoomID.String(),
				"matrix.event_id":        reaction.ID.String(),
				"matrix.sender":          reaction.Sender.String(),
				"matrix.reply_event_id":  replyID.String(),
				"fgentic.ghost":          "agent-k8s",
				"fgentic.quality_signal": tt.signal,
			} {
				if got := attributes[key]; got != want {
					t.Errorf("span attribute %s = %#v, want %#v", key, got, want)
				}
			}
			if serialized := fmt.Sprint(spans, output.String()); strings.Contains(serialized, "sensitive reply body") {
				t.Fatal("quality telemetry contains agent reply content")
			}
		})
	}
}

func TestQualityReactionIgnoresUnknownWrongRoomAndOwnUsers(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.replies.record(agentReplyRef{room: evt.RoomID, event: "$known-reply", ghost: "agent-k8s"})
	metric := agentReplyQualitySignals.WithLabelValues("agent-k8s", positiveQualitySignal)
	before := counterValue(t, metric)

	cases := []*event.Event{
		reactionEvent(id.NewUserID("reviewer", ownServer), evt.RoomID, "$unknown", positiveQualityReactionKey),
		reactionEvent(id.NewUserID("reviewer", ownServer), "!other:"+ownServer, "$known-reply", positiveQualityReactionKey),
		reactionEvent(id.NewUserID("agent-k8s", ownServer), evt.RoomID, "$known-reply", positiveQualityReactionKey),
	}
	for _, reaction := range cases {
		b.HandleReaction(t.Context(), reaction)
	}

	if got := counterValue(t, metric); got != before {
		t.Errorf("ignored quality reactions changed metric from %v to %v", before, got)
	}
	if client.callCount != 0 {
		t.Fatalf("ignored quality reactions made %d A2A calls", client.callCount)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("ignored quality reactions emitted Matrix events: %+v", events)
	}
}

func TestReactionIgnoresOtherGesturesAndInvalidCancelTargets(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, _ := pollingHarness(t, client)
	// Register a fake in-flight task so the cancel key would match, isolating the key/target/self checks.
	b.inflight.register(&inflightTask{placeholder: "$reply-1", originalSender: evt.Sender, cancelPoll: func() {}})

	cases := []*event.Event{
		reactionEvent(evt.Sender, evt.RoomID, "$reply-1", "🔥"),                       // unsupported emoji
		cancelReaction(evt.Sender, evt.RoomID, "$unknown"),                           // unknown target
		cancelReaction(id.NewUserID("agent-k8s", ownServer), evt.RoomID, "$reply-1"), // own ghost
	}
	for _, evtR := range cases {
		b.HandleReaction(t.Context(), evtR)
	}

	task, ok := b.inflight.lookup("$reply-1")
	if !ok || task.canceler() != "" {
		t.Fatalf("a non-cancel gesture canceled the task (ok=%v, canceler=%q)", ok, task.canceler())
	}
	if len(client.cancelTasks) != 0 {
		t.Fatalf("a non-cancel gesture called the agent: %v", client.cancelTasks)
	}
}
