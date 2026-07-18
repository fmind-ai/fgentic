package state

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var ledgerEpoch = time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)

func testDelegation(eventID, ghost, roomID string) NewDelegation {
	return NewDelegation{
		MatrixEventID:       eventID,
		GhostMXID:           "@" + ghost + ":example.test",
		GhostLocalpart:      ghost,
		RoomID:              roomID,
		SenderMXID:          "@alice:example.test",
		SenderOriginKind:    "local",
		SenderOriginNetwork: "example.test",
		OriginServerTS:      ledgerEpoch.UnixMilli(),
		TargetFingerprint:   "sha256:agent-card-v1",
		Prompt:              "minimum recoverable prompt",
		Payload:             []byte("minimum recoverable payload"),
	}
}

func testAdmission(transactionID string, at time.Time, delegations ...NewDelegation) TransactionAdmission {
	return TransactionAdmission{
		TransactionID:  transactionID,
		BodyHash:       HashTransaction([]byte("exact body for " + transactionID)),
		CommittedAt:    at,
		RoomCapacity:   32,
		GlobalCapacity: 256,
		Delegations:    delegations,
	}
}

func mustAdmit(t *testing.T, store *Memory, admission TransactionAdmission) AdmissionResult {
	t.Helper()
	result, err := store.AdmitTransaction(t.Context(), admission)
	if err != nil {
		t.Fatalf("AdmitTransaction(%q): %v", admission.TransactionID, err)
	}
	return result
}

func mustClaim(t *testing.T, store *Memory, owner string, at time.Time) Job {
	t.Helper()
	job, ok, err := store.Claim(t.Context(), ClaimRequest{
		Owner:         owner,
		Now:           at,
		LeaseDuration: time.Hour,
	})
	if err != nil {
		t.Fatalf("Claim(%q): %v", owner, err)
	}
	if !ok {
		t.Fatalf("Claim(%q) returned no job", owner)
	}
	return job
}

func mustTransition(
	t *testing.T,
	store *Memory,
	lease LeaseToken,
	from, to DelegationState,
	at time.Time,
	patch TransitionPatch,
) {
	t.Helper()
	if err := store.Transition(t.Context(), TransitionRequest{
		Lease: lease,
		From:  from,
		To:    to,
		At:    at,
		Patch: patch,
	}); err != nil {
		t.Fatalf("Transition(%s -> %s): %v", from, to, err)
	}
}

func memoryJobAtState(t *testing.T, state DelegationState) (*Memory, Job, time.Time) {
	t.Helper()
	store := NewMemory()
	mustAdmit(t, store, testAdmission("txn-setup", ledgerEpoch, testDelegation("$setup", "agent", "!room")))
	job := mustClaim(t, store, "worker", ledgerEpoch)
	at := ledgerEpoch
	switch state {
	case StatePending:
	case StateA2APrepared:
		at = at.Add(time.Second)
		mustTransition(t, store, job.LeaseToken(), StatePending, StateA2APrepared, at, TransitionPatch{})
	case StateAwaitingTask:
		at = at.Add(time.Second)
		mustTransition(t, store, job.LeaseToken(), StatePending, StateA2APrepared, at, TransitionPatch{})
		at = at.Add(time.Second)
		mustTransition(t, store, job.LeaseToken(), StateA2APrepared, StateAwaitingTask, at, TransitionPatch{})
	case StateReplyPending:
		at = at.Add(time.Second)
		mustTransition(t, store, job.LeaseToken(), StatePending, StateReplyPending, at, TransitionPatch{})
	default:
		t.Fatalf("memoryJobAtState does not accept terminal state %q", state)
	}
	stored, ok, err := store.Job(t.Context(), job.JobID)
	if err != nil || !ok {
		t.Fatalf("Job(%q) = (%t, %v)", job.JobID, ok, err)
	}
	return store, stored, at
}

func TestMemoryExecutesEveryLegalTransition(t *testing.T) {
	edges := [][2]DelegationState{
		{StatePending, StateA2APrepared},
		{StatePending, StateReplyPending},
		{StatePending, StateDenied},
		{StatePending, StateDead},
		{StateA2APrepared, StateAwaitingTask},
		{StateA2APrepared, StateReplyPending},
		{StateA2APrepared, StateAmbiguous},
		{StateA2APrepared, StateDead},
		{StateAwaitingTask, StateReplyPending},
		{StateAwaitingTask, StateDenied},
		{StateAwaitingTask, StateDead},
		{StateReplyPending, StateDelivered},
		{StateReplyPending, StateDenied},
		{StateReplyPending, StateAmbiguous},
		{StateReplyPending, StateDead},
	}
	for _, edge := range edges {
		edge := edge
		t.Run(fmt.Sprintf("%s_to_%s", edge[0], edge[1]), func(t *testing.T) {
			store, job, at := memoryJobAtState(t, edge[0])
			mustTransition(t, store, job.LeaseToken(), edge[0], edge[1], at.Add(time.Second), TransitionPatch{})
			got, ok, err := store.Job(t.Context(), job.JobID)
			if err != nil || !ok {
				t.Fatalf("Job() = (%t, %v)", ok, err)
			}
			if got.State != edge[1] {
				t.Fatalf("state = %s, want %s", got.State, edge[1])
			}
			if edge[1].Terminal() {
				if got.Prompt != "" || len(got.Payload) != 0 || got.ResultText != "" {
					t.Fatal("terminal transition retained content")
				}
				if got.TerminalAt.IsZero() || got.LeaseToken() != (LeaseToken{}) {
					t.Fatal("terminal transition did not timestamp and release the lease")
				}
			} else if got.TerminalAt.IsZero() == false {
				t.Fatal("non-terminal transition set terminal timestamp")
			}
		})
	}
}

