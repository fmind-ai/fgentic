package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	// Register the same Postgres driver used by the bridge binary for the direct lease proof.
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.mau.fi/util/dbutil"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	crashPollInterval       = 100 * time.Millisecond
	crashReplyQuietTime     = time.Second
	crashLeaseProofFuture   = 24 * time.Hour
	crashLeaseProofDuration = 10 * time.Second
	crashLeaseProofCapacity = 1 << 20
	crashDatabaseURLEnv     = "BRIDGE_DATABASE_URL"
	crashDatabaseOwner      = "matrix-a2a-bridge"
	crashLeaseProofError    = "lease_fence_proof"
	crashWorkingText        = "⏳ working on it…"
	crashLongReply          = "long reply room=98 seq=01"
	crashAmbiguousReply     = "⚠️ Agent \"agent-integration\" may have received this request, but its acknowledgement was lost. The bridge did not resend it; check the agent before retrying."
)

var crashPhases = [...]string{
	"ledger_committed_pre_ack",
	"acknowledged_pre_claim",
	"a2a_accepted_pre_record",
	"control_intent_committed_pre_claim",
	"cancel_accepted_pre_record",
	"continuation_accepted_pre_record",
	"question_accepted_pre_record",
	"progress_accepted_pre_record",
	"pin_accepted_pre_record",
	"result_persisted_pre_matrix",
	"matrix_accepted_pre_record",
	"long_task_polling",
}

type crashFaultState struct {
	Mode        string   `json:"mode"`
	Armed       bool     `json:"armed"`
	Tripped     bool     `json:"tripped"`
	MatchedPath string   `json:"matched_path"`
	MatrixPaths []string `json:"matrix_paths"`
	A2AMethods  []string `json:"a2a_methods"`
}

func (f fixture) runCrashRecovery(ctx context.Context) error {
	startedAt := time.Now()
	if err := provePostgresLeaseFencing(ctx); err != nil {
		return err
	}
	sess, err := f.register(ctx)
	if err != nil {
		return err
	}
	ghost := "@" + ghostLocalpart + ":" + f.server
	roomID, err := f.createRoom(ctx, sess.AccessToken)
	if err != nil {
		return err
	}
	if err := f.grantRoomPower(ctx, sess.AccessToken, roomID, ghost, 50); err != nil {
		return err
	}
	if err := f.invite(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	if err := f.waitForJoin(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}

	if err := f.provePreAckRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.provePreClaimRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveAmbiguousA2ARecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveControlIntentRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveCancelControlRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveContinuationControlRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveQuestionProjectionRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveProgressProjectionRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.provePinProjectionRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.provePreMatrixRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveLostMatrixResponse(ctx, sess, roomID, ghost); err != nil {
		return err
	}
	if err := f.proveLongTaskRecovery(ctx, sess, roomID, ghost); err != nil {
		return err
	}

	slog.Info(
		"bridge hard-crash recovery scenario passed",
		"crash_recovery", "passed",
		"sigkill_boundaries", len(crashPhases),
		"scenario_duration_ms", time.Since(startedAt).Milliseconds(),
	)
	return nil
}

func (f fixture) grantRoomPower(
	ctx context.Context,
	token, roomID, userID string,
	level int,
) error {
	endpoint := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/state/m.room.power_levels",
		f.matrixURL,
		pathSegment(roomID),
	)
	status, body, err := f.request(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return fmt.Errorf("read room power levels: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("read room power levels: status %d: %s", status, body)
	}
	var content map[string]any
	if err := json.Unmarshal(body, &content); err != nil {
		return fmt.Errorf("decode room power levels: %w", err)
	}
	users, ok := content["users"].(map[string]any)
	if !ok {
		users = make(map[string]any)
		content["users"] = users
	}
	users[userID] = level
	status, body, err = f.request(ctx, http.MethodPut, endpoint, token, content)
	if err != nil {
		return fmt.Errorf("grant room power to %s: %w", userID, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("grant room power to %s: status %d: %s", userID, status, body)
	}
	return nil
}

type crashClaimResult struct {
	job     state.Job
	claimed bool
	err     error
}

// provePostgresLeaseFencing exercises the production ledger against the fixture's real Postgres
// before any SIGKILL boundary. The row is scheduled in the future so the live bridge's wall-clock
// workers cannot claim it while the driver advances logical time explicitly.
func provePostgresLeaseFencing(ctx context.Context) (returnedErr error) {
	databaseURL := strings.TrimSpace(os.Getenv(crashDatabaseURLEnv))
	if databaseURL == "" {
		return fmt.Errorf("%s must be set for the crash-recovery scenario", crashDatabaseURLEnv)
	}
	db, err := dbutil.NewWithDialect(databaseURL, "pgx")
	if err != nil {
		return fmt.Errorf("open bridge database for lease proof: %w", err)
	}
	db.Owner = crashDatabaseOwner
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("close lease-proof database: %w", closeErr))
		}
	}()
	store, err := state.NewPostgres(ctx, db)
	if err != nil {
		return fmt.Errorf("open bridge state for lease proof: %w", err)
	}

	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	committedAt := time.Now().UTC().Add(crashLeaseProofFuture).Truncate(time.Second)
	ghostMXID := "@agent-lease-proof:integration.test"
	eventID := "$lease-proof-" + runID + ":integration.test"
	jobID := state.JobIDFor(eventID, ghostMXID)
	result, err := store.AdmitTransaction(ctx, state.TransactionAdmission{
		TransactionID:  "lease-proof-" + runID,
		BodyHash:       state.HashTransaction([]byte("lease-proof-" + runID)),
		CommittedAt:    committedAt,
		RoomCapacity:   crashLeaseProofCapacity,
		GlobalCapacity: crashLeaseProofCapacity,
		Delegations: []state.NewDelegation{{
			MatrixEventID:       eventID,
			GhostMXID:           ghostMXID,
			GhostLocalpart:      "agent-lease-proof",
			RoomID:              "!lease-proof-" + runID + ":integration.test",
			SenderMXID:          "@lease-proof:integration.test",
			SenderOriginKind:    "matrix",
			SenderOriginNetwork: "matrix",
			OriginServerTS:      committedAt.UnixMilli(),
			TargetFingerprint:   "lease-proof-v1",
		}},
	})
	if err != nil {
		return fmt.Errorf("admit lease-proof delegation: %w", err)
	}
	if result.Disposition != state.TransactionAccepted || len(result.InsertedJobIDs) != 1 ||
		result.InsertedJobIDs[0] != jobID || len(result.ExistingJobIDs) != 0 ||
		len(result.LegacyTombstonedJobIDs) != 0 || len(result.CapacityDenied) != 0 {
		return fmt.Errorf("lease-proof admission evidence did not contain exactly one new job")
	}

	claimAt := committedAt.Add(time.Second)
	first, err := racePostgresClaims(ctx, store, jobID, claimAt)
	if err != nil {
		return err
	}
	replacementAt := claimAt.Add(crashLeaseProofDuration + time.Second)
	replacement, claimed, err := store.Claim(ctx, state.ClaimRequest{
		Owner:         "lease-proof-replacement",
		Now:           replacementAt,
		LeaseDuration: crashLeaseProofDuration,
	})
	if err != nil {
		return fmt.Errorf("claim lease-proof replacement: %w", err)
	}
	if !claimed || replacement.JobID != jobID || replacement.LeaseGeneration != first.LeaseGeneration+1 {
		return fmt.Errorf("lease-proof replacement did not acquire the next generation")
	}

	staleErr := store.Transition(ctx, state.TransitionRequest{
		Lease: first.LeaseToken(),
		From:  state.StatePending,
		To:    state.StateDenied,
		At:    replacementAt,
	})
	if staleErr == nil {
		return errors.New("stale lease-proof transition unexpectedly succeeded")
	}
	if !errors.Is(staleErr, state.ErrLeaseLost) {
		return fmt.Errorf("stale lease-proof transition did not return ErrLeaseLost: %w", staleErr)
	}
	if err := requirePersistedLease(ctx, store, replacement); err != nil {
		return err
	}

	terminalAt := replacementAt.Add(time.Second)
	errorCode := crashLeaseProofError
	if err := store.Transition(ctx, state.TransitionRequest{
		Lease: replacement.LeaseToken(),
		From:  state.StatePending,
		To:    state.StateDenied,
		At:    terminalAt,
		Patch: state.TransitionPatch{ErrorCode: &errorCode},
	}); err != nil {
		return fmt.Errorf("terminalize lease-proof delegation: %w", err)
	}
	persisted, found, err := store.Job(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load terminal lease-proof delegation: %w", err)
	}
	if !found || persisted.State != state.StateDenied || persisted.LeaseGeneration != replacement.LeaseGeneration ||
		persisted.LeaseOwner != "" || !persisted.LeaseExpiresAt.IsZero() ||
		!persisted.TerminalAt.Equal(terminalAt) || persisted.ErrorCode != crashLeaseProofError {
		return fmt.Errorf("terminal lease-proof evidence did not preserve the fresh fence and cleared lease")
	}
	if err := provePostgresControlFencing(ctx, store, committedAt.Add(time.Minute), runID); err != nil {
		return err
	}

	slog.Info(
		"postgres lease fencing scenario passed",
		"lease_fencing", "passed",
		"concurrent_claim_winners", 1,
		"stale_transition", "rejected",
		"terminal_state", persisted.State,
		"lease_generation", persisted.LeaseGeneration,
	)
	return nil
}

