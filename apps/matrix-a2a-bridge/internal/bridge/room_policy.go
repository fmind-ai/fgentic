package bridge

import (
	"context"
	"errors"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	errorRoomBinding            = "room_binding_rejected"
	errorRoomBindingUnavailable = "room_binding_unavailable"
	errorGhostMembership        = "ghost_membership_required"
	errorGhostMembershipStore   = "ghost_membership_unavailable"

	managedInviteAuditSchema = "fgentic.managed_room_invite.v1"
	managedInviteAuditStream = "managed_room_invite"
)

// roomAdmission verifies the two independent managed-room facts required before a delegation:
// this exact room is bound to the target, and the target ghost is already joined. It never mutates
// Matrix state, so an event cannot create its own authorization by causing an ambient join.
func (b *Bridge) roomAdmission(ctx context.Context, ref *AgentRef, localpart string, roomID id.RoomID) string {
	bound, reason := b.roomBound(ctx, ref, roomID)
	if !bound {
		return reason
	}
	if b.as == nil || b.as.StateStore == nil {
		return errorGhostMembershipStore
	}
	member, err := b.as.StateStore.GetMember(
		ctx,
		roomID,
		id.NewUserID(localpart, b.cfg.ServerName),
	)
	if err != nil {
		return errorGhostMembershipStore
	}
	if member == nil || member.Membership != event.MembershipJoin {
		return errorGhostMembership
	}
	return ""
}

// roomBound compares an event room with immutable room IDs first. Exact local aliases are a
// bootstrap convenience for rooms whose IDs are created at runtime; the bridge resolves them on
// every decision rather than caching a stale authorization after an alias is repointed.
func (b *Bridge) roomBound(ctx context.Context, ref *AgentRef, roomID id.RoomID) (bool, string) {
	if ref == nil {
		return false, errorRoomBinding
	}
	if _, ok := ref.allowedRoomIDs[roomID]; ok {
		return true, ""
	}
	if len(ref.allowedAliases) == 0 {
		return false, errorRoomBinding
	}
	if b.as == nil {
		return false, errorRoomBindingUnavailable
	}
	intent := b.as.BotIntent()
	if intent == nil || intent.Client == nil {
		return false, errorRoomBindingUnavailable
	}
	lookupFailed := false
	for _, alias := range ref.allowedAliases {
		if roomReferenceServer(alias.String()) != b.cfg.ServerName {
			continue
		}
		resolved, err := intent.ResolveAlias(ctx, alias)
		if errors.Is(err, mautrix.MNotFound) {
			continue
		}
		if err != nil || resolved == nil {
			lookupFailed = true
			continue
		}
		if resolved.RoomID == "" || resolved.RoomID[0] != '!' || validateMatrixRoomReference(resolved.RoomID.String()) != nil {
			lookupFailed = true
			continue
		}
		if resolved.RoomID == roomID {
			return true, ""
		}
	}
	if lookupFailed {
		return false, errorRoomBindingUnavailable
	}
	return false, errorRoomBinding
}

func roomReferenceServer(room string) string {
	separator := strings.IndexByte(room, ':')
	if separator < 0 || separator == len(room)-1 {
		return ""
	}
	return room[separator+1:]
}

func (b *Bridge) logManagedInvite(evt *event.Event, target id.UserID, outcome, reason string) {
	if b.auditLog == nil {
		return
	}
	b.auditLog.Info(
		"managed room ghost invitation decision",
		"audit_schema", managedInviteAuditSchema,
		"audit_stream", managedInviteAuditStream,
		"event_id", evt.ID,
		"room_id", evt.RoomID,
		"inviter_mxid", evt.Sender,
		"ghost_mxid", target,
		"outcome", outcome,
		"reason", reason,
	)
}
