// Package reconcile is the security core: it converges exact IdP-group membership into managed
// Matrix room membership through the scoped access-manager identity (docs/adr/0009). It is
// deliberately fail-closed in every ambiguous direction — a partial directory read, an ambiguous
// subject mapping, a missing/invalid localpart, a nonexistent Matrix account, an unmanaged room, an
// unexpected creator, or power-level drift all produce NO grant (and partial/ambiguous reads also
// produce NO bulk removal, retaining last-known Matrix state). Audit-only is the default: it
// computes and reports every diff without a single Matrix mutation.
package reconcile

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/fmind-ai/matrix-group-sync/internal/bindings"
	"github.com/fmind-ai/matrix-group-sync/internal/directory"
	"github.com/fmind-ai/matrix-group-sync/internal/matrix"
	"github.com/fmind-ai/matrix-group-sync/internal/metrics"
	"github.com/fmind-ai/matrix-group-sync/internal/mxid"
)

// Reconcile-cycle outcomes.
const (
	outcomeApplied   = "applied"
	outcomeAudit     = "audit"
	outcomePartial   = "partial"
	outcomeAmbiguous = "ambiguous"
)

// Grant decision outcomes.
const (
	grantInvited          = "invited"
	grantAudit            = "audit"
	grantNoAccount        = "skipped_no_account"
	grantInvalidLocalpart = "skipped_invalid_localpart"
	grantBlockedRoom      = "blocked_room"
	grantFailed           = "failed"
)

// Revocation decision outcomes.
const (
	revokeKicked = "kicked"
	revokeAudit  = "audit"
	revokeFailed = "failed"
)

// Room-guard fail-closed reasons.
const (
	guardUnresolved       = "unresolved"
	guardUnexpectedCreate = "unexpected_creator"
	guardRoomVersion      = "room_version"
	guardPowerDrift       = "power_drift"
	guardStateRead        = "state_read"
)

// roomVersion12 is the only managed-room version accepted; anything else is treated as unmanaged.
const roomVersion12 = "12"

// Options are the reconciler's immutable settings.
type Options struct {
	ServerName        string
	AccessManagerMXID string
	GhostPrefix       string
	// Enforce enables real invites/kicks and the revocation-SLO alert; false is audit-only.
	Enforce             bool
	RevocationSLO       time.Duration
	MissedIntervalAlert int
	// Now is injectable for deterministic tests; nil defaults to time.Now.
	Now func() time.Time
}

// Reconciler holds the bindings, IdP directory, Matrix room manager, and the small cross-cycle state
// (consecutive missed intervals and pending-revocation ages) that back the two alerts.
type Reconciler struct {
	bindings *bindings.Set
	dir      directory.Directory
	rooms    matrix.RoomManager
	metrics  *metrics.Metrics
	log      *slog.Logger
	opts     Options

	missed  int
	pending map[string]time.Time // roomID|mxid -> first seen unapplied (enforce mode only)
}

// RoomPlan is the computed intent for one managed room in a cycle (for logging and tests).
type RoomPlan struct {
	Group         string
	RoomID        string
	Grants        []string
	Revokes       []string
	GrantsBlocked bool
	Guards        []string
}

// Result summarizes a cycle for logging and tests. Applied is true only when a mutation was allowed
// (enforce mode) and the cycle was complete and unambiguous.
type Result struct {
	Complete    bool
	Ambiguous   bool
	Applied     bool
	Stalled     bool
	SLOBreached bool
	Plans       []RoomPlan
}

// New builds a reconciler. Now defaults to time.Now when unset.
func New(bs *bindings.Set, dir directory.Directory, rooms matrix.RoomManager, m *metrics.Metrics, log *slog.Logger, opts Options) *Reconciler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Reconciler{
		bindings: bs,
		dir:      dir,
		rooms:    rooms,
		metrics:  m,
		log:      log,
		opts:     opts,
		pending:  map[string]time.Time{},
	}
}