type crashControlClaimResult struct {
	control state.Control
	claimed bool
	err     error
}

// provePostgresControlFencing exercises the interactive-control outbox against the same real
// Postgres fixture. Concurrent callers share one job lease; only one may prepare the control, a
// replacement lease recovers it with explicit ambiguity evidence, and the stale owner is fenced.
func provePostgresControlFencing(
	ctx context.Context,
	store state.Store,
	committedAt time.Time,
	runID string,
) error {
	roomID := "!control-proof-" + runID + ":integration.test"
	ghostMXID := "@agent-control-proof:integration.test"
	eventID := "$control-proof-" + runID + ":integration.test"
	placeholderID := "$control-placeholder-" + runID + ":integration.test"
	jobID := state.JobIDFor(eventID, ghostMXID)
	result, err := store.AdmitTransaction(ctx, state.TransactionAdmission{
		TransactionID:  "control-job-proof-" + runID,
		BodyHash:       state.HashTransaction([]byte("control-job-proof-" + runID)),
		CommittedAt:    committedAt,
		RoomCapacity:   crashLeaseProofCapacity,
		GlobalCapacity: crashLeaseProofCapacity,
		Delegations: []state.NewDelegation{{
			MatrixEventID: eventID, GhostMXID: ghostMXID, GhostLocalpart: "agent-control-proof",
			RoomID: roomID, SenderMXID: "@control-proof:integration.test",
			SenderOriginKind: "matrix", SenderOriginNetwork: "matrix",
			OriginServerTS: committedAt.UnixMilli(), TargetFingerprint: "control-proof-v1",
		}},
	})
	if err != nil || len(result.InsertedJobIDs) != 1 || result.InsertedJobIDs[0] != jobID {
		return fmt.Errorf("admit control-proof delegation: result=%+v: %w", result, err)
	}
	firstAt := committedAt.Add(time.Second)
	first, claimed, err := store.Claim(ctx, state.ClaimRequest{
		Owner: "control-proof-first", Now: firstAt, LeaseDuration: crashLeaseProofDuration,
	})
	if err != nil || !claimed || first.JobID != jobID {
		return fmt.Errorf("claim control-proof delegation: claimed=%t job=%s: %w", claimed, first.JobID, err)
	}
	if err := store.RecordMatrixEvent(ctx, state.MatrixEventRequest{
		Lease: first.LeaseToken(), At: firstAt.Add(time.Second),
		Stage: state.MatrixEventPlaceholder, EventID: placeholderID,
	}); err != nil {
		return fmt.Errorf("record control-proof placeholder: %w", err)
	}
	if err := store.ScheduleRetry(ctx, state.RetryRequest{
		Lease: first.LeaseToken(), At: firstAt.Add(2 * time.Second),
		NextAttemptAt: firstAt.Add(time.Hour), ErrorCode: "control_proof_wait", Kind: state.RetryPoll,
	}); err != nil {
		return fmt.Errorf("release control-proof initial lease: %w", err)
	}
	controlAt := firstAt.Add(3 * time.Second)
	controlResult, err := store.AdmitTransaction(ctx, state.TransactionAdmission{
		TransactionID:   "control-intent-proof-" + runID,
		BodyHash:        state.HashTransaction([]byte("control-intent-proof-" + runID)),
		CommittedAt:     controlAt,
		ControlCapacity: 8,
		RoomCapacity:    crashLeaseProofCapacity, GlobalCapacity: crashLeaseProofCapacity,
		Controls: []state.NewControl{{
			TargetMatrixEventID: placeholderID, SourceMatrixEventID: "$control-cancel-" + runID,
			RoomID: roomID, SenderMXID: "@control-proof:integration.test",
			Kind: state.ControlCancel, Authorized: true,
		}},
	})
	if err != nil || len(controlResult.InsertedControlIDs) != 1 {
		return fmt.Errorf("admit control-proof intent: result=%+v: %w", controlResult, err)
	}
	controlID := controlResult.InsertedControlIDs[0]
	owner, claimed, err := store.Claim(ctx, state.ClaimRequest{
		Owner: "control-proof-owner", Now: controlAt.Add(time.Second), LeaseDuration: crashLeaseProofDuration,
	})
	if err != nil || !claimed || owner.JobID != jobID {
		return fmt.Errorf("claim control-proof owner: claimed=%t job=%s: %w", claimed, owner.JobID, err)
	}
	start := make(chan struct{})
	claims := make(chan crashControlClaimResult, 2)
	for range 2 {
		go func() {
			<-start
			control, ok, claimErr := store.ClaimControl(ctx, owner.LeaseToken(), controlAt.Add(2*time.Second))
			claims <- crashControlClaimResult{control: control, claimed: ok, err: claimErr}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		claim := <-claims
		if claim.err != nil {
			return fmt.Errorf("race control-proof claims: %w", claim.err)
		}
		if claim.claimed {
			winners++
			if claim.control.ControlID != controlID || claim.control.State != state.ControlPrepared {
				return fmt.Errorf("control-proof winner returned unexpected control %+v", claim.control)
			}
		}
	}
	if winners != 1 {
		return fmt.Errorf("concurrent control-proof claims produced %d winners, want one", winners)
	}
	takeoverAt := owner.LeaseExpiresAt.Add(time.Second)
	takeover, claimed, err := store.Claim(ctx, state.ClaimRequest{
		Owner: "control-proof-takeover", Now: takeoverAt, LeaseDuration: crashLeaseProofDuration,
	})
	if err != nil || !claimed || takeover.JobID != jobID {
		return fmt.Errorf("claim control-proof takeover: claimed=%t job=%s: %w", claimed, takeover.JobID, err)
	}
	recovered, claimed, err := store.ClaimControl(ctx, takeover.LeaseToken(), takeoverAt.Add(time.Second))
	if err != nil || !claimed || recovered.ControlID != controlID || recovered.RecoveryCount != 1 {
		return fmt.Errorf("recover prepared control: control=%+v claimed=%t: %w", recovered, claimed, err)
	}
	staleErr := store.TransitionControl(ctx, state.ControlTransitionRequest{
		Lease: owner.LeaseToken(), ControlID: controlID, From: state.ControlPrepared,
		To: state.ControlApplied, At: takeoverAt.Add(2 * time.Second),
	})
	if staleErr == nil {
		return errors.New("stale control-proof transition unexpectedly succeeded")
	}
	if !errors.Is(staleErr, state.ErrLeaseLost) {
		return fmt.Errorf("stale control-proof transition did not return ErrLeaseLost: %w", staleErr)
	}
	controlCode := "control_ack_ambiguous"
	if err := store.TransitionControl(ctx, state.ControlTransitionRequest{
		Lease: takeover.LeaseToken(), ControlID: controlID, From: state.ControlPrepared,
		To: state.ControlAmbiguous, At: takeoverAt.Add(2 * time.Second),
		Patch: state.ControlTransitionPatch{ErrorCode: &controlCode},
	}); err != nil {
		return fmt.Errorf("finish recovered control proof: %w", err)
	}
	controls, err := store.Controls(ctx, jobID)
	if err != nil || len(controls) != 1 || controls[0].State != state.ControlAmbiguous ||
		controls[0].RecoveryCount != 1 || len(controls[0].Payload) != 0 {
		return fmt.Errorf("persisted control-proof evidence = %+v: %w", controls, err)
	}
	terminalCode := "control_proof_complete"
	if err := store.Transition(ctx, state.TransitionRequest{
		Lease: takeover.LeaseToken(), From: state.StatePending, To: state.StateDenied,
		At: takeoverAt.Add(3 * time.Second), Patch: state.TransitionPatch{ErrorCode: &terminalCode},
	}); err != nil {
		return fmt.Errorf("terminalize control-proof delegation: %w", err)
	}
	slog.Info(
		"postgres control fencing scenario passed",
		"control_fencing", "passed",
		"concurrent_claim_winners", winners,
		"stale_transition", "rejected",
		"recovery_count", recovered.RecoveryCount,
	)
	return nil
}

func racePostgresClaims(
	ctx context.Context,
	store state.Ledger,
	jobID string,
	at time.Time,
) (state.Job, error) {
	owners := [...]string{"lease-proof-racer-a", "lease-proof-racer-b"}
	ready := make(chan struct{}, len(owners))
	start := make(chan struct{})
	results := make(chan crashClaimResult, len(owners))
	for _, owner := range owners {
		go func() {
			ready <- struct{}{}
			<-start
			job, claimed, err := store.Claim(ctx, state.ClaimRequest{
				Owner:         owner,
				Now:           at,
				LeaseDuration: crashLeaseProofDuration,
			})
			results <- crashClaimResult{job: job, claimed: claimed, err: err}
		}()
	}
	for range owners {
		<-ready
	}
	close(start)

	winners := make([]state.Job, 0, 1)
	for range owners {
		result := <-results
		if result.err != nil {
			return state.Job{}, fmt.Errorf("race lease-proof claim: %w", result.err)
		}
		if result.claimed {
			winners = append(winners, result.job)
		}
	}
	if len(winners) != 1 || winners[0].JobID != jobID || winners[0].LeaseGeneration != 1 ||
		winners[0].LeaseOwner == "" || !winners[0].LeaseExpiresAt.Equal(at.Add(crashLeaseProofDuration)) {
		return state.Job{}, fmt.Errorf("concurrent lease-proof claims produced %d valid winners, want one", len(winners))
	}
	return winners[0], nil
}

func requirePersistedLease(ctx context.Context, store state.Ledger, want state.Job) error {
	persisted, found, err := store.Job(ctx, want.JobID)
	if err != nil {
		return fmt.Errorf("load replacement lease-proof delegation: %w", err)
	}
	if !found || persisted.State != state.StatePending || persisted.LeaseOwner != want.LeaseOwner ||
		persisted.LeaseGeneration != want.LeaseGeneration || !persisted.LeaseExpiresAt.Equal(want.LeaseExpiresAt) {
		return fmt.Errorf("stale transition changed the persisted replacement lease")
	}
	return nil
}

func (f fixture) provePreAckRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[0]
	event, err := crashEvent(roomID, sess.UserID, "$crash-pre-ack:integration.test", crashContent(ghost, phase))
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "postgres-ledger-commit")
	if err != nil {
		return err
	}
	requestDone := make(chan error, 1)
	go func() {
		_, requestErr := f.pushCrashTransaction(ctx, "crash-pre-ack", event)
		requestDone <- requestErr
	}()
	if err := f.waitForFault(ctx, "postgres-ledger-commit"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if requestErr := <-requestDone; requestErr == nil {
		return fmt.Errorf("%s appservice request unexpectedly received a response", phase)
	}
	status, err := f.pushCrashTransaction(ctx, "crash-pre-ack", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("replay exact pre-ACK transaction: status=%d: %w", status, err)
	}
	changed := event
	changed.OriginServerTS++
	status, err = f.pushCrashTransaction(ctx, "crash-pre-ack", changed)
	if err != nil {
		return fmt.Errorf("push changed pre-ACK transaction: %w", err)
	}
	if status != http.StatusConflict {
		return fmt.Errorf("changed transaction status = %d, want %d", status, http.StatusConflict)
	}
	return f.requireStableReply(ctx, sess.AccessToken, roomID, ghost, event.EventID, replyText)
}

