package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const roomWelcomeNoticeScope = "room-welcome"

// maybeWelcomeRoom owns the single welcome attempt for a room before spending notice capacity.
// Marking first means an invite storm cannot turn a temporarily exhausted bucket into delayed
// amplification on a later rejoin. The stable Matrix transaction ID additionally deduplicates a
// dev-mode process restart, where the in-memory marker is intentionally not durable.
func (b *Bridge) maybeWelcomeRoom(ctx context.Context, evt *event.Event, intent *appservice.IntentAPI) {
	if !b.cfg.WelcomeEnabled {
		return
	}
	if intent == nil {
		b.log.Error("create bot intent for room welcome", "room", evt.RoomID)
		return
	}
	first, err := b.store.MarkRoomWelcomed(ctx, evt.RoomID.String())
	if err != nil {
		b.log.Error("record room welcome marker", "room", evt.RoomID, "err", err)
		return
	}
	if !first {
		return
	}
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.allowNotice(sender, evt.RoomID, roomWelcomeNoticeScope) {
		b.log.Info("suppressing room welcome after notice rate limit", "room", evt.RoomID)
		return
	}
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: b.roomWelcomeText(ctx, evt.Sender, evt.RoomID)}
	response, err := sendMessageEvent(
		ctx,
		intent,
		evt.RoomID,
		event.EventMessage,
		automatedContent(content),
		mautrix.ReqSendEvent{TransactionID: roomWelcomeTransactionID(evt.RoomID)},
	)
	if err != nil {
		b.log.Error("post room welcome", "room", evt.RoomID, "err", err)
		return
	}
	b.log.Info("posted room welcome", "room", evt.RoomID, "event", response.EventID)
}

func (b *Bridge) roomWelcomeText(ctx context.Context, sender id.UserID, roomID id.RoomID) string {
	return fmt.Sprintf(
		"Welcome to this agent room. Messages are plaintext, so share only approved task context.\n\n%s\n\n"+
			"Delegate with a full @mention, or use !ask <agent> <prompt>. Run !agents to refresh the sender-filtered gallery, and !budget to inspect admission availability. Clients that send leading slashes unchanged may also use /ask, /agents, and /budget.",
		b.agentDirectoryText(ctx, sender, roomID),
	)
}

func roomWelcomeTransactionID(roomID id.RoomID) string {
	digest := sha256.Sum256([]byte(roomID))
	return "fgentic-room-welcome-" + hex.EncodeToString(digest[:])
}