func TestMemoryDurableControlsClaimTakeoverAndFence(t *testing.T) {
	store := NewMemory()
	delegation := testDelegation("$control-job", "agent", "!room")
	mustAdmit(t, store, testAdmission("txn-control-job", ledgerEpoch, delegation))
	job := mustClaim(t, store, "worker-one", ledgerEpoch)
	mustTransition(
		t, store, job.LeaseToken(), StatePending, StateA2APrepared,
		ledgerEpoch.Add(time.Second), TransitionPatch{},
	)
	job, _, _ = store.Job(t.Context(), job.JobID)
	mustTransition(
		t, store, job.LeaseToken(), StateA2APrepared, StateAwaitingTask,
		ledgerEpoch.Add(2*time.Second), TransitionPatch{},
	)
	job, _, _ = store.Job(t.Context(), job.JobID)
	if err := store.RecordMatrixEvent(t.Context(), MatrixEventRequest{
		Lease: job.LeaseToken(), At: ledgerEpoch.Add(3 * time.Second),
		Stage: MatrixEventPlaceholder, EventID: "$placeholder",
	}); err != nil {
		t.Fatalf("record placeholder: %v", err)
	}

	controlAdmission := testAdmission("txn-cancel", ledgerEpoch.Add(4*time.Second))
	controlAdmission.ControlCapacity = 8
	controlAdmission.Controls = []NewControl{{
		TargetMatrixEventID: "$placeholder", SourceMatrixEventID: "$cancel-one",
		RoomID: "!room", SenderMXID: "@alice:example.test", Kind: ControlCancel,
		Authorized: true,
	}}
	result := mustAdmit(t, store, controlAdmission)
	if len(result.InsertedControlIDs) != 1 || len(result.ExistingControlIDs) != 0 {
		t.Fatalf("control admission = %+v", result)
	}
	control, ok, err := store.ClaimControl(t.Context(), job.LeaseToken(), ledgerEpoch.Add(5*time.Second))
	if err != nil || !ok || control.State != ControlPrepared ||
		control.LeaseGeneration != job.LeaseGeneration {
		t.Fatalf("ClaimControl = (%+v, %t, %v)", control, ok, err)
	}

	secondAdmission := testAdmission("txn-cancel-two", ledgerEpoch.Add(6*time.Second))
	secondAdmission.ControlCapacity = 8
	secondAdmission.Controls = []NewControl{{
		TargetMatrixEventID: "$placeholder", SourceMatrixEventID: "$cancel-two",
		RoomID: "!room", SenderMXID: "@alice:example.test", Kind: ControlContinuation,
		Authorized: true,
	}}
	mustAdmit(t, store, secondAdmission)

	takeoverAt := job.LeaseExpiresAt.Add(time.Second)
	takeover := mustClaim(t, store, "worker-two", takeoverAt)
	claimed, ok, err := store.ClaimControl(t.Context(), takeover.LeaseToken(), takeoverAt.Add(time.Second))
	if err != nil || !ok || claimed.SourceMatrixEventID != "$cancel-one" ||
		claimed.LeaseGeneration != takeover.LeaseGeneration || claimed.RecoveryCount != 1 {
		t.Fatalf("takeover ClaimControl = (%+v, %t, %v)", claimed, ok, err)
	}
	if err := store.TransitionControl(t.Context(), ControlTransitionRequest{
		Lease: job.LeaseToken(), ControlID: control.ControlID,
		From: ControlPrepared, To: ControlApplied, At: takeoverAt.Add(2 * time.Second),
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale control transition error = %v, want ErrLeaseLost", err)
	}
	if err := store.TransitionControl(t.Context(), ControlTransitionRequest{
		Lease: takeover.LeaseToken(), ControlID: claimed.ControlID,
		From: ControlPrepared, To: ControlApplied, At: takeoverAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("takeover control transition: %v", err)
	}
	controls, err := store.Controls(t.Context(), job.JobID)
	if err != nil || len(controls) != 2 || controls[0].State != ControlApplied ||
		controls[0].RecoveryCount != 1 || len(controls[0].Payload) != 0 ||
		controls[0].TerminalAt.IsZero() || controls[1].State != ControlPending {
		t.Fatalf("controls after takeover = (%+v, %v)", controls, err)
	}
}

func TestMemoryTerminalControlReplayUsesContentFreeFingerprint(t *testing.T) {
	store := NewMemory()
	delegation := testDelegation("$control-replay-job", "agent", "!room")
	mustAdmit(t, store, testAdmission("txn-control-replay-job", ledgerEpoch, delegation))
	job := mustClaim(t, store, "worker", ledgerEpoch.Add(time.Second))
	if err := store.RecordMatrixEvent(t.Context(), MatrixEventRequest{
		Lease: job.LeaseToken(), At: ledgerEpoch.Add(2 * time.Second),
		Stage: MatrixEventPlaceholder, EventID: "$control-replay-placeholder",
	}); err != nil {
		t.Fatalf("record placeholder: %v", err)
	}
	control := NewControl{
		TargetMatrixEventID: "$control-replay-placeholder", SourceMatrixEventID: "$control-replay-answer",
		RoomID: delegation.RoomID, SenderMXID: delegation.SenderMXID,
		Kind: ControlContinuation, Slot: 3, Payload: []byte("sensitive answer"), Authorized: true,
	}
	admission := testAdmission("txn-control-replay-answer", ledgerEpoch.Add(3*time.Second))
	admission.ControlCapacity = 8
	admission.Controls = []NewControl{control}
	result := mustAdmit(t, store, admission)
	if len(result.InsertedControlIDs) != 1 {
		t.Fatalf("initial control admission = %+v", result)
	}
	claimed, found, err := store.ClaimControl(t.Context(), job.LeaseToken(), ledgerEpoch.Add(4*time.Second))
	if err != nil || !found {
		t.Fatalf("claim control = (%+v, %t, %v)", claimed, found, err)
	}
	if err := store.TransitionControl(t.Context(), ControlTransitionRequest{
		Lease: job.LeaseToken(), ControlID: claimed.ControlID, From: ControlPrepared,
		To: ControlApplied, At: ledgerEpoch.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("apply control: %v", err)
	}
	replay := testAdmission("txn-control-replay-redelivery", ledgerEpoch.Add(6*time.Second))
	replay.ControlCapacity = 8
	replay.Controls = []NewControl{control}
	result = mustAdmit(t, store, replay)
	if len(result.ExistingControlIDs) != 1 || result.ExistingControlIDs[0] != claimed.ControlID {
		t.Fatalf("terminal control replay = %+v", result)
	}
	changed := control
	changed.Payload = []byte("changed answer")
	conflict := testAdmission("txn-control-replay-conflict", ledgerEpoch.Add(7*time.Second))
	conflict.ControlCapacity = 8
	conflict.Controls = []NewControl{changed}
	if _, err := store.AdmitTransaction(t.Context(), conflict); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("changed terminal control replay error = %v, want ErrControlConflict", err)
	}
}

func TestMemoryDurableControlAuthorizationBoundsAndPlannedSlots(t *testing.T) {
	store := NewMemory()
	mustAdmit(t, store, testAdmission(
		"txn-control-bounds", ledgerEpoch, testDelegation("$bounded", "agent", "!room"),
	))
	job := mustClaim(t, store, "worker", ledgerEpoch)
	if err := store.RecordMatrixEvent(t.Context(), MatrixEventRequest{
		Lease: job.LeaseToken(), At: ledgerEpoch.Add(time.Second),
		Stage: MatrixEventPlaceholder, EventID: "$bounded-placeholder",
	}); err != nil {
		t.Fatal(err)
	}
	deniedAdmission := testAdmission("txn-wrong-sender", ledgerEpoch.Add(2*time.Second))
	deniedAdmission.ControlCapacity = 4
	deniedAdmission.Controls = []NewControl{{
		TargetMatrixEventID: "$bounded-placeholder", SourceMatrixEventID: "$wrong-answer",
		RoomID: "!room", SenderMXID: "@mallory:example.test", Kind: ControlContinuation,
		Authorized: false, ErrorCode: "control_sender_rejected",
	}}
	result := mustAdmit(t, store, deniedAdmission)
	if len(result.InsertedControlIDs) != 1 {
		t.Fatalf("denied control admission = %+v", result)
	}
	controls, _ := store.Controls(t.Context(), job.JobID)
	if len(controls) != 1 || controls[0].State != ControlDenied ||
		controls[0].ErrorCode != "control_sender_rejected" || len(controls[0].Payload) != 0 {
		t.Fatalf("denied control = %+v", controls)
	}

	fullAdmission := testAdmission("txn-control-full", ledgerEpoch.Add(3*time.Second))
	fullAdmission.ControlCapacity = 4
	fullAdmission.Controls = []NewControl{{
		TargetMatrixEventID: "$bounded-placeholder", SourceMatrixEventID: "$over-capacity",
		RoomID: "!room", SenderMXID: "@alice:example.test", Kind: ControlCancel, Authorized: true,
	}}
	result = mustAdmit(t, store, fullAdmission)
	if len(result.InsertedControlIDs) != 1 || len(result.UnmatchedControlIDs) != 0 {
		t.Fatalf("authorized control admission after denied evidence = %+v", result)
	}

	duplicateDenied := testAdmission("txn-wrong-sender-duplicate", ledgerEpoch.Add(3500*time.Millisecond))
	duplicateDenied.ControlCapacity = 4
	duplicateDenied.Controls = []NewControl{{
		TargetMatrixEventID: "$bounded-placeholder", SourceMatrixEventID: "$wrong-answer-duplicate",
		RoomID: "!room", SenderMXID: "@eve:example.test", Kind: ControlContinuation,
		Authorized: false, ErrorCode: "control_sender_rejected",
	}}
	result = mustAdmit(t, store, duplicateDenied)
	if len(result.UnmatchedControlIDs) != 1 || len(result.InsertedControlIDs) != 0 {
		t.Fatalf("duplicate denied control admission = %+v", result)
	}

	planned, err := store.PlanControl(t.Context(), PlanControlRequest{
		Lease: job.LeaseToken(), At: ledgerEpoch.Add(4 * time.Second), Kind: ControlProgress,
		Slot: 0, Capacity: 4, Payload: []byte("first"),
	})
	if err != nil || string(planned.Payload) != "first" {
		t.Fatalf("PlanControl first = (%+v, %v)", planned, err)
	}
	planned, err = store.PlanControl(t.Context(), PlanControlRequest{
		Lease: job.LeaseToken(), At: ledgerEpoch.Add(5 * time.Second), Kind: ControlProgress,
		Slot: 0, Capacity: 4, Payload: []byte("collapsed latest"),
	})
	if err != nil || string(planned.Payload) != "collapsed latest" {
		t.Fatalf("PlanControl collapse = (%+v, %v)", planned, err)
	}
}

func TestMemoryTransactionReplayHashAndAtomicTargetAdmission(t *testing.T) {
	store := NewMemory()
	first := testDelegation("$one", "agent-a", "!room")
	second := testDelegation("$two", "agent-b", "!room")
	admission := testAdmission("txn-one", ledgerEpoch, first, second)
	result := mustAdmit(t, store, admission)
	if result.Disposition != TransactionAccepted || len(result.InsertedJobIDs) != 2 || len(result.ExistingJobIDs) != 0 {
		t.Fatalf("first admission = %+v", result)
	}

	replay := mustAdmit(t, store, admission)
	if replay.Disposition != TransactionReplay || len(replay.InsertedJobIDs) != 0 || len(replay.ExistingJobIDs) != 0 {
		t.Fatalf("exact replay = %+v", replay)
	}
	changedHash := admission
	changedHash.BodyHash = HashTransaction([]byte("changed exact request bytes"))
	if _, err := store.AdmitTransaction(t.Context(), changedHash); !errors.Is(err, ErrTransactionHashConflict) {
		t.Fatalf("changed transaction replay error = %v, want ErrTransactionHashConflict", err)
	}

	changedEvidence := first
	changedEvidence.TargetFingerprint = "sha256:redirected-agent"
	third := testDelegation("$three", "agent-c", "!other")
	conflictingBatch := testAdmission("txn-two", ledgerEpoch.Add(time.Second), changedEvidence, third)
	if _, err := store.AdmitTransaction(t.Context(), conflictingBatch); !errors.Is(err, ErrDelegationConflict) {
		t.Fatalf("changed target evidence error = %v, want ErrDelegationConflict", err)
	}
	if _, ok, err := store.Job(t.Context(), JobIDFor(third.MatrixEventID, third.GhostMXID)); err != nil || ok {
		t.Fatalf("conflicting batch partially inserted third job: ok=%t err=%v", ok, err)
	}

	correctedBatch := testAdmission("txn-two", ledgerEpoch.Add(time.Second), first, third)
	result = mustAdmit(t, store, correctedBatch)
	if result.Disposition != TransactionAccepted || len(result.ExistingJobIDs) != 1 || len(result.InsertedJobIDs) != 1 {
		t.Fatalf("corrected atomic batch = %+v", result)
	}

	// Admission and Job return copies: callers cannot mutate the durable payload through aliases.
	third.Payload[0] = 'X'
	stored, ok, err := store.Job(t.Context(), JobIDFor(third.MatrixEventID, third.GhostMXID))
	if err != nil || !ok {
		t.Fatalf("load third job: ok=%t err=%v", ok, err)
	}
	stored.Payload[0] = 'Y'
	reloaded, _, _ := store.Job(t.Context(), stored.JobID)
	if string(reloaded.Payload) != "minimum recoverable payload" {
		t.Fatalf("durable payload was aliased: %q", reloaded.Payload)
	}
}

func TestMemoryTerminalTransitionScrubsPendingAndPreparedControls(t *testing.T) {
	store, job, at := memoryJobAtState(t, StatePending)
	for slot, kind := range []ControlKind{ControlQuestion, ControlProgress} {
		if _, err := store.PlanControl(t.Context(), PlanControlRequest{
			Lease: job.LeaseToken(), At: at.Add(time.Duration(slot+1) * time.Second),
			Kind: kind, Slot: slot, Capacity: 5, Payload: []byte("sensitive control content"),
		}); err != nil {
			t.Fatalf("PlanControl(%s): %v", kind, err)
		}
	}
	if _, found, err := store.ClaimControl(t.Context(), job.LeaseToken(), at.Add(3*time.Second)); err != nil || !found {
		t.Fatalf("ClaimControl before terminal transition = (%t, %v)", found, err)
	}
	mustTransition(
		t, store, job.LeaseToken(), StatePending, StateDead, at.Add(4*time.Second), TransitionPatch{},
	)
	controls, err := store.Controls(t.Context(), job.JobID)
	if err != nil || len(controls) != 2 {
		t.Fatalf("terminal controls = (%+v, %v)", controls, err)
	}
	for _, control := range controls {
		if control.State != ControlDead || control.ErrorCode != parentTerminalControlError ||
			len(control.Payload) != 0 || control.TerminalAt.IsZero() {
			t.Errorf("terminal control retained content or pending work: %+v", control)
		}
	}
}

func TestMemoryCapacityCountsLeasedAndDelayedJobsWithRoomPrecedence(t *testing.T) {
	store := NewMemory()
	leasing := testDelegation("$leased", "agent", "!room-full")
	delayed := testDelegation("$delayed", "agent", "!other-full")
	mustAdmit(t, store, testAdmission("txn-backlog", ledgerEpoch, leasing, delayed))

	leasedJob := mustClaim(t, store, "worker-leased", ledgerEpoch)
	if leasedJob.MatrixEventID != leasing.MatrixEventID {
		t.Fatalf("first leased job = %q, want %q", leasedJob.MatrixEventID, leasing.MatrixEventID)
	}
	delayedJob := mustClaim(t, store, "worker-delayed", ledgerEpoch)
	if delayedJob.MatrixEventID != delayed.MatrixEventID {
		t.Fatalf("second leased job = %q, want %q", delayedJob.MatrixEventID, delayed.MatrixEventID)
	}
	if err := store.ScheduleRetry(t.Context(), RetryRequest{
		Lease:         delayedJob.LeaseToken(),
		At:            ledgerEpoch.Add(time.Second),
		NextAttemptAt: ledgerEpoch.Add(time.Hour),
		ErrorCode:     "delayed_test",
		Kind:          RetryFailure,
	}); err != nil {
		t.Fatalf("delay second job: %v", err)
	}

	roomDenied := testDelegation("$room-denied", "agent", leasing.RoomID)
	globalDenied := testDelegation("$global-denied", "agent", "!available-room")
	limited := testAdmission(
		"txn-capacity-denied",
		ledgerEpoch.Add(2*time.Second),
		roomDenied,
		globalDenied,
	)
	limited.RoomCapacity = 1
	limited.GlobalCapacity = 2
	result := mustAdmit(t, store, limited)
	wantDenials := []CapacityDenial{
		{JobID: JobIDFor(roomDenied.MatrixEventID, roomDenied.GhostMXID), Reason: QueueRoomCapacityRejected},
		{JobID: JobIDFor(globalDenied.MatrixEventID, globalDenied.GhostMXID), Reason: QueueGlobalCapacityRejected},
	}
	if result.Disposition != TransactionAccepted || len(result.InsertedJobIDs) != 0 ||
		len(result.ExistingJobIDs) != 0 || len(result.LegacyTombstonedJobIDs) != 0 ||
		!capacityDenialsEqual(result.CapacityDenied, wantDenials) {
		t.Fatalf("capacity-limited admission = %+v, want denials %+v", result, wantDenials)
	}
	for _, denial := range wantDenials {
		assertCapacityDeniedJob(t, store, denial, limited.TransactionID, limited.CommittedAt)
	}

	replay := mustAdmit(t, store, limited)
	if replay.Disposition != TransactionReplay || len(replay.CapacityDenied) != 0 {
		t.Fatalf("exact capacity-denied replay = %+v", replay)
	}
	redelivery := mustAdmit(
		t,
		store,
		testAdmission("txn-capacity-existing", limited.CommittedAt.Add(time.Second), roomDenied),
	)
	if len(redelivery.ExistingJobIDs) != 1 || len(redelivery.CapacityDenied) != 0 {
		t.Fatalf("capacity-denied job redelivery = %+v, want existing evidence", redelivery)
	}
}

func TestMemoryCapacityExcludesTerminalTombstones(t *testing.T) {
	store := NewMemory()
	first := testDelegation("$terminal", "agent", "!room")
	mustAdmit(t, store, testAdmission("txn-terminal-capacity", ledgerEpoch, first))
	job := mustClaim(t, store, "worker", ledgerEpoch)
	mustTransition(
		t,
		store,
		job.LeaseToken(),
		StatePending,
		StateDenied,
		ledgerEpoch.Add(time.Second),
		TransitionPatch{},
	)

	second := testDelegation("$after-terminal", "agent", first.RoomID)
	admission := testAdmission("txn-after-terminal", ledgerEpoch.Add(2*time.Second), second)
	admission.RoomCapacity = 1
	admission.GlobalCapacity = 1
	result := mustAdmit(t, store, admission)
	if len(result.InsertedJobIDs) != 1 || len(result.CapacityDenied) != 0 {
		t.Fatalf("admission after terminal tombstone = %+v, want one pending job", result)
	}
}

func TestMemoryNonTerminalCountIncludesLeasedAndDelayedRecovery(t *testing.T) {
	store := NewMemory()
	mustAdmit(t, store, testAdmission(
		"txn-count",
		ledgerEpoch,
		testDelegation("$leased", "agent-a", "!room-a"),
		testDelegation("$delayed", "agent-b", "!room-b"),
	))
	leased := mustClaim(t, store, "worker-a", ledgerEpoch)
	delayed := mustClaim(t, store, "worker-b", ledgerEpoch)
	if err := store.ScheduleRetry(t.Context(), RetryRequest{
		Lease:         delayed.LeaseToken(),
		At:            ledgerEpoch.Add(time.Second),
		NextAttemptAt: ledgerEpoch.Add(time.Hour),
		ErrorCode:     "delayed_test",
		Kind:          RetryFailure,
	}); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	if got, err := store.NonTerminalCount(t.Context()); err != nil || got != 2 {
		t.Fatalf("NonTerminalCount with leased and delayed jobs = (%d, %v), want (2, nil)", got, err)
	}
	mustTransition(
		t,
		store,
		leased.LeaseToken(),
		StatePending,
		StateDenied,
		ledgerEpoch.Add(2*time.Second),
		TransitionPatch{},
	)
	if got, err := store.NonTerminalCount(t.Context()); err != nil || got != 1 {
		t.Fatalf("NonTerminalCount after terminal transition = (%d, %v), want (1, nil)", got, err)
	}
}

func assertCapacityDeniedJob(
	t *testing.T,
	store *Memory,
	denial CapacityDenial,
	transactionID string,
	terminalAt time.Time,
) {
	t.Helper()
	job, ok, err := store.Job(t.Context(), denial.JobID)
	if err != nil || !ok {
		t.Fatalf("load capacity-denied job %q: ok=%t err=%v", denial.JobID, ok, err)
	}
	if job.State != StateDenied || job.ErrorCode != denial.Reason ||
		!job.AdmissionChecked || job.AdmissionAllowed || job.AdmissionReason != denial.Reason ||
		job.AppserviceTransactionID != transactionID || !job.TerminalAt.Equal(terminalAt) ||
		job.Prompt != "" || len(job.Payload) != 0 || job.ResultText != "" ||
		job.LeaseToken() != (LeaseToken{}) {
		t.Fatalf("capacity-denied job = %+v", job)
	}
}

func capacityDenialsEqual(left, right []CapacityDenial) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestAdmissionNormalizesNilAndEmptyPayload(t *testing.T) {
	store := NewMemory()
	delegation := testDelegation("$empty-payload", "agent", "!room")
	delegation.Payload = nil
	mustAdmit(t, store, testAdmission("txn-nil-payload", ledgerEpoch, delegation))
	delegation.Payload = []byte{}
	result := mustAdmit(t, store, testAdmission("txn-empty-payload", ledgerEpoch.Add(time.Second), delegation))
	if len(result.ExistingJobIDs) != 1 || len(result.InsertedJobIDs) != 0 {
		t.Fatalf("normalized empty payload admission = %+v", result)
	}
	stored, ok, err := store.Job(t.Context(), JobIDFor(delegation.MatrixEventID, delegation.GhostMXID))
	if err != nil || !ok {
		t.Fatalf("load empty-payload job: ok=%t err=%v", ok, err)
	}
	if stored.Payload == nil || len(stored.Payload) != 0 {
		t.Fatalf("empty payload persisted as %#v, want non-nil empty bytes", stored.Payload)
	}
}

func TestAdmissionHonorsLegacyProcessedEventTombstones(t *testing.T) {
	store := NewMemory()
	eventID := "$legacy-event"
	store.processed[eventID] = ledgerEpoch.Add(-retention)
	first := testDelegation(eventID, "agent-a", "!room")
	second := testDelegation(eventID, "agent-b", "!room")
	result := mustAdmit(t, store, testAdmission("txn-legacy", ledgerEpoch, first, second))
	if len(result.LegacyTombstonedJobIDs) != 2 || len(result.InsertedJobIDs) != 0 || len(result.ExistingJobIDs) != 0 {
		t.Fatalf("legacy-tombstoned admission = %+v", result)
	}
	for _, delegation := range []NewDelegation{first, second} {
		if _, ok, err := store.Job(t.Context(), JobIDFor(delegation.MatrixEventID, delegation.GhostMXID)); err != nil || ok {
			t.Fatalf("legacy event created job for %s: ok=%t err=%v", delegation.GhostMXID, ok, err)
		}
	}

	expired := testDelegation("$expired-legacy", "agent", "!other")
	store.processed[expired.MatrixEventID] = ledgerEpoch.Add(-retention - time.Nanosecond)
	result = mustAdmit(t, store, testAdmission("txn-expired-legacy", ledgerEpoch, expired))
	if len(result.InsertedJobIDs) != 1 || len(result.LegacyTombstonedJobIDs) != 0 {
		t.Fatalf("expired legacy tombstone admission = %+v", result)
	}

	// A durable job is stronger evidence than a later legacy event-level marker.
	store.processed[expired.MatrixEventID] = ledgerEpoch
	result = mustAdmit(t, store, testAdmission("txn-existing-over-legacy", ledgerEpoch.Add(time.Second), expired))
	if len(result.ExistingJobIDs) != 1 || len(result.LegacyTombstonedJobIDs) != 0 {
		t.Fatalf("durable job with legacy marker admission = %+v", result)
	}
}

func TestMemoryClaimsOnceAndPreservesRoomFIFO(t *testing.T) {
	store := NewMemory()
	a1 := testDelegation("$a1", "agent", "!room-a")
	a2 := testDelegation("$a2", "agent", "!room-a")
	b1 := testDelegation("$b1", "agent", "!room-b")
	mustAdmit(t, store, testAdmission("txn-fifo", ledgerEpoch, a1, a2, b1))

	first := mustClaim(t, store, "worker-a", ledgerEpoch)
	if first.MatrixEventID != a1.MatrixEventID {
		t.Fatalf("first claim = %s, want %s", first.MatrixEventID, a1.MatrixEventID)
	}
	secondRoom := mustClaim(t, store, "worker-b", ledgerEpoch)
	if secondRoom.MatrixEventID != b1.MatrixEventID {
		t.Fatalf("concurrent-room claim = %s, want %s", secondRoom.MatrixEventID, b1.MatrixEventID)
	}
	if _, ok, err := store.Claim(t.Context(), ClaimRequest{
		Owner:         "worker-c",
		Now:           ledgerEpoch,
		LeaseDuration: time.Hour,
	}); err != nil || ok {
		t.Fatalf("third claim while room heads leased = (%t, %v), want no job", ok, err)
	}

	replacement, ok, err := store.Claim(t.Context(), ClaimRequest{
		Owner:         "replacement",
		Now:           ledgerEpoch.Add(time.Hour),
		LeaseDuration: time.Hour,
	})
	if err != nil || !ok {
		t.Fatalf("expired lease replacement = (%t, %v)", ok, err)
	}
	if replacement.JobID != first.JobID || replacement.LeaseGeneration != first.LeaseGeneration+1 {
		t.Fatalf("replacement = %+v, want same job with next generation", replacement)
	}
	if err := store.Heartbeat(t.Context(), first.LeaseToken(), ledgerEpoch.Add(time.Hour), time.Hour); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat error = %v, want ErrLeaseLost", err)
	}
	if err := store.Transition(t.Context(), TransitionRequest{
		Lease: first.LeaseToken(),
		From:  StatePending,
		To:    StateDenied,
		At:    ledgerEpoch.Add(time.Hour),
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale transition error = %v, want ErrLeaseLost", err)
	}

	mustTransition(
		t,
		store,
		replacement.LeaseToken(),
		StatePending,
		StateDenied,
		ledgerEpoch.Add(time.Hour+time.Second),
		TransitionPatch{},
	)
	next := mustClaim(t, store, "worker-next", ledgerEpoch.Add(time.Hour+time.Second))
	if next.MatrixEventID != a2.MatrixEventID {
		t.Fatalf("next same-room claim = %s, want %s", next.MatrixEventID, a2.MatrixEventID)
	}
}

func TestMemoryConcurrentWorkersCannotClaimSameJob(t *testing.T) {
	store := NewMemory()
	mustAdmit(t, store, testAdmission("txn-concurrent", ledgerEpoch, testDelegation("$event", "agent", "!room")))

	start := make(chan struct{})
	claimed := make(chan string, 2)
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		owner := owner
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			job, ok, err := store.Claim(t.Context(), ClaimRequest{
				Owner:         owner,
				Now:           ledgerEpoch,
				LeaseDuration: time.Minute,
			})
			if err != nil {
				errorsFound <- err
				return
			}
			if ok {
				claimed <- job.JobID
			}
		}()
	}
	close(start)
	workers.Wait()
	close(claimed)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("concurrent claim: %v", err)
	}
	var count int
	for range claimed {
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent successful claims = %d, want 1", count)
	}
}