func (f fixture) provePreClaimRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[1]
	event, err := crashEvent(roomID, sess.UserID, "$crash-pre-claim:integration.test", crashContent(ghost, phase))
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "postgres-claim")
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-pre-claim", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push pre-claim transaction: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "postgres-claim"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	return f.requireStableReply(ctx, sess.AccessToken, roomID, ghost, event.EventID, replyText)
}

func (f fixture) proveAmbiguousA2ARecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[2]
	before, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	event, err := crashEvent(roomID, sess.UserID, "$crash-a2a:integration.test", crashContent(ghost, phase))
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "a2a-response")
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-a2a", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push A2A-response transaction: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "a2a-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.requireStableReply(
		ctx,
		sess.AccessToken,
		roomID,
		ghost,
		event.EventID,
		crashAmbiguousReply,
	); err != nil {
		return err
	}
	after, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if after.TotalRequests != before.TotalRequests+1 {
		return fmt.Errorf("ambiguous A2A requests = %d, want exactly one after baseline %d", after.TotalRequests, before.TotalRequests)
	}
	state, err := f.faultState(ctx)
	if err != nil {
		return err
	}
	if countA2AMethod(state.A2AMethods, "SendMessage") != 1 {
		return fmt.Errorf("ambiguous A2A methods = %v, want one SendMessage", state.A2AMethods)
	}
	return nil
}

