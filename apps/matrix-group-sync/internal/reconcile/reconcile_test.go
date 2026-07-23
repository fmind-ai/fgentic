package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fmind-ai/matrix-group-sync/internal/bindings"
	"github.com/fmind-ai/matrix-group-sync/internal/directory"
	"github.com/fmind-ai/matrix-group-sync/internal/matrix"
	"github.com/fmind-ai/matrix-group-sync/internal/metrics"
)

const (
	serverName = "fgentic.localhost"
	accessMgr  = "@access-manager:fgentic.localhost"
	group      = "/fgentic/agent-access/platform"
	roomAlias  = "#agent-platform:fgentic.localhost"
	roomID     = "!platform:fgentic.localhost"
)

// --- fakes ---

type fakeDir struct {
	snap directory.Snapshot
	err  error
}

func (f *fakeDir) Snapshot(context.Context, []string) (directory.Snapshot, error) {
	return f.snap, f.err
}

type call struct{ room, mxid string }

type fakeRooms struct {
	aliases    map[string]string
	states     map[string]matrix.RoomState
	accounts   map[string]bool // nil => every account exists
	resolveErr map[string]error
	stateErr   map[string]error
	kickErr    map[string]error

	invited []call
	kicked  []call
	banned  []call
}

func (f *fakeRooms) ResolveAlias(_ context.Context, alias string) (string, error) {
	if err := f.resolveErr[alias]; err != nil {
		return "", err
	}
	id, ok := f.aliases[alias]
	if !ok {
		return "", errors.New("alias not found")
	}
	return id, nil
}

func (f *fakeRooms) RoomState(_ context.Context, id string) (matrix.RoomState, error) {
	if err := f.stateErr[id]; err != nil {
		return matrix.RoomState{}, err
	}
	return f.states[id], nil
}

func (f *fakeRooms) AccountExists(_ context.Context, mxid string) (bool, error) {
	if f.accounts == nil {
		return true, nil
	}
	return f.accounts[mxid], nil
}

func (f *fakeRooms) Invite(_ context.Context, room, mxid, _ string) error {
	f.invited = append(f.invited, call{room, mxid})
	return nil
}

func (f *fakeRooms) Kick(_ context.Context, room, mxid, _ string) error {
	if err := f.kickErr[mxid]; err != nil {
		return err
	}
	f.kicked = append(f.kicked, call{room, mxid})
	return nil
}

func (f *fakeRooms) Ban(_ context.Context, room, mxid, _ string) error {
	f.banned = append(f.banned, call{room, mxid})
	return nil
}

// --- helpers ---

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func managedRoom(members map[string]matrix.Membership) matrix.RoomState {
	return matrix.RoomState{
		RoomID:  roomID,
		Version: roomVersion12,
		Creator: accessMgr,
		Members: members,
		Power: matrix.PowerLevels{
			Users:        map[string]int{accessMgr: 100},
			UsersDefault: 0,
			Invite:       50,
			Kick:         50,
			Ban:          50,
			StateDefault: 50,
		},
	}
}

func singleBinding(t *testing.T) *bindings.Set {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/bindings.yaml"
	content := "schemaVersion: 1\nbindings:\n  - group: " + group +
		"\n    roomAlias: \"" + roomAlias + "\"\n    agents: [agent-k8s]\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	set, err := bindings.Load(path, "agent-")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func newReconciler(t *testing.T, dir directory.Directory, rooms matrix.RoomManager, enforce bool, now func() time.Time) *Reconciler {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry())
	return New(singleBinding(t), dir, rooms, m, discardLog(), Options{
		ServerName:          serverName,
		AccessManagerMXID:   accessMgr,
		GhostPrefix:         "agent-",
		Enforce:             enforce,
		RevocationSLO:       2 * time.Minute,
		MissedIntervalAlert: 2,
		Now:                 now,
	})
}

func snapshot(members ...directory.Member) directory.Snapshot {
	return directory.Snapshot{Groups: map[string][]directory.Member{group: members}, Complete: true}
}

func mxidOf(localpart string) string { return "@" + localpart + ":" + serverName }

// --- tests ---