func TestMemoryHeartbeatBackoffAndGenerationFencing(t *testing.T) {
	store := NewMemory()
	mustAdmit(t, store, testAdmission("txn-retry", ledgerEpoch, testDelegation("$event", "agent", "!room")))
	first, ok, err := store.Claim(t.Context(), ClaimRequest{
		Owner:         "worker-a",
		Now:           ledgerEpoch,
		LeaseDuration: time.Minute,
	})
	if err != nil || !ok {
		t.Fatalf("initial claim = (%t, %v)", ok, err)
	}
	if err := store.Heartbeat(t.Context(), first.LeaseToken(), ledgerEpoch.Add(30*time.Second), 2*time.Minute); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if _, ok, err := store.Claim(t.Context(), ClaimRequest{
		Owner:         "worker-b",
		Now:           ledgerEpoch.Add(61 * time.Second),
		LeaseDuration: time.Minute,
	}); err != nil || ok {
		t.Fatalf("claim before heartbeat expiry = (%t, %v), want no job", ok, err)
	}

	retryAt := ledgerEpoch.Add(40 * time.Second)
	nextAttempt := ledgerEpoch.Add(5 * time.Minute)
	if err := store.ScheduleRetry(t.Context(), RetryRequest{
		Lease:         first.LeaseToken(),
		At:            retryAt,
		NextAttemptAt: nextAttempt,
		ErrorCode:     "a2a_unavailable",
		Kind:          RetryFailure,
	}); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	if _, ok, err := store.Claim(t.Context(), ClaimRequest{
		Owner:         "worker-b",
		Now:           nextAttempt.Add(-time.Nanosecond),
		LeaseDuration: time.Minute,
	}); err != nil || ok {
		t.Fatalf("claim before backoff = (%t, %v), want no job", ok, err)
	}
	replacement := mustClaim(t, store, "worker-b", nextAttempt)
	if replacement.LeaseGeneration != first.LeaseGeneration+1 || replacement.AttemptCount != 1 {
		t.Fatalf("replacement fence/attempt = generation %d attempt %d", replacement.LeaseGeneration, replacement.AttemptCount)
	}
	if replacement.ErrorCode != "a2a_unavailable" {
		t.Fatalf("replacement error code = %q", replacement.ErrorCode)
	}
	if err := store.ScheduleRetry(t.Context(), RetryRequest{
		Lease:         first.LeaseToken(),
		At:            nextAttempt,
		NextAttemptAt: nextAttempt.Add(time.Minute),
		ErrorCode:     "stale_worker",
		Kind:          RetryFailure,
	}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale retry error = %v, want ErrLeaseLost", err)
	}
	pollAt := nextAttempt.Add(time.Minute)
	if err := store.ScheduleRetry(t.Context(), RetryRequest{
		Lease:         replacement.LeaseToken(),
		At:            nextAttempt,
		NextAttemptAt: pollAt,
		ErrorCode:     "task_working",
		Kind:          RetryPoll,
	}); err != nil {
		t.Fatalf("schedule routine task poll: %v", err)
	}
	poll := mustClaim(t, store, "worker-c", pollAt)
	if poll.AttemptCount != 0 || poll.PollCount != 1 {
		t.Fatalf("routine task poll cursors = failures %d polls %d, want 0/1", poll.AttemptCount, poll.PollCount)
	}
}

