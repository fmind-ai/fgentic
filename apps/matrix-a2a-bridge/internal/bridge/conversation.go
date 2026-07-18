package bridge

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	conversationLockStripes = 64
	conversationSweepBatch  = 64
)

type sessionPurger interface {
	Purge(context.Context, string, []string) error
}

// SetSessionPurger installs the internal kagent deletion boundary before Start.
func (b *Bridge) SetSessionPurger(purger sessionPurger) {
	b.sessionPurger = purger
}

func (b *Bridge) conversationLock(roomID, ghost string) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(roomID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(ghost))
	return &b.conversationLocks[hash.Sum32()%conversationLockStripes]
}

func (b *Bridge) handleForgetCommand(ctx context.Context, evt *event.Event, agent string) {
	localpart, ref, found := b.directoryTarget(agent)
	if !found {
		b.handleCommandNotice(ctx, evt, forgetCommand, unknownCommandAgentText)
		return
	}
	sender := b.agents.IdentifySender(evt.Sender)
	if !ref.AllowsSender(sender, b.cfg.ServerName) && !b.mayModerate(ctx, evt.Sender, evt.RoomID) {
		b.handleCommandNotice(ctx, evt, forgetCommand, func() string {
			return "You are not allowed to forget this agent's conversation in this room."
		})
		return
	}
	if ref.Target().IsRemote() {
		b.handleCommandNotice(ctx, evt, forgetCommand, func() string {
			return "This is a remote agent. Its operator has not published a trusted session-deletion contract, so the bridge did not reset or claim to delete it."
		})
		return
	}
	runCtx := b.runCtx
	if runCtx == nil {
		runCtx = ctx
	}
	result := b.dispatcher.Enqueue(runCtx, evt.RoomID, func(jobCtx context.Context) {
		message := b.forgetConversation(jobCtx, evt.RoomID.String(), localpart)
		b.handleCommandNotice(jobCtx, evt, forgetCommand, func() string { return message })
	}, func() {
		b.handleCommandNotice(context.WithoutCancel(ctx), evt, forgetCommand, func() string {
			return "The conversation could not be forgotten because the bridge is stopping. Retry after it is ready."
		})
	})
	if result != enqueueAccepted {
		b.handleCommandNotice(ctx, evt, forgetCommand, func() string {
			return "The conversation forget request could not be queued. Retry after current room work finishes."
		})
	}
}

func (b *Bridge) mayModerate(ctx context.Context, sender id.UserID, roomID id.RoomID) bool {
	levels, err := b.as.StateStore.GetPowerLevels(ctx, roomID)
	if err != nil || levels == nil {
		return false
	}
	return levels.GetUserLevel(sender) >= b.cfg.CancelModeratorPowerLevel
}

func (b *Bridge) forgetConversation(ctx context.Context, roomID, ghost string) string {
	lock := b.conversationLock(roomID, ghost)
	lock.Lock()
	defer lock.Unlock()

	conversation, found, err := b.store.Conversation(ctx, roomID, ghost)
	if err != nil {
		b.log.Warn("load conversation for forget failed", "room", roomID, "ghost", ghost, "reason", "storage_error")
		return "The conversation could not be forgotten because durable state is unavailable. Nothing was reset."
	}
	if !found {
		return fmt.Sprintf("No stored conversation exists for @%s in this room. The next delegation will start fresh.", ghost)
	}
	busy, err := b.store.ConversationBusy(ctx, roomID, ghost)
	if err != nil {
		return "The conversation could not be forgotten because active-work state is unavailable. Nothing was reset."
	}
	if busy {
		return "This agent still has queued or active work in the room. Wait for it to finish or cancel it, then retry forget."
	}
	if !conversation.OwnersComplete {
		return "This conversation predates owner tracking, so the bridge cannot verify deletion for every backend identity. Nothing was reset; an operator must clean up the legacy kagent context first."
	}
	if b.sessionPurger == nil {
		return "The kagent session-deletion boundary is unavailable. Nothing was reset."
	}
	if err := b.sessionPurger.Purge(ctx, conversation.ContextID, conversation.Owners); err != nil {
		b.log.Warn(
			"verified kagent conversation deletion failed",
			"room", roomID, "ghost", ghost, "reason", "session_delete_failed", "error_type", fmt.Sprintf("%T", err),
		)
		return "kagent did not verify deletion of every observed session owner. The bridge kept the context and made no forget claim."
	}
	deleted, err := b.store.DeleteConversation(ctx, conversation)
	if err != nil {
		return "kagent deleted the old session, but the bridge could not reset durable context. Retry forget before delegating again."
	}
	if !deleted {
		return "The conversation changed while it was being forgotten. The old kagent session was deleted, but a newer context remains; retry to forget that context too."
	}
	b.log.Info("forgot kagent conversation", "room", roomID, "ghost", ghost)
	return fmt.Sprintf(
		"Forgot @%s's kagent conversation for this room. The next delegation starts with a new context. Matrix room history and federated copies were not deleted.",
		ghost,
	)
}

func (b *Bridge) runConversationRetention(ctx context.Context) {
	defer b.watchWG.Done()
	sweep := func() {
		now := time.Now().UTC()
		for _, entry := range b.agents.Entries() {
			maxAge := entry.Ref.MaxSessionAge()
			if maxAge <= 0 || entry.Ref.Target().IsRemote() {
				continue
			}
			conversations, err := b.store.ConversationsBefore(ctx, entry.Ghost, now.Add(-maxAge), conversationSweepBatch)
			if err != nil {
				b.log.Warn("conversation retention scan failed", "ghost", entry.Ghost, "reason", "storage_error")
				continue
			}
			for _, conversation := range conversations {
				b.expireConversation(ctx, conversation)
			}
		}
	}
	sweep()
	interval := b.cfg.ConversationSweepInterval
	if interval <= 0 {
		// Config validation makes this unreachable in production; keep direct Bridge unit fixtures
		// deterministic when they intentionally construct only the fields under test.
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func (b *Bridge) expireConversation(ctx context.Context, conversation state.Conversation) {
	lock := b.conversationLock(conversation.RoomID, conversation.Ghost)
	lock.Lock()
	defer lock.Unlock()
	current, found, err := b.store.Conversation(ctx, conversation.RoomID, conversation.Ghost)
	if err != nil || !found || current.ContextID != conversation.ContextID || !current.UpdatedAt.Equal(conversation.UpdatedAt) {
		return
	}
	busy, err := b.store.ConversationBusy(ctx, conversation.RoomID, conversation.Ghost)
	if err != nil || busy || b.sessionPurger == nil {
		return
	}
	if !conversation.OwnersComplete {
		b.log.Warn(
			"conversation retention skipped incomplete legacy owner set",
			"room", conversation.RoomID, "ghost", conversation.Ghost, "reason", "owners_incomplete",
		)
		return
	}
	if err := b.sessionPurger.Purge(ctx, conversation.ContextID, conversation.Owners); err != nil {
		b.log.Warn(
			"conversation retention deletion failed",
			"room", conversation.RoomID, "ghost", conversation.Ghost,
			"reason", "session_delete_failed", "error_type", fmt.Sprintf("%T", err),
		)
		return
	}
	deleted, err := b.store.DeleteConversation(ctx, conversation)
	if err != nil || !deleted {
		return
	}
	b.log.Info("expired kagent conversation", "room", conversation.RoomID, "ghost", conversation.Ghost)
}