func TestAcceptedInviteNoAction(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("alice"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if len(rooms.invited) != 0 || len(rooms.kicked) != 0 {
		t.Fatalf("desired member already joined must produce no mutation: invited=%v kicked=%v", rooms.invited, rooms.kicked)
	}
	if !res.Complete || res.Ambiguous {
		t.Fatalf("expected complete unambiguous cycle: %+v", res)
	}
}

func TestGrantInvitesMissingMember(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(nil)},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.invited) != 1 || rooms.invited[0].mxid != mxidOf("alice") {
		t.Fatalf("expected one invite for alice, got %v", rooms.invited)
	}
}

func TestAuditOnlyNoMutation(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		// bob is joined but not desired (revoke), alice desired but absent (grant).
		states: map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("bob"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, false, nil)
	res := r.Reconcile(context.Background())
	if len(rooms.invited) != 0 || len(rooms.kicked) != 0 {
		t.Fatalf("audit-only must make no mutation: invited=%v kicked=%v", rooms.invited, rooms.kicked)
	}
	if res.Applied {
		t.Fatalf("audit-only cycle must not report Applied")
	}
	// The diff must still be computed.
	if len(res.Plans) != 1 || len(res.Plans[0].Grants) != 1 || len(res.Plans[0].Revokes) != 1 {
		t.Fatalf("audit must still compute the diff: %+v", res.Plans)
	}
}

func TestRevokeJoinedAndPendingInvite(t *testing.T) {
	dir := &fakeDir{snap: snapshot()} // empty desired set
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states: map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{
			mxidOf("joined"):  matrix.Join,
			mxidOf("pending"): matrix.Invite,
		})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.kicked) != 2 {
		t.Fatalf("expected both a joined member and a pending invite revoked, got %v", rooms.kicked)
	}
}

func TestRenamedOrDeletedGroupRevokes(t *testing.T) {
	// A renamed/deleted group is a successful empty result → the whole managed set is revoked.
	dir := &fakeDir{snap: directory.Snapshot{Groups: map[string][]directory.Member{group: nil}, Complete: true}}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("alice"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.kicked) != 1 || rooms.kicked[0].mxid != mxidOf("alice") {
		t.Fatalf("empty group must revoke its members, got %v", rooms.kicked)
	}
}

func TestPartialDirectoryNoMutationAndStalls(t *testing.T) {
	dir := &fakeDir{snap: directory.Snapshot{Complete: false}}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("stale"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res1 := r.Reconcile(context.Background())
	if res1.Complete || res1.Stalled {
		t.Fatalf("first incomplete cycle: complete=false, not yet stalled: %+v", res1)
	}
	res2 := r.Reconcile(context.Background())
	if !res2.Stalled {
		t.Fatalf("two missed intervals must raise the stall alert")
	}
	if len(rooms.invited) != 0 || len(rooms.kicked) != 0 {
		t.Fatalf("partial read must create no grants and no removals (last-known retained): %v %v", rooms.invited, rooms.kicked)
	}
}

func TestIdpOutageContinuityRetainsMembers(t *testing.T) {
	dir := &fakeDir{err: errors.New("keycloak unreachable")}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("alice"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if res.Complete {
		t.Fatalf("directory error must yield an incomplete cycle")
	}
	if len(rooms.kicked) != 0 {
		t.Fatalf("an IdP outage must never evict last-known members, got %v", rooms.kicked)
	}
}

func TestDuplicateLocalpartFailsClosed(t *testing.T) {
	// Two distinct subjects claiming the same localpart is a directory-integrity fault.
	dir := &fakeDir{snap: snapshot(
		directory.Member{Sub: "s1", Localpart: "alice"},
		directory.Member{Sub: "s2", Localpart: "alice"},
	)}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("bob"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if !res.Ambiguous {
		t.Fatalf("duplicate localpart must mark the cycle ambiguous")
	}
	if len(rooms.invited) != 0 || len(rooms.kicked) != 0 {
		t.Fatalf("ambiguous snapshot must create no grants and no removals: %v %v", rooms.invited, rooms.kicked)
	}
}

func TestDuplicateSubjectFailsClosed(t *testing.T) {
	// One subject mapping to two different localparts is likewise ambiguous.
	dir := &fakeDir{snap: snapshot(
		directory.Member{Sub: "s1", Localpart: "alice"},
		directory.Member{Sub: "s1", Localpart: "alice2"},
	)}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(nil)},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if !res.Ambiguous || len(rooms.invited) != 0 {
		t.Fatalf("duplicate subject must fail closed: ambiguous=%v invited=%v", res.Ambiguous, rooms.invited)
	}
}

func TestMissingMatrixAccountNoGrant(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "ghostuser"})}
	rooms := &fakeRooms{
		aliases:  map[string]string{roomAlias: roomID},
		states:   map[string]matrix.RoomState{roomID: managedRoom(nil)},
		accounts: map[string]bool{}, // ghostuser is absent → does not exist
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.invited) != 0 {
		t.Fatalf("a nonexistent Matrix account must not be invited, got %v", rooms.invited)
	}
}