func TestMemorySuccessfulTransitionResetsFailureCount(t *testing.T) {
	store := NewMemory()
	mustAdmit(t, store, testAdmission("txn-reset", ledgerEpoch, testDelegation("$reset", "agent", "!room")))
	first := mustClaim(t, store, "worker-a", ledgerEpoch)
	retryAt := ledgerEpoch.Add(time.Second)
	if err := store.ScheduleRetry(t.Context(), RetryRequest{
		Lease:         first.LeaseToken(),
		At:            retryAt,
		NextAttemptAt: retryAt,
		ErrorCode:     "transient_failure",
		Kind:          RetryFailure,
	}); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	replacement := mustClaim(t, store, "worker-b", retryAt)
	if replacement.AttemptCount != 1 {
		t.Fatalf("failure count before progress = %d, want 1", replacement.AttemptCount)
	}
	mustTransition(
		t,
		store,
		replacement.LeaseToken(),
		StatePending,
		StateA2APrepared,
		retryAt.Add(time.Second),
		TransitionPatch{},
	)
	stored, _, _ := store.Job(t.Context(), replacement.JobID)
	if stored.AttemptCount != 0 || stored.PollCount != 0 {
		t.Fatalf("retry cursors after successful transition = failures %d polls %d, want 0/0",
			stored.AttemptCount, stored.PollCount)
	}
}