func (f fixture) proveControlIntentRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[3]
	placeholderID, err := f.startCrashInputTask(ctx, sess, roomID, ghost, "control-intent", 1)
	if err != nil {
		return err
	}
	before, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "postgres-claim")
	if err != nil {
		return err
	}
	cancelEvent, err := crashCancelEvent(
		roomID, sess.UserID, "$crash-control-intent-cancel:integration.test", placeholderID,
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-control-intent-cancel", cancelEvent)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push pre-claim control: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "postgres-claim"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.waitForEditContaining(ctx, sess.AccessToken, roomID, ghost, placeholderID, "canceled by"); err != nil {
		return err
	}
	after, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if after.CancelRequests != before.CancelRequests+1 {
		return fmt.Errorf("pre-claim recovered cancellation requests = %d, want one after %d",
			after.CancelRequests, before.CancelRequests)
	}
	return nil
}

func (f fixture) proveCancelControlRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[4]
	placeholderID, err := f.startCrashInputTask(ctx, sess, roomID, ghost, "cancel-control-task", 2)
	if err != nil {
		return err
	}
	before, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "a2a-response")
	if err != nil {
		return err
	}
	cancelEvent, err := crashCancelEvent(
		roomID, sess.UserID, "$crash-cancel-control:integration.test", placeholderID,
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-cancel-control", cancelEvent)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push ambiguous cancel control: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "a2a-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.waitForEditContaining(ctx, sess.AccessToken, roomID, ghost, placeholderID, "cancel requested by"); err != nil {
		return err
	}
	after, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if after.CancelRequests != before.CancelRequests+1 {
		return fmt.Errorf("ambiguous cancellation requests = %d, want one after %d",
			after.CancelRequests, before.CancelRequests)
	}
	state, err := f.faultState(ctx)
	if err != nil {
		return err
	}
	if countA2AMethod(state.A2AMethods, "CancelTask") != 1 {
		return fmt.Errorf("ambiguous cancellation methods = %v, want one CancelTask", state.A2AMethods)
	}
	return nil
}

