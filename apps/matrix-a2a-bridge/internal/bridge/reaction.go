package bridge

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// cancelReactionKey is the emoji a room member adds to an agent's in-flight placeholder to cancel
// the delegation (#98). ❌ (U+274C) is the documented trigger; token burn stops as soon as it lands.
const cancelReactionKey = "❌"

const (
	positiveQualityReactionKey = "👍"
	negativeQualityReactionKey = "👎"
	positiveQualitySignal      = "positive"
	negativeQualitySignal      = "negative"
)

// HandleReaction cancels a long-running delegation when an authorized room member reacts to its
// placeholder with the cancel emoji. Everything else is ignored: reactions are untrusted room input
// and must never themselves invoke an agent or act on another automation's output. The handler is
// deliberately cheap — a registry lookup plus at most one cached power-level read — and never calls
// A2A or an LLM; the actual agent-side cancel and room edit happen on the delegation's own goroutine.
func (b *Bridge) HandleReaction(ctx context.Context, evt *event.Event) {
	if b.isOwnUser(evt.Sender) {
		return // the bridge's own ghosts/bot never drive cancellation
	}
	rel := evt.Content.AsReaction().GetRelatesTo()
	target := rel.GetAnnotationID()
	if target == "" {
		return
	}
	switch rel.GetAnnotationKey() {
	case positiveQualityReactionKey:
		b.recordReplyQualitySignal(ctx, evt, target, positiveQualitySignal)
		return
	case negativeQualityReactionKey:
		b.recordReplyQualitySignal(ctx, evt, target, negativeQualitySignal)
		return
	case cancelReactionKey:
		// Continue through the existing cancel-only path below.
	default:
		return
	}
	task, ok := b.inflight.lookup(target)
	if !ok {
		return // the reaction targets something that is not a cancelable in-flight task
	}
	if !b.mayCancel(ctx, evt.Sender, task) {
		b.log.Info(
			"ignoring cancel reaction from unauthorized member",
			"sender", evt.Sender,
			"room", evt.RoomID,
			"placeholder", target,
		)
		return
	}
	if task.requestCancel(evt.Sender) {
		b.log.Info(
			"cancel requested from room",
			"sender", evt.Sender,
			"room", evt.RoomID,
			"placeholder", target,
		)
	}
}

// recordReplyQualitySignal accepts only a normalized thumbs-up/down on a successfully projected,
// process-known terminal m.notice. It never reads the reply, calls Matrix/A2A, or emits another
// event. High-cardinality operational identifiers stay on the span/log; Prometheus uses only the
// bounded ghost and signal dimensions.
func (b *Bridge) recordReplyQualitySignal(
	ctx context.Context,
	evt *event.Event,
	target id.EventID,
	signal string,
) {
	reply, ok := b.replies.lookup(target, evt.RoomID)
	if !ok {
		return
	}
	agentReplyQualitySignals.WithLabelValues(reply.ghost, signal).Inc()
	_, span := b.tracer.Start(
		ctx,
		"fgentic.agent_reply.quality_signal",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("matrix.room_id", evt.RoomID.String()),
			attribute.String("matrix.event_id", evt.ID.String()),
			attribute.String("matrix.sender", evt.Sender.String()),
			attribute.String("matrix.reply_event_id", reply.event.String()),
			attribute.String("fgentic.ghost", reply.ghost),
			attribute.String("fgentic.quality_signal", signal),
		),
	)
	span.End()
	b.log.Info(
		"recorded agent reply quality signal",
		"sender", evt.Sender,
		"room", evt.RoomID,
		"reaction_event", evt.ID,
		"reply_event", reply.event,
		"ghost", reply.ghost,
		"signal", signal,
	)
}

// mayCancel authorizes a cancel gesture: the original delegating sender may always stop their own
// task; anyone else needs at least the configured moderator power level in the room. Power levels
// are read from the cached appservice StateStore only — the appservice keeps it current from the
// room's m.room.power_levels state events (mautrix UpdateStateStore), so a hot fetch is never needed
// and this stays non-blocking inside the synchronous event processor. A cold or unreadable cache
// fails closed: only the original delegating sender can cancel until power levels are observed.
func (b *Bridge) mayCancel(ctx context.Context, sender id.UserID, task *inflightTask) bool {
	if sender == task.originalSender {
		return true
	}
	levels, err := b.as.StateStore.GetPowerLevels(ctx, task.room)
	if err != nil || levels == nil {
		b.log.Warn(
			"cancel authorization: power levels unavailable, only the original sender may cancel",
			"sender", sender,
			"room", task.room,
			"err", err,
		)
		return false
	}
	return levels.GetUserLevel(sender) >= b.cfg.CancelModeratorPowerLevel
}
