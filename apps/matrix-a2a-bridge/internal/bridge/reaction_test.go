package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
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

func TestReactionIgnoresNonCancelGestures(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, _ := pollingHarness(t, client)
	// Register a fake in-flight task so the cancel key would match, isolating the key/target/self checks.
	b.inflight.register(&inflightTask{placeholder: "$reply-1", originalSender: evt.Sender, cancelPoll: func() {}})

	cases := []*event.Event{
		reactionEvent(evt.Sender, evt.RoomID, "$reply-1", "👍"),                       // wrong emoji
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