func TestMemoryPersistsAdmissionExactlyOnce(t *testing.T) {
	store, job, at := memoryJobAtState(t, StatePending)
	request := AdmissionRequest{
		Lease:   job.LeaseToken(),
		At:      at.Add(time.Second),
		Allowed: true,
		Reason:  "policy_allowed",
	}
	if err := store.RecordAdmission(t.Context(), request); err != nil {
		t.Fatalf("RecordAdmission: %v", err)
	}
	request.At = request.At.Add(time.Second)
	if err := store.RecordAdmission(t.Context(), request); err != nil {
		t.Fatalf("idempotent RecordAdmission: %v", err)
	}
	request.Allowed = false
	request.Reason = "policy_denied"
	if err := store.RecordAdmission(t.Context(), request); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed admission error = %v, want ErrAdmissionConflict", err)
	}
	stored, _, _ := store.Job(t.Context(), job.JobID)
	if !stored.AdmissionChecked || !stored.AdmissionAllowed || stored.AdmissionReason != "policy_allowed" {
		t.Fatalf("persisted admission = checked=%t allowed=%t reason=%q", stored.AdmissionChecked, stored.AdmissionAllowed, stored.AdmissionReason)
	}
}

func TestMemoryRecordsMatrixEventsWithoutStateTransition(t *testing.T) {
	store, job, at := memoryJobAtState(t, StateAwaitingTask)
	request := MatrixEventRequest{
		Lease:   job.LeaseToken(),
		At:      at.Add(time.Second),
		Stage:   MatrixEventPlaceholder,
		EventID: "$placeholder",
	}
	if err := store.RecordMatrixEvent(t.Context(), request); err != nil {
		t.Fatalf("RecordMatrixEvent: %v", err)
	}
	request.At = request.At.Add(time.Second)
	if err := store.RecordMatrixEvent(t.Context(), request); err != nil {
		t.Fatalf("idempotent RecordMatrixEvent: %v", err)
	}
	request.EventID = "$different"
	if err := store.RecordMatrixEvent(t.Context(), request); !errors.Is(err, ErrMatrixEventConflict) {
		t.Fatalf("changed Matrix event error = %v, want ErrMatrixEventConflict", err)
	}
	stored, _, _ := store.Job(t.Context(), job.JobID)
	if stored.State != StateAwaitingTask || stored.MatrixPlaceholderEventID != "$placeholder" {
		t.Fatalf("recorded placeholder = state %s event %q", stored.State, stored.MatrixPlaceholderEventID)
	}
}