func TestInvalidLocalpartSkippedButCycleProceeds(t *testing.T) {
	dir := &fakeDir{snap: snapshot(
		directory.Member{Sub: "s1", Localpart: "Bad Localpart!"}, // invalid
		directory.Member{Sub: "s2", Localpart: "alice"},          // valid
	)}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(nil)},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if res.Ambiguous {
		t.Fatalf("an invalid localpart is a per-member skip, not a whole-cycle ambiguity")
	}
	if len(rooms.invited) != 1 || rooms.invited[0].mxid != mxidOf("alice") {
		t.Fatalf("only the valid member must be granted, got %v", rooms.invited)
	}
}

func TestFederationSafetyRemoteMemberNeverRevoked(t *testing.T) {
	// A partner member is present and joined; the desired set is empty. The reconciler must never
	// evict a remote user based on a local IdP group.
	dir := &fakeDir{snap: snapshot()}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states: map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{
			"@partner:other.example": matrix.Join,
			mxidOf("local"):          matrix.Join,
		})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.kicked) != 1 || rooms.kicked[0].mxid != mxidOf("local") {
		t.Fatalf("only the local member may be revoked; the partner must be untouched, got %v", rooms.kicked)
	}
}

func TestGhostAndAccessManagerNeverRevoked(t *testing.T) {
	dir := &fakeDir{snap: snapshot()}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states: map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{
			accessMgr:              matrix.Join,
			mxidOf("agent-k8s"):    matrix.Join,
			mxidOf("someone-else"): matrix.Join,
		})},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.kicked) != 1 || rooms.kicked[0].mxid != mxidOf("someone-else") {
		t.Fatalf("ghosts and the access-manager must never be revoked, got %v", rooms.kicked)
	}
}

func TestUnexpectedCreatorBlocksGrants(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	state := managedRoom(nil)
	state.Creator = "@someone-else:fgentic.localhost"
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: state},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if len(rooms.invited) != 0 {
		t.Fatalf("an unexpected creator must block grants, got %v", rooms.invited)
	}
	if !res.Plans[0].GrantsBlocked {
		t.Fatalf("expected grants blocked for the room")
	}
}

func TestRoomVersionDriftBlocksGrants(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	state := managedRoom(nil)
	state.Version = "11"
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: state},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	r.Reconcile(context.Background())
	if len(rooms.invited) != 0 {
		t.Fatalf("a non-v12 room is unmanaged and must block grants, got %v", rooms.invited)
	}
}

func TestPowerLevelDriftBlocksGrants(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	state := managedRoom(nil)
	state.Power.Users["@human:fgentic.localhost"] = 50 // a human holding power is drift
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: state},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if len(rooms.invited) != 0 {
		t.Fatalf("power-level drift must fail closed for grants, got %v", rooms.invited)
	}
	if !res.Plans[0].GrantsBlocked {
		t.Fatalf("expected grants blocked on power drift")
	}
}

func TestPowerDriftDetectsWeakAccessManager(t *testing.T) {
	state := managedRoom(nil)
	// A NON-creator access-manager relies on an EXPLICIT power level; drop it below the thresholds.
	state.Creator = "@other:fgentic.localhost"
	state.Power.Users[accessMgr] = 40 // below the 50 thresholds → cannot enforce
	r := newReconciler(t, &fakeDir{snap: snapshot()}, &fakeRooms{}, true, nil)
	if !r.powerDrift(state) {
		t.Fatalf("a non-creator access-manager below the action thresholds must count as drift")
	}
}

