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
	crashAmbiguousReply     = "⚠️ agent \"agent-integration\" may have received this request, but its acknowledgement was lost; the bridge did not resend it."
)

var crashPhases = [...]string{
	"ledger_committed_pre_ack",
	"acknowledged_pre_claim",
	"a2a_accepted_pre_record",
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
	roomID, err := f.createRoom(ctx, sess.AccessToken)
	if err != nil {
		return err
	}
	ghost := "@" + ghostLocalpart + ":" + f.server
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

func (f fixture) provePreMatrixRecovery(
	ctx context.Context,
	sess session,
	roomID, ghost string,
) error {
	phase := crashPhases[3]
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
	phase := crashPhases[4]
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
	phase := crashPhases[5]
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
	if stats.LongStarted != 1 || stats.LongCompleted != 1 {
		return fmt.Errorf("long task started/completed = %d/%d, want 1/1", stats.LongStarted, stats.LongCompleted)
	}
	return nil
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
