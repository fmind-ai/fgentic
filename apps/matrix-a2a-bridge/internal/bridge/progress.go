package bridge

import (
	"context"
	"errors"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// taskProgress bounds and deduplicates the threaded working-state updates one long task may post
// under its placeholder (#118). The cap and last-text dedup keep progress from becoming notice spam
// under a flood of status changes (D7 response plane); the final answer stays the m.replace edit of
// the root placeholder, so client thread-summary previews remain correct.
type taskProgress struct {
	root     id.EventID
	max      int
	posted   int
	lastText string
}

// surface posts one bounded, deduplicated progress update as a threaded m.notice rooted at the
// placeholder. It silently drops updates once the per-task cap is reached, when progress is disabled
// (max <= 0), or when the text is empty or unchanged. A failed send does not consume the budget.
func (b *Bridge) surface(ctx context.Context, intent *appservice.IntentAPI, roomID id.RoomID, p *taskProgress, text string) {
	if p.max <= 0 || text == "" || text == p.lastText || p.posted >= p.max {
		return
	}
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	// Thread the update under the placeholder (stable Matrix v1.4). Keeping progress in a thread
	// keeps the main timeline to placeholder -> final answer, while the sub-timeline shows detail.
	content.RelatesTo = &event.RelatesTo{Type: event.RelThread, EventID: p.root}
	if _, err := sendMessageEvent(ctx, intent, roomID, event.EventMessage, automatedContent(content)); err != nil {
		b.log.Warn("post threaded task progress", "room", roomID, "err", err)
		return
	}
	p.lastText = text
	p.posted++
}

// pinPlaceholder best-effort adds the placeholder to the room's pinned events so a running long task
// is visible without opening its thread (#118). Opt-in (PinInFlightTasks) and degrading silently:
// ghosts usually lack the state-event power level, and a "what is running" hint is not worth failing
// a delegation. Per-room FIFO dispatch serializes a room's tasks, so this read-modify-write cannot
// race another pin in the same room.
func (b *Bridge) pinPlaceholder(ctx context.Context, intent *appservice.IntentAPI, roomID id.RoomID, placeholder id.EventID) {
	b.updatePinned(ctx, intent, roomID, "pin", func(pinned []id.EventID) ([]id.EventID, bool) {
		if slices.Contains(pinned, placeholder) {
			return nil, false
		}
		return append(pinned, placeholder), true
	})
}

// unpinPlaceholder best-effort removes the placeholder from the room's pinned events when a task
// reaches any terminal state. Like pinning it degrades silently.
func (b *Bridge) unpinPlaceholder(ctx context.Context, intent *appservice.IntentAPI, roomID id.RoomID, placeholder id.EventID) {
	b.updatePinned(ctx, intent, roomID, "unpin", func(pinned []id.EventID) ([]id.EventID, bool) {
		next := slices.DeleteFunc(slices.Clone(pinned), func(e id.EventID) bool { return e == placeholder })
		return next, len(next) != len(pinned)
	})
}

// updatePinned reads the room's current pinned events, applies mutate, and writes the result back
// only when it changed. A missing pinned-events state (M_NOT_FOUND) is treated as an empty list; any
// other read error skips the update so a transient failure never clobbers existing pins.
func (b *Bridge) updatePinned(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	op string,
	mutate func([]id.EventID) ([]id.EventID, bool),
) {
	var current event.PinnedEventsEventContent
	if err := intent.Client.StateEvent(ctx, roomID, event.StatePinnedEvents, "", &current); err != nil {
		if !errors.Is(err, mautrix.MNotFound) {
			b.log.Info("task placeholder "+op+" skipped: cannot read pinned events", "room", roomID, "err", err)
			return
		}
		current.Pinned = nil // no pinned events yet — start from empty
	}
	next, changed := mutate(current.Pinned)
	if !changed {
		return
	}
	if _, err := sendStateEvent(ctx, intent, roomID, event.StatePinnedEvents, "", &event.PinnedEventsEventContent{Pinned: next}); err != nil {
		b.log.Info("task placeholder "+op+" skipped (likely missing state-event power level)", "room", roomID, "err", err)
	}
}