func TestCreatorImplicitPowerNotDrift(t *testing.T) {
	// A pristine room v12: the access-manager is the creator and is OMITTED from power_levels.users,
	// holding implicit privileged power. This must NOT be read as drift, so grants proceed.
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	state := managedRoom(nil)            // Creator is accessMgr
	delete(state.Power.Users, accessMgr) // absent from the users map, as a real v12 room may leave it
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: state},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if res.Plans[0].GrantsBlocked {
		t.Fatalf("a room-v12 creator with implicit power must not be read as drift")
	}
	if len(rooms.invited) != 1 || rooms.invited[0].mxid != mxidOf("alice") {
		t.Fatalf("grants must proceed for a creator-owned room, got %v", rooms.invited)
	}
}

func TestAdditionalCreatorIsDrift(t *testing.T) {
	// A non-access-manager additional creator holds implicit privileged power → drift, blocks grants.
	state := managedRoom(nil)
	state.AdditionalCreators = []string{"@intruder:fgentic.localhost"}
	r := newReconciler(t, &fakeDir{snap: snapshot()}, &fakeRooms{}, true, nil)
	if !r.powerDrift(state) {
		t.Fatalf("an additional creator other than the access-manager must count as drift")
	}
}

func TestGhostLocalpartNeverGranted(t *testing.T) {
	// An IdP member whose matrix_localpart lands in the reserved ghost namespace must not be invited.
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "agent-x"})}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(nil)},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if len(rooms.invited) != 0 {
		t.Fatalf("a ghost-namespace localpart must never be granted, got %v", rooms.invited)
	}
	if len(res.Plans[0].Grants) != 0 {
		t.Fatalf("a ghost-namespace localpart must not appear in the desired grant set, got %v", res.Plans[0].Grants)
	}
}

func TestUnresolvedRoomSkipped(t *testing.T) {
	dir := &fakeDir{snap: snapshot(directory.Member{Sub: "s1", Localpart: "alice"})}
	rooms := &fakeRooms{
		aliases:    map[string]string{},
		resolveErr: map[string]error{roomAlias: errors.New("unknown alias")},
	}
	r := newReconciler(t, dir, rooms, true, nil)
	res := r.Reconcile(context.Background())
	if len(rooms.invited) != 0 || len(rooms.kicked) != 0 {
		t.Fatalf("an unresolved room must produce no mutation")
	}
	if len(res.Plans[0].Guards) == 0 || res.Plans[0].Guards[0] != guardUnresolved {
		t.Fatalf("expected an unresolved guard, got %+v", res.Plans[0].Guards)
	}
}

func TestRevocationSLOAlertFiresWhenKickFails(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	dir := &fakeDir{snap: snapshot()} // empty desired → revoke the joined member
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("stuck"): matrix.Join})},
		kickErr: map[string]error{mxidOf("stuck"): errors.New("homeserver 500")},
	}
	r := newReconciler(t, dir, rooms, true, clock)

	res1 := r.Reconcile(context.Background())
	if res1.SLOBreached {
		t.Fatalf("no breach on the first failed revoke (age 0)")
	}
	now = now.Add(3 * time.Minute) // exceed the 2-minute SLO
	res2 := r.Reconcile(context.Background())
	if !res2.SLOBreached {
		t.Fatalf("a revoke unapplied past the SLO must raise the revocation-SLO alert")
	}
}

func TestAuditModeRaisesNoSLOAlert(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	dir := &fakeDir{snap: snapshot()}
	rooms := &fakeRooms{
		aliases: map[string]string{roomAlias: roomID},
		states:  map[string]matrix.RoomState{roomID: managedRoom(map[string]matrix.Membership{mxidOf("stuck"): matrix.Join})},
	}
	r := newReconciler(t, dir, rooms, false, clock)
	r.Reconcile(context.Background())
	now = now.Add(10 * time.Minute)
	res := r.Reconcile(context.Background())
	if res.SLOBreached {
		t.Fatalf("audit-only must never raise the revocation-SLO alert")
	}
}