func TestMemoryRecordsDeadManDelayWithoutStateTransition(t *testing.T) {
	store, job, at := memoryJobAtState(t, StateAwaitingTask)
	request := DeadManRequest{
		Lease:   job.LeaseToken(),
		At:      at.Add(time.Second),
		DelayID: "delay-1",
	}
	if err := store.RecordDeadMan(t.Context(), request); err != nil {
		t.Fatalf("RecordDeadMan: %v", err)
	}
	request.At = request.At.Add(time.Second)
	if err := store.RecordDeadMan(t.Context(), request); err != nil {
		t.Fatalf("idempotent RecordDeadMan: %v", err)
	}
	request.DelayID = "delay-2"
	if err := store.RecordDeadMan(t.Context(), request); !errors.Is(err, ErrDeadManConflict) {
		t.Fatalf("changed delayed-event ID error = %v, want ErrDeadManConflict", err)
	}
	stored, _, _ := store.Job(t.Context(), job.JobID)
	if stored.State != StateAwaitingTask || stored.MatrixDeadManDelayID != "delay-1" {
		t.Fatalf("recorded dead-man = state %s delay %q", stored.State, stored.MatrixDeadManDelayID)
	}
}

func TestMemoryTransitionCannotReplaceMatrixEventEvidence(t *testing.T) {
	store, job, at := memoryJobAtState(t, StateReplyPending)
	request := MatrixEventRequest{
		Lease:   job.LeaseToken(),
		At:      at.Add(time.Second),
		Stage:   MatrixEventReply,
		EventID: "$reply",
	}
	if err := store.RecordMatrixEvent(t.Context(), request); err != nil {
		t.Fatalf("RecordMatrixEvent: %v", err)
	}
	different := "$different"
	err := store.Transition(t.Context(), TransitionRequest{
		Lease: job.LeaseToken(),
		From:  StateReplyPending,
		To:    StateDelivered,
		At:    request.At.Add(time.Second),
		Patch: TransitionPatch{MatrixReplyEventID: &different},
	})
	if !errors.Is(err, ErrMatrixEventConflict) {
		t.Fatalf("changed transition event error = %v, want ErrMatrixEventConflict", err)
	}
	stored, _, _ := store.Job(t.Context(), job.JobID)
	if stored.State != StateReplyPending || stored.MatrixReplyEventID != "$reply" {
		t.Fatalf("conflicting transition mutated job: state=%s event=%q", stored.State, stored.MatrixReplyEventID)
	}
	same := "$reply"
	mustTransition(
		t,
		store,
		job.LeaseToken(),
		StateReplyPending,
		StateDelivered,
		request.At.Add(2*time.Second),
		TransitionPatch{MatrixReplyEventID: &same},
	)
}