// Reconcile runs one full cycle. It never returns an error for an operational fault (partial read,
// unresolved room, kick failure): those are recorded as fail-closed outcomes and reflected in the
// Result and metrics so the loop keeps running and the alerts fire.
func (r *Reconciler) Reconcile(ctx context.Context) Result {
	snap, err := r.dir.Snapshot(ctx, r.bindings.Groups())
	if err != nil || !snap.Complete {
		// Partial read: retain last-known Matrix state, make no grants and no removals.
		r.log.Warn("directory snapshot incomplete — retaining last-known Matrix state", "error", err)
		return r.stall(outcomePartial, Result{Complete: false})
	}

	subToLP, lpToSub := map[string]string{}, map[string]string{}
	invalid := map[string]struct{}{}
	ambiguous := false
	for _, members := range snap.Groups {
		for _, mem := range members {
			if mem.Sub == "" {
				ambiguous = true // cannot key reconciliation without a stable subject
				continue
			}
			if err := mxid.ValidateLocalpart(mem.Localpart); err != nil {
				invalid[mem.Sub] = struct{}{}
				continue
			}
			if prev, ok := subToLP[mem.Sub]; ok && prev != mem.Localpart {
				ambiguous = true
			}
			if prev, ok := lpToSub[mem.Localpart]; ok && prev != mem.Sub {
				ambiguous = true
			}
			subToLP[mem.Sub] = mem.Localpart
			lpToSub[mem.Localpart] = mem.Sub
		}
	}
	if ambiguous {
		// A duplicate subject/localpart is a directory-integrity fault: fail closed for the whole
		// cycle (no grants and no bulk removals), exactly like a partial read.
		r.log.Warn("directory snapshot ambiguous (duplicate subject/localpart) — no mutation this cycle")
		return r.stall(outcomeAmbiguous, Result{Complete: true, Ambiguous: true})
	}

	// A complete, unambiguous read clears the stall state.
	r.missed = 0
	r.metrics.SetStalled(false)
	for range invalid {
		r.metrics.Grant(grantInvalidLocalpart)
	}

	result := Result{Complete: true, Applied: r.opts.Enforce}
	newPending := map[string]time.Time{}
	for _, b := range r.bindings.All() {
		plan := r.reconcileRoom(ctx, b, snap.Groups[b.Group], newPending)
		result.Plans = append(result.Plans, plan)
	}

	// Revocation-SLO alert (enforce mode only): a computed revoke unapplied past the SLO breaches it.
	now := r.opts.Now()
	breach := false
	for _, since := range newPending {
		if now.Sub(since) > r.opts.RevocationSLO {
			breach = true
		}
	}
	r.pending = newPending
	result.SLOBreached = breach && r.opts.Enforce
	r.metrics.SetSLOBreach(result.SLOBreached)

	if r.opts.Enforce {
		r.metrics.Reconcile(outcomeApplied)
	} else {
		r.metrics.Reconcile(outcomeAudit)
	}
	return result
}

// stall records an incomplete/ambiguous cycle: it advances the missed counter, raises the stall
// alert after the threshold, and performs no mutation.
func (r *Reconciler) stall(outcome string, res Result) Result {
	r.missed++
	r.metrics.Reconcile(outcome)
	if r.missed >= r.opts.MissedIntervalAlert {
		res.Stalled = true
		r.metrics.SetStalled(true)
	}
	// Retain pending-revocation ages across a stall; do not clear them.
	return res
}

func (r *Reconciler) reconcileRoom(ctx context.Context, b bindings.Binding, members []directory.Member, newPending map[string]time.Time) RoomPlan {
	plan := RoomPlan{Group: b.Group}

	roomID, err := r.rooms.ResolveAlias(ctx, b.RoomAlias)
	if err != nil {
		r.log.Warn("resolve room alias failed — room skipped", "group", b.Group, "alias", b.RoomAlias, "error", err)
		r.metrics.GuardFailure(guardUnresolved)
		plan.Guards = append(plan.Guards, guardUnresolved)
		return plan
	}
	plan.RoomID = roomID

	state, err := r.rooms.RoomState(ctx, roomID)
	if err != nil {
		r.log.Warn("read room state failed — room skipped", "group", b.Group, "room", roomID, "error", err)
		r.metrics.GuardFailure(guardStateRead)
		plan.Guards = append(plan.Guards, guardStateRead)
		return plan
	}

	// Room guards: a managed room must be created and solely powered by the access-manager and be
	// room v12. Any violation blocks GRANTS (fail closed) but still permits revocation, since
	// removing access is always the safe direction.
	if state.Creator != r.opts.AccessManagerMXID {
		r.metrics.GuardFailure(guardUnexpectedCreate)
		plan.Guards = append(plan.Guards, guardUnexpectedCreate)
		plan.GrantsBlocked = true
	}
	if state.Version != roomVersion12 {
		r.metrics.GuardFailure(guardRoomVersion)
		plan.Guards = append(plan.Guards, guardRoomVersion)
		plan.GrantsBlocked = true
	}
	if r.powerDrift(state) {
		r.metrics.GuardFailure(guardPowerDrift)
		plan.Guards = append(plan.Guards, guardPowerDrift)
		plan.GrantsBlocked = true
	}

	desired := r.desiredMXIDs(members)
	actual := r.managedActual(state)

	grants := setDiff(desired, actual)  // desired but not currently join/invite
	revokes := setDiff(actual, desired) // currently join/invite but no longer desired
	plan.Grants = grants
	plan.Revokes = revokes

	for _, target := range grants {
		r.applyGrant(ctx, roomID, b.Group, target, plan.GrantsBlocked)
	}
	for _, target := range revokes {
		r.applyRevoke(ctx, roomID, b.Group, target, newPending)
	}
	return plan
}