func (f fixture) proveContinuationControlRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[5]
	placeholderID, err := f.startCrashInputTask(ctx, sess, roomID, ghost, "continuation-control-task", 3)
	if err != nil {
		return err
	}
	before, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "a2a-response")
	if err != nil {
		return err
	}
	answerEvent, err := crashThreadEvent(
		roomID, sess.UserID, "$crash-continuation-control:integration.test", placeholderID, "kube-system",
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-continuation-control", answerEvent)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push ambiguous continuation control: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "a2a-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.waitForEditContaining(ctx, sess.AccessToken, roomID, ghost, placeholderID, "continuation requested by"); err != nil {
		return err
	}
	after, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if after.TotalRequests != before.TotalRequests+1 || after.InputContinued != before.InputContinued+1 {
		return fmt.Errorf("ambiguous continuation requests/inputs = %d/%d, want one after %d/%d",
			after.TotalRequests, after.InputContinued, before.TotalRequests, before.InputContinued)
	}
	state, err := f.faultState(ctx)
	if err != nil {
		return err
	}
	if countA2AMethod(state.A2AMethods, "SendMessage") != 1 {
		return fmt.Errorf("ambiguous continuation methods = %v, want one SendMessage", state.A2AMethods)
	}
	return nil
}

func (f fixture) proveQuestionProjectionRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[6]
	baseline, err := f.prepareCrashBoundary(ctx, "matrix-question-response")
	if err != nil {
		return err
	}
	event, err := crashEvent(
		roomID, sess.UserID, "$crash-question-control:integration.test",
		messageContent{Body: ghost + " input room=97 seq=04", Mentions: mentions{UserIDs: []string{ghost}}, MsgType: "m.text"},
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-question-control", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push question projection task: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "matrix-question-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	placeholderID, err := f.waitForCrashPlaceholder(ctx, sess.AccessToken, roomID, ghost, event.EventID)
	if err != nil {
		return err
	}
	if err := f.waitForEditContaining(ctx, sess.AccessToken, roomID, ghost, placeholderID, "which namespace?"); err != nil {
		return err
	}
	if err := f.requireFirstDeterministicMatrixReplay(ctx); err != nil {
		return err
	}
	return f.cancelCrashTask(ctx, sess, roomID, ghost, placeholderID, "question-control-cleanup")
}

func (f fixture) proveProgressProjectionRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[7]
	baseline, err := f.prepareCrashBoundary(ctx, "matrix-progress-response")
	if err != nil {
		return err
	}
	event, err := crashEvent(
		roomID, sess.UserID, "$crash-progress-control:integration.test",
		messageContent{Body: ghost + " long room=96 seq=02", Mentions: mentions{UserIDs: []string{ghost}}, MsgType: "m.text"},
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-progress-control", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push progress projection task: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "matrix-progress-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	placeholderID, err := f.waitForCrashPlaceholder(ctx, sess.AccessToken, roomID, ghost, event.EventID)
	if err != nil {
		return err
	}
	if err := f.waitForThreadContaining(ctx, sess.AccessToken, roomID, ghost, placeholderID, "working"); err != nil {
		return err
	}
	if err := f.requireFirstDeterministicMatrixReplay(ctx); err != nil {
		return err
	}
	return f.cancelCrashTask(ctx, sess, roomID, ghost, placeholderID, "progress-control-cleanup")
}

func (f fixture) provePinProjectionRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[8]
	baseline, err := f.prepareCrashBoundary(ctx, "matrix-pin-response")
	if err != nil {
		return err
	}
	event, err := crashEvent(
		roomID, sess.UserID, "$crash-pin-control:integration.test",
		messageContent{Body: ghost + " long room=96 seq=03", Mentions: mentions{UserIDs: []string{ghost}}, MsgType: "m.text"},
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-pin-control", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push pin projection task: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "matrix-pin-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	placeholderID, err := f.waitForCrashPlaceholder(ctx, sess.AccessToken, roomID, ghost, event.EventID)
	if err != nil {
		return err
	}
	if err := f.waitForPinned(ctx, sess.AccessToken, roomID, placeholderID, true); err != nil {
		return err
	}
	if err := f.requireConvergentMatrixProjection(ctx); err != nil {
		return err
	}
	if err := f.cancelCrashTask(ctx, sess, roomID, ghost, placeholderID, "pin-control-cleanup"); err != nil {
		return err
	}
	return f.waitForPinned(ctx, sess.AccessToken, roomID, placeholderID, false)
}

func (f fixture) provePreMatrixRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[9]
	event, err := crashEvent(roomID, sess.UserID, "$crash-pre-matrix:integration.test", crashContent(ghost, phase))
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "matrix-request")
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-pre-matrix", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push pre-Matrix transaction: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "matrix-request"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.requireStableReply(ctx, sess.AccessToken, roomID, ghost, event.EventID, replyText); err != nil {
		return err
	}
	return f.requireDeterministicMatrixReplay(ctx)
}

