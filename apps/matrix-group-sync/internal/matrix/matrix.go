// Package matrix is the reconciler's Matrix room-management boundary. It exposes ONLY the normal
// client operations the scoped access-manager identity needs — resolve alias, read room state,
// check a local account exists, invite, kick, ban — and deliberately NO Synapse-admin or MAS
// `urn:mas:admin` capability (docs/adr/0009). Revocation withdraws a pending invite or kicks a
// joined member; both are the same normal `kick` endpoint, and the invite-only room prevents rejoin.
package matrix

import "context"

// Membership is the m.room.member state the reconciler reasons about.
type Membership string

const (
	// Join is an accepted, active member.
	Join Membership = "join"
	// Invite is a pending, not-yet-accepted invitation.
	Invite Membership = "invite"
	// Leave is a former or never-joined member (includes a kicked user).
	Leave Membership = "leave"
	// Ban is a banned member.
	Ban Membership = "ban"
)

// PowerLevels is the subset of m.room.power_levels the reconciler audits for drift. Invite/Kick/Ban
// are the thresholds required to perform each action; a human kept at level 0 cannot meet them.
type PowerLevels struct {
	Users        map[string]int
	UsersDefault int
	Invite       int
	Kick         int
	Ban          int
	StateDefault int
}

// RoomState is the managed room's current authorization-relevant state. Creator is the sender of the
// m.room.create event (the room-v12 creator), which must equal the access-manager for a managed room.
// AdditionalCreators are the room-v12 `additional_creators`, each of whom ALSO holds implicit
// privileged power even when absent from the power_levels users map; any creator other than the
// access-manager is power drift.
type RoomState struct {
	RoomID             string
	Version            string
	Creator            string
	AdditionalCreators []string
	Members            map[string]Membership
	Power              PowerLevels
}

// RoomManager is the normal-client Matrix surface the reconciler drives. Every method is a standard
// Client-Server API call available to an ordinary room-creating account.
type RoomManager interface {
	// ResolveAlias maps a room alias to its room ID.
	ResolveAlias(ctx context.Context, alias string) (roomID string, err error)
	// RoomState returns the current membership, power levels, creator, and version of a room.
	RoomState(ctx context.Context, roomID string) (RoomState, error)
	// AccountExists reports whether a local MXID is a real, provisioned account (profile lookup).
	AccountExists(ctx context.Context, mxid string) (bool, error)
	// Invite grants access by inviting a user into the invite-only room.
	Invite(ctx context.Context, roomID, mxid, reason string) error
	// Kick revokes access: it withdraws a pending invite or removes a joined member.
	Kick(ctx context.Context, roomID, mxid, reason string) error
	// Ban is the emergency-revocation escalation (kick + prevent rejoin at the membership layer).
	Ban(ctx context.Context, roomID, mxid, reason string) error
}