func TestMemoryTransitionAtomicallyUpdatesConversationContext(t *testing.T) {
	store, job, at := memoryJobAtState(t, StateA2APrepared)
	if err := store.SetContext(t.Context(), job.RoomID, job.GhostLocalpart, "old-context"); err != nil {
		t.Fatal(err)
	}
	contextID := "new-context"
	mustTransition(
		t,
		store,
		job.LeaseToken(),
		StateA2APrepared,
		StateAwaitingTask,
		at.Add(time.Second),
		TransitionPatch{A2AContextID: &contextID},
	)
	stored, _, _ := store.Job(t.Context(), job.JobID)
	persistedContext, err := store.Context(t.Context(), job.RoomID, job.GhostLocalpart)
	if err != nil {
		t.Fatal(err)
	}
	if stored.A2AContextID != contextID || persistedContext != contextID {
		t.Fatalf("atomic context = job %q thread %q, want %q", stored.A2AContextID, persistedContext, contextID)
	}
}

func TestMemoryTerminalCleanupRetainsFailureEvidence(t *testing.T) {
	store := NewMemory()
	terminalAt := ledgerEpoch.Add(2 * time.Second)
	states := []DelegationState{StateDelivered, StateDenied, StateAmbiguous, StateDead}
	for i, terminalState := range states {
		delegation := testDelegation(fmt.Sprintf("$terminal-%d", i), "agent", fmt.Sprintf("!room-%d", i))
		mustAdmit(t, store, testAdmission(fmt.Sprintf("txn-terminal-%d", i), ledgerEpoch, delegation))
		job := mustClaim(t, store, fmt.Sprintf("worker-%d", i), ledgerEpoch)
		lease := job.LeaseToken()
		at := ledgerEpoch.Add(time.Second)
		switch terminalState {
		case StateDelivered, StateDenied:
			mustTransition(t, store, lease, StatePending, StateReplyPending, at, TransitionPatch{})
			at = at.Add(time.Second)
			mustTransition(t, store, lease, StateReplyPending, terminalState, at, TransitionPatch{})
		case StateAmbiguous:
			mustTransition(t, store, lease, StatePending, StateA2APrepared, at, TransitionPatch{})
			at = at.Add(time.Second)
			mustTransition(t, store, lease, StateA2APrepared, StateAmbiguous, at, TransitionPatch{})
		case StateDead:
			mustTransition(t, store, lease, StatePending, StateDead, terminalAt, TransitionPatch{})
		}
	}

	// Simulate content left by an older writer so cleanup's defensive scrub is exercised.
	store.mu.Lock()
	for jobID, job := range store.jobs {
		job.Prompt = "residual prompt"
		job.Payload = []byte("residual payload")
		job.ResultText = "residual result"
		store.jobs[jobID] = job
		if job.TerminalAt != terminalAt {
			t.Fatalf("terminal timestamp = %s, want %s", job.TerminalAt, terminalAt)
		}
	}
	store.mu.Unlock()

	beforeExpiry := terminalAt.Add(TerminalRetention - time.Nanosecond)
	result, err := store.CleanupTerminal(t.Context(), beforeExpiry)
	if err != nil {
		t.Fatalf("CleanupTerminal(before expiry): %v", err)
	}
	if result.ContentCleared != int64(len(states)) || result.TombstonesDeleted != 0 {
		t.Fatalf("cleanup before expiry = %+v", result)
	}
	result, err = store.CleanupTerminal(t.Context(), terminalAt.Add(TerminalRetention))
	if err != nil {
		t.Fatalf("CleanupTerminal(at expiry): %v", err)
	}
	if result.ContentCleared != 0 || result.TombstonesDeleted != 2 {
		t.Fatalf("cleanup at expiry = %+v", result)
	}
	if result.TransactionsDeleted != 2 {
		t.Fatalf("expired unreferenced transaction cleanup = %+v, want 2 deleted", result)
	}

	store.mu.Lock()
	if len(store.jobs) != 2 {
		t.Fatalf("retained terminal jobs = %d, want ambiguous and dead", len(store.jobs))
	}
	for _, job := range store.jobs {
		if job.State != StateAmbiguous && job.State != StateDead {
			t.Errorf("ordinary terminal job %s retained", job.State)
		}
		if job.Prompt != "" || len(job.Payload) != 0 || job.ResultText != "" {
			t.Errorf("retained %s job contains content", job.State)
		}
	}
	if len(store.jobOrder) != 2 {
		t.Errorf("job order retained %d stale tombstone entries, want 2", len(store.jobOrder))
	}
	store.mu.Unlock()

	// Once the non-content tombstone expires, the deterministic identity can be admitted again
	// without leaving an older ordering entry that would claim the new job out of sequence.
	reused := testDelegation("$terminal-0", "agent", "!room-0")
	resultAdmission := mustAdmit(t, store, testAdmission("txn-reused", terminalAt.Add(TerminalRetention), reused))
	if len(resultAdmission.InsertedJobIDs) != 1 {
		t.Fatalf("re-admission after tombstone expiry = %+v", resultAdmission)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.jobOrder) != 3 {
		t.Fatalf("job order after re-admission = %d entries, want 3", len(store.jobOrder))
	}
}