func (f fixture) proveLostMatrixResponse(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[10]
	event, err := crashEvent(roomID, sess.UserID, "$crash-matrix-response:integration.test", crashContent(ghost, phase))
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "matrix-response")
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-matrix-response", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push Matrix-response transaction: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "matrix-response"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.requireStableReply(ctx, sess.AccessToken, roomID, ghost, event.EventID, replyText); err != nil {
		return err
	}
	return f.requireDeterministicMatrixReplay(ctx)
}

func (f fixture) proveLongTaskRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[11]
	before, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	event, err := crashEvent(
		roomID,
		sess.UserID,
		"$crash-long-task:integration.test",
		messageContent{Body: ghost + " long room=98 seq=01 crash boundary " + phase, Mentions: mentions{UserIDs: []string{ghost}}, MsgType: "m.text"},
	)
	if err != nil {
		return err
	}
	baseline, err := f.prepareCrashBoundary(ctx, "a2a-task-poll")
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-long-task", event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push long-task transaction: status=%d: %w", status, err)
	}
	if err := f.waitForFault(ctx, "a2a-task-poll"); err != nil {
		return err
	}
	if err := f.crossCrashBoundary(ctx, phase, baseline); err != nil {
		return err
	}
	if err := f.releaseLongTask(ctx); err != nil {
		return err
	}
	if err := f.waitForLongProjection(ctx, sess.AccessToken, roomID, ghost, event.EventID); err != nil {
		return err
	}
	state, err := f.faultState(ctx)
	if err != nil {
		return err
	}
	if countA2AMethod(state.A2AMethods, "SendMessage") != 1 || countA2AMethod(state.A2AMethods, "GetTask") < 2 {
		return fmt.Errorf("long-task A2A methods = %v, want one SendMessage and recovered GetTask", state.A2AMethods)
	}
	stats, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if stats.LongStarted != before.LongStarted+1 || stats.LongCompleted != before.LongCompleted+1 {
		return fmt.Errorf("long task started/completed = %d/%d, want one after %d/%d",
			stats.LongStarted, stats.LongCompleted, before.LongStarted, before.LongCompleted)
	}
	return nil
}

func (f fixture) startCrashInputTask(
	ctx context.Context,
	sess session,
	roomID, ghost, name string,
	sequence int,
) (string, error) {
	event, err := crashEvent(
		roomID,
		sess.UserID,
		"$crash-"+name+":integration.test",
		messageContent{
			Body:     ghost + fmt.Sprintf(" input room=97 seq=%02d", sequence),
			Mentions: mentions{UserIDs: []string{ghost}}, MsgType: "m.text",
		},
	)
	if err != nil {
		return "", err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-"+name, event)
	if err != nil || status != http.StatusOK {
		return "", fmt.Errorf("push %s input task: status=%d: %w", name, status, err)
	}
	placeholderID, err := f.waitForCrashPlaceholder(ctx, sess.AccessToken, roomID, ghost, event.EventID)
	if err != nil {
		return "", err
	}
	if err := f.waitForEditContaining(
		ctx, sess.AccessToken, roomID, ghost, placeholderID, "which namespace?",
	); err != nil {
		return "", err
	}
	return placeholderID, nil
}

func (f fixture) cancelCrashTask(
	ctx context.Context,
	sess session,
	roomID, ghost, placeholderID, name string,
) error {
	event, err := crashCancelEvent(
		roomID, sess.UserID, "$crash-"+name+":integration.test", placeholderID,
	)
	if err != nil {
		return err
	}
	status, err := f.pushCrashTransaction(ctx, "crash-"+name, event)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("push %s cancellation: status=%d: %w", name, status, err)
	}
	return f.waitForEditContaining(ctx, sess.AccessToken, roomID, ghost, placeholderID, "canceled by")
}

func crashCancelEvent(roomID, sender, eventID, targetEventID string) (matrixEvent, error) {
	content, err := json.Marshal(map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": "m.annotation", "event_id": targetEventID, "key": "❌",
		},
	})
	if err != nil {
		return matrixEvent{}, fmt.Errorf("encode crash cancel event: %w", err)
	}
	return matrixEvent{
		Content: content, EventID: eventID, OriginServerTS: 1_700_000_000_100,
		RoomID: roomID, Sender: sender, Type: "m.reaction",
	}, nil
}

func crashThreadEvent(roomID, sender, eventID, targetEventID, body string) (matrixEvent, error) {
	content, err := json.Marshal(map[string]any{
		"msgtype": "m.text", "body": body,
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": targetEventID},
	})
	if err != nil {
		return matrixEvent{}, fmt.Errorf("encode crash thread event: %w", err)
	}
	return matrixEvent{
		Content: content, EventID: eventID, OriginServerTS: 1_700_000_000_200,
		RoomID: roomID, Sender: sender, Type: "m.room.message",
	}, nil
}

func (f fixture) waitForCrashPlaceholder(
	ctx context.Context,
	token, roomID, ghost, originalEventID string,
) (string, error) {
	for {
		events, err := f.roomMessages(ctx, token, roomID)
		if err == nil {
			for _, evt := range events {
				if evt.Type != "m.room.message" || evt.Sender != ghost {
					continue
				}
				var content messageContent
				if json.Unmarshal(evt.Content, &content) == nil && content.Body == crashWorkingText &&
					content.RelatesTo.InReplyTo.EventID == originalEventID {
					return evt.EventID, nil
				}
			}
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return "", errors.Join(err, waitErr)
			}
			return "", fmt.Errorf("wait for crash placeholder: %w", waitErr)
		}
	}
}

func (f fixture) waitForEditContaining(
	ctx context.Context,
	token, roomID, ghost, placeholderID, fragment string,
) error {
	return f.waitForRelatedControl(ctx, token, roomID, ghost, placeholderID, "m.replace", fragment, 1)
}