// desiredMXIDs forms the sorted set of full local MXIDs for a group's members with a valid
// localpart. Every MXID is local by construction, so a local IdP group can only ever grant a local
// user (federation-safe).
func (r *Reconciler) desiredMXIDs(members []directory.Member) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(members))
	for _, mem := range members {
		id, err := mxid.Format(mem.Localpart, r.opts.ServerName)
		if err != nil {
			continue // invalid localpart already counted globally; never guess an identity
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// managedActual is the sorted set of currently join/invite members this reconciler is allowed to
// manage: LOCAL, non-ghost, non-access-manager MXIDs. Remote (partner) members are never included,
// so the reconciler never evicts a federated user based on a local IdP group.
func (r *Reconciler) managedActual(state matrix.RoomState) []string {
	out := make([]string, 0, len(state.Members))
	for member, membership := range state.Members {
		if membership != matrix.Join && membership != matrix.Invite {
			continue
		}
		if !mxid.IsLocal(member, r.opts.ServerName) {
			continue // federation-safe: partner users are managed by explicit Matrix membership
		}
		if member == r.opts.AccessManagerMXID {
			continue
		}
		if mxid.IsLocalGhost(member, r.opts.GhostPrefix, r.opts.ServerName) {
			continue // agent ghosts are placed by the bridge, not by IdP groups
		}
		out = append(out, member)
	}
	sort.Strings(out)
	return out
}

func (r *Reconciler) applyGrant(ctx context.Context, roomID, group, target string, blocked bool) {
	if blocked {
		r.log.Warn("grant withheld — room failed a fail-closed guard", "group", group, "room", roomID, "mxid", target)
		r.metrics.Grant(grantBlockedRoom)
		return
	}
	exists, err := r.rooms.AccountExists(ctx, target)
	if err != nil {
		// A lookup failure is fail-closed: never invite an identity we could not confirm.
		r.log.Warn("account existence check failed — grant withheld", "mxid", target, "error", err)
		r.metrics.Grant(grantNoAccount)
		return
	}
	if !exists {
		r.log.Warn("target Matrix account does not exist — grant withheld", "mxid", target)
		r.metrics.Grant(grantNoAccount)
		return
	}
	if !r.opts.Enforce {
		r.log.Info("audit: would invite", "group", group, "room", roomID, "mxid", target)
		r.metrics.Grant(grantAudit)
		return
	}
	if err := r.rooms.Invite(ctx, roomID, target, "granted by IdP group "+group); err != nil {
		r.log.Error("invite failed", "group", group, "room", roomID, "mxid", target, "error", err)
		r.metrics.Grant(grantFailed)
		return
	}
	r.log.Info("invited", "group", group, "room", roomID, "mxid", target)
	r.metrics.Grant(grantInvited)
}

func (r *Reconciler) applyRevoke(ctx context.Context, roomID, group, target string, newPending map[string]time.Time) {
	if !r.opts.Enforce {
		r.log.Info("audit: would revoke", "group", group, "room", roomID, "mxid", target)
		r.metrics.Revocation(revokeAudit)
		return
	}
	key := roomID + "|" + target
	since, ok := r.pending[key]
	if !ok {
		since = r.opts.Now()
	}
	if err := r.rooms.Kick(ctx, roomID, target, "access revoked: no longer a member of IdP group "+group); err != nil {
		// Keep it pending so the revocation-SLO alert fires if this persists past the SLO.
		r.log.Error("kick failed — revocation still pending", "group", group, "room", roomID, "mxid", target, "error", err)
		r.metrics.Revocation(revokeFailed)
		newPending[key] = since
		return
	}
	r.log.Info("revoked", "group", group, "room", roomID, "mxid", target)
	r.metrics.Revocation(revokeKicked)
}

// powerDrift reports whether a managed room's power levels diverge from the required posture: the
// access-manager must outrank every action threshold, the invite/kick/ban thresholds must be above
// 0 so a level-0 human cannot perform them, users_default must be 0, and no non-ghost, non-manager
// user may hold power above 0 (docs/adr/0009).
func (r *Reconciler) powerDrift(state matrix.RoomState) bool {
	pl := state.Power
	amLevel := pl.UsersDefault
	if lvl, ok := pl.Users[r.opts.AccessManagerMXID]; ok {
		amLevel = lvl
	}
	if amLevel < pl.Invite || amLevel < pl.Kick || amLevel < pl.Ban || amLevel < pl.StateDefault {
		return true
	}
	if pl.Invite <= 0 || pl.Kick <= 0 || pl.Ban <= 0 {
		return true
	}
	if pl.UsersDefault != 0 {
		return true
	}
	for user, level := range pl.Users {
		if user == r.opts.AccessManagerMXID {
			continue
		}
		if mxid.IsLocalGhost(user, r.opts.GhostPrefix, r.opts.ServerName) {
			continue
		}
		if level > 0 {
			return true
		}
	}
	return false
}

// setDiff returns the sorted elements of a not present in b.
func setDiff(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, v := range b {
		inB[v] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if _, ok := inB[v]; !ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