func TestMemoryCleanupBoundsTransactionsAndLegacyTombstones(t *testing.T) {
	store := NewMemory()
	cleanupAt := ledgerEpoch.Add(48 * time.Hour)
	mustAdmit(t, store, testAdmission("txn-old-empty", ledgerEpoch))
	mustAdmit(
		t,
		store,
		testAdmission("txn-old-referenced", ledgerEpoch, testDelegation("$pending", "agent", "!room")),
	)
	mustAdmit(t, store, testAdmission("txn-recent-empty", cleanupAt.Add(-time.Hour)))
	store.processed["$legacy-expired"] = cleanupAt.Add(-retention - time.Nanosecond)
	store.processed["$legacy-boundary"] = cleanupAt.Add(-retention)

	result, err := store.CleanupTerminal(t.Context(), cleanupAt)
	if err != nil {
		t.Fatalf("CleanupTerminal: %v", err)
	}
	if result.TransactionsDeleted != 1 || result.LegacyTombstonesDeleted != 1 {
		t.Fatalf("bounded metadata cleanup = %+v", result)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.transactions["txn-old-empty"]; ok {
		t.Error("expired unreferenced transaction was retained")
	}
	if _, ok := store.transactions["txn-old-referenced"]; !ok {
		t.Error("transaction referenced by pending job was deleted")
	}
	if _, ok := store.transactions["txn-recent-empty"]; !ok {
		t.Error("recent unreferenced transaction was deleted")
	}
	if _, ok := store.processed["$legacy-expired"]; ok {
		t.Error("expired legacy tombstone was retained")
	}
	if _, ok := store.processed["$legacy-boundary"]; !ok {
		t.Error("24-hour boundary legacy tombstone was deleted early")
	}
}

func TestMemoryCleanupRetainsTransactionReferencedOnlyByRetainedControl(t *testing.T) {
	store, job, at := memoryJobAtState(t, StatePending)
	placeholderID := "$retained-control-placeholder"
	if err := store.RecordMatrixEvent(t.Context(), MatrixEventRequest{
		Lease: job.LeaseToken(), At: at.Add(time.Second), Stage: MatrixEventPlaceholder, EventID: placeholderID,
	}); err != nil {
		t.Fatalf("RecordMatrixEvent: %v", err)
	}
	controlAdmission := testAdmission("txn-retained-control", at.Add(2*time.Second))
	controlAdmission.ControlCapacity = 5
	controlAdmission.Controls = []NewControl{{
		TargetMatrixEventID: placeholderID, SourceMatrixEventID: "$retained-control",
		RoomID: job.RoomID, SenderMXID: job.SenderMXID, Kind: ControlContinuation, Authorized: true,
		Payload: []byte("answer"),
	}}
	result := mustAdmit(t, store, controlAdmission)
	if len(result.InsertedControlIDs) != 1 {
		t.Fatalf("control admission = %+v", result)
	}
	mustTransition(
		t, store, job.LeaseToken(), StatePending, StateDead, at.Add(3*time.Second), TransitionPatch{},
	)
	if _, err := store.CleanupTerminal(t.Context(), at.Add(48*time.Hour)); err != nil {
		t.Fatalf("CleanupTerminal: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.transactions["txn-retained-control"]; !ok {
		t.Fatal("transaction referenced by a retained control was deleted")
	}
}