func (f fixture) waitForThreadContaining(
	ctx context.Context,
	token, roomID, ghost, placeholderID, fragment string,
) error {
	return f.waitForRelatedControl(ctx, token, roomID, ghost, placeholderID, "m.thread", fragment, 2)
}

func (f fixture) waitForRelatedControl(
	ctx context.Context,
	token, roomID, ghost, placeholderID, relation, fragment string,
	maximum int,
) error {
	for {
		events, err := f.roomMessages(ctx, token, roomID)
		matches := 0
		if err == nil {
			for _, evt := range events {
				if evt.Type != "m.room.message" || evt.Sender != ghost {
					continue
				}
				var content struct {
					Body      string `json:"body"`
					RelatesTo struct {
						RelType string `json:"rel_type"`
						EventID string `json:"event_id"`
					} `json:"m.relates_to"`
					NewContent struct {
						Body string `json:"body"`
					} `json:"m.new_content"`
				}
				if decodeErr := json.Unmarshal(evt.Content, &content); decodeErr != nil {
					return fmt.Errorf("decode durable control Matrix event %s: %w", evt.EventID, decodeErr)
				}
				body := content.Body
				if relation == "m.replace" {
					body = content.NewContent.Body
				}
				if content.RelatesTo.RelType == relation && content.RelatesTo.EventID == placeholderID &&
					strings.Contains(body, fragment) {
					matches++
				}
			}
			if matches >= 1 && matches <= maximum {
				return nil
			}
			if matches > maximum {
				return fmt.Errorf("durable %s projections containing %q = %d, want at most %d",
					relation, fragment, matches, maximum)
			}
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for durable %s projection containing %q: %w", relation, fragment, waitErr)
		}
	}
}

func (f fixture) waitForPinned(
	ctx context.Context,
	token, roomID, eventID string,
	want bool,
) error {
	endpoint := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/state/m.room.pinned_events",
		f.matrixURL,
		pathSegment(roomID),
	)
	for {
		status, body, err := f.request(ctx, http.MethodGet, endpoint, token, nil)
		if err == nil && status == http.StatusOK {
			var content struct {
				Pinned []string `json:"pinned"`
			}
			if decodeErr := json.Unmarshal(body, &content); decodeErr != nil {
				return fmt.Errorf("decode pinned-events state: %w", decodeErr)
			}
			found := false
			for _, pinned := range content.Pinned {
				if pinned == eventID {
					found = true
					break
				}
			}
			if found == want {
				return nil
			}
		}
		if err == nil && status == http.StatusNotFound && !want {
			return nil
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for pinned=%t on %s (status %d): %w", want, eventID, status, waitErr)
		}
	}
}

func (f fixture) prepareCrashBoundary(ctx context.Context, mode string) (bridgeMetrics, error) {
	baseline, err := f.fetchBridgeMetrics(ctx)
	if err != nil {
		return bridgeMetrics{}, fmt.Errorf("read bridge metrics before %s: %w", mode, err)
	}
	status, body, err := f.request(ctx, http.MethodPost, f.faultURL+"/arm/"+mode, "", nil)
	if err != nil {
		return bridgeMetrics{}, fmt.Errorf("arm %s fault: %w", mode, err)
	}
	if status != http.StatusNoContent {
		return bridgeMetrics{}, fmt.Errorf("arm %s fault: status %d: %s", mode, status, body)
	}
	return baseline, nil
}

func (f fixture) waitForFault(ctx context.Context, mode string) error {
	for {
		state, err := f.faultState(ctx)
		if err == nil && state.Mode == mode && state.Tripped {
			return nil
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for fault %s (state=%+v): %w", mode, state, waitErr)
		}
	}
}

func (f fixture) faultState(ctx context.Context) (crashFaultState, error) {
	status, body, err := f.request(ctx, http.MethodGet, f.faultURL+"/state", "", nil)
	if err != nil {
		return crashFaultState{}, fmt.Errorf("read fault state: %w", err)
	}
	if status != http.StatusOK {
		return crashFaultState{}, fmt.Errorf("read fault state: status %d: %s", status, body)
	}
	var state crashFaultState
	if err := json.Unmarshal(body, &state); err != nil {
		return crashFaultState{}, fmt.Errorf("decode fault state: %w", err)
	}
	return state, nil
}

func (f fixture) crossCrashBoundary(ctx context.Context, phase string, baseline bridgeMetrics) error {
	slog.Info(
		"bridge hard-crash boundary ready",
		"crash_action", "sigkill_ready",
		"crash_phase", phase,
		"process_start_time_seconds", baseline.ProcessStartTime,
	)
	replacement, err := f.waitForCrashReplacement(ctx, baseline.ProcessStartTime)
	if err != nil {
		return fmt.Errorf("recover from %s: %w", phase, err)
	}
	slog.Info(
		"bridge hard-crash boundary recovered",
		"crash_action", "recovered",
		"crash_phase", phase,
		"process_start_time_seconds", replacement.ProcessStartTime,
	)
	return nil
}

func (f fixture) waitForCrashReplacement(ctx context.Context, original float64) (bridgeMetrics, error) {
	for {
		metrics, err := f.fetchBridgeMetrics(ctx)
		if err == nil && metrics.ProcessStartTime != original {
			return metrics, nil
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return bridgeMetrics{}, errors.Join(err, waitErr)
			}
			return bridgeMetrics{}, fmt.Errorf("wait for replacement process after %.0f: %w", original, waitErr)
		}
	}
}

func crashContent(ghost, phase string) messageContent {
	return messageContent{
		Body:     ghost + " crash boundary " + phase,
		Mentions: mentions{UserIDs: []string{ghost}},
		MsgType:  "m.text",
	}
}

func crashEvent(roomID, sender, eventID string, content messageContent) (matrixEvent, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return matrixEvent{}, fmt.Errorf("encode crash event %s: %w", eventID, err)
	}
	return matrixEvent{
		Content:        encoded,
		EventID:        eventID,
		OriginServerTS: 1_700_000_000_000,
		RoomID:         roomID,
		Sender:         sender,
		Type:           "m.room.message",
	}, nil
}

func (f fixture) pushCrashTransaction(
	ctx context.Context,
	transactionID string,
	event matrixEvent,
) (int, error) {
	status, _, err := f.putAppserviceTransaction(ctx, transactionID, []matrixEvent{event})
	return status, err
}

func (f fixture) requireStableReply(
	ctx context.Context,
	token, roomID, ghost, eventID, body string,
) error {
	if err := f.waitForReply(ctx, token, roomID, ghost, eventID, body); err != nil {
		return err
	}
	deadline := time.Now().Add(crashReplyQuietTime)
	for time.Now().Before(deadline) {
		if err := f.assertReplyCount(ctx, token, roomID, ghost, eventID, body, 1); err != nil {
			return err
		}
		if err := wait(ctx, crashPollInterval); err != nil {
			return err
		}
	}
	return nil
}

func (f fixture) requireDeterministicMatrixReplay(ctx context.Context) error {
	for {
		state, err := f.faultState(ctx)
		if err == nil && len(state.MatrixPaths) >= 2 {
			want := state.MatrixPaths[0]
			for _, path := range state.MatrixPaths[1:] {
				if path != want {
					return fmt.Errorf("matrix replay paths = %v, want one deterministic transaction path", state.MatrixPaths)
				}
			}
			return nil
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for deterministic Matrix replay: %w", waitErr)
		}
	}
}

func (f fixture) requireFirstDeterministicMatrixReplay(ctx context.Context) error {
	for {
		state, err := f.faultState(ctx)
		if err == nil && len(state.MatrixPaths) >= 2 {
			if state.MatrixPaths[0] != state.MatrixPaths[1] {
				return fmt.Errorf("first Matrix replay paths = %v, want the accepted path twice", state.MatrixPaths)
			}
			return nil
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for first deterministic Matrix replay: %w", waitErr)
		}
	}
}

func (f fixture) requireConvergentMatrixProjection(ctx context.Context) error {
	for {
		state, err := f.faultState(ctx)
		if err == nil && len(state.MatrixPaths) >= 1 {
			want := state.MatrixPaths[0]
			for _, path := range state.MatrixPaths[1:] {
				if path != want {
					return fmt.Errorf("matrix convergence paths = %v, want one stable projection path", state.MatrixPaths)
				}
			}
			return nil
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for convergent Matrix projection: %w", waitErr)
		}
	}
}

func (f fixture) releaseLongTask(ctx context.Context) error {
	status, body, err := f.request(ctx, http.MethodPost, f.stubURL+"/control/release-long", "", nil)
	if err != nil {
		return fmt.Errorf("release long task: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("release long task: status %d: %s", status, body)
	}
	return nil
}

func (f fixture) waitForLongProjection(
	ctx context.Context,
	token, roomID, ghost, originalEventID string,
) error {
	for {
		placeholderCount, editCount, err := f.longProjectionCounts(ctx, token, roomID, ghost, originalEventID)
		if err == nil && placeholderCount == 1 && editCount == 1 {
			return nil
		}
		if err == nil && (placeholderCount > 1 || editCount > 1) {
			return fmt.Errorf("long task placeholder/edit counts = %d/%d, want 1/1", placeholderCount, editCount)
		}
		if waitErr := wait(ctx, crashPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for long task projection (placeholder=%d edit=%d): %w", placeholderCount, editCount, waitErr)
		}
	}
}

func (f fixture) longProjectionCounts(
	ctx context.Context,
	token, roomID, ghost, originalEventID string,
) (int, int, error) {
	events, err := f.roomMessages(ctx, token, roomID)
	if err != nil {
		return 0, 0, err
	}
	placeholderIDs := make(map[string]struct{})
	for _, event := range events {
		if event.Sender != ghost || event.Type != "m.room.message" {
			continue
		}
		var content struct {
			Body      string `json:"body"`
			RelatesTo struct {
				RelType   string `json:"rel_type"`
				EventID   string `json:"event_id"`
				InReplyTo struct {
					EventID string `json:"event_id"`
				} `json:"m.in_reply_to"`
			} `json:"m.relates_to"`
			NewContent struct {
				Body string `json:"body"`
			} `json:"m.new_content"`
		}
		if err := json.Unmarshal(event.Content, &content); err != nil {
			return 0, 0, fmt.Errorf("decode long-task Matrix event %s: %w", event.EventID, err)
		}
		if content.Body == crashWorkingText && content.RelatesTo.InReplyTo.EventID == originalEventID {
			placeholderIDs[event.EventID] = struct{}{}
		}
	}
	edits := 0
	for _, event := range events {
		if event.Sender != ghost || event.Type != "m.room.message" {
			continue
		}
		var content struct {
			RelatesTo struct {
				RelType string `json:"rel_type"`
				EventID string `json:"event_id"`
			} `json:"m.relates_to"`
			NewContent struct {
				Body string `json:"body"`
			} `json:"m.new_content"`
		}
		if err := json.Unmarshal(event.Content, &content); err != nil {
			return 0, 0, fmt.Errorf("decode long-task Matrix edit %s: %w", event.EventID, err)
		}
		_, replacesPlaceholder := placeholderIDs[content.RelatesTo.EventID]
		if content.RelatesTo.RelType == "m.replace" && replacesPlaceholder && content.NewContent.Body == crashLongReply {
			edits++
		}
	}
	return len(placeholderIDs), edits, nil
}

func countA2AMethod(methods []string, want string) int {
	count := 0
	for _, method := range methods {
		if method == want {
			count++
		}
	}
	return count
}
