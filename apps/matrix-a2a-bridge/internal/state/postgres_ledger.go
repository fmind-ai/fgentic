package state

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var jobColumnNames = []string{
	"job_id",
	"matrix_event_id",
	"ghost_mxid",
	"ghost_localpart",
	"appservice_transaction_id",
	"room_id",
	"intake_sequence",
	"sender_mxid",
	"sender_origin_kind",
	"sender_origin_network",
	"matrix_origin_server_ts",
	"target_fingerprint",
	"intake_fingerprint",
	"prompt",
	"payload",
	"state",
	"lease_owner",
	"lease_generation",
	"lease_expires_at",
	"attempt_count",
	"poll_count",
	"next_attempt_at",
	"error_code",
	"admission_checked",
	"admission_allowed",
	"admission_reason",
	"a2a_message_id",
	"a2a_task_id",
	"a2a_context_id",
	"result_text",
	"matrix_reply_txn_id",
	"matrix_placeholder_txn_id",
	"matrix_edit_txn_id",
	"matrix_reply_event_id",
	"matrix_placeholder_event_id",
	"matrix_edit_event_id",
	"matrix_dead_man_delay_id",
	"task_deadline_at",
	"input_wait_started_at",
	"input_wait_expires_at",
	"created_at",
	"updated_at",
	"terminal_at",
}

var controlColumnNames = []string{
	"control_id", "job_id", "appservice_transaction_id", "source_matrix_event_id", "intake_fingerprint",
	"authorized_sender", "kind", "state",
	"slot", "lease_generation", "recovery_count", "payload", "a2a_message_id", "matrix_txn_id", "matrix_event_id",
	"error_code", "prepared_at", "created_at", "updated_at", "terminal_at",
}

const (
	// admissionCapacityLockKey is the database-local, transaction-scoped namespace for serializing
	// durable backlog counts with inserts. The hexadecimal value is ASCII "fgentic".
	admissionCapacityLockKey  int64 = 0x6667656e746963
	admissionCapacityLockSQL        = `SELECT pg_advisory_xact_lock($1)`
	admissionCapacityCountSQL       = `
		SELECT room_id, COUNT(*)
		FROM bridge_delegations
		WHERE terminal_at IS NULL
		GROUP BY room_id`
	planControlParentSQL = `SELECT %s FROM bridge_delegations WHERE job_id = $1 FOR UPDATE`
)

// Claim uses one locking statement so concurrent workers cannot observe or claim the same row. The
// NOT EXISTS guard keeps a delayed or leased earlier room job ahead of every later room sequence.
const claimJobSQL = `
	WITH candidate AS (
		SELECT candidate_job.job_id
		FROM bridge_delegations AS candidate_job
		WHERE candidate_job.terminal_at IS NULL
		  AND candidate_job.next_attempt_at <= $2
		  AND (candidate_job.lease_expires_at IS NULL OR candidate_job.lease_expires_at <= $2)
		  AND NOT EXISTS (
			SELECT 1
			FROM bridge_delegations AS earlier
			WHERE earlier.room_id = candidate_job.room_id
			  AND earlier.intake_sequence < candidate_job.intake_sequence
			  AND earlier.terminal_at IS NULL
		  )
		ORDER BY candidate_job.intake_sequence
		FOR UPDATE OF candidate_job SKIP LOCKED
		LIMIT 1
	)
	UPDATE bridge_delegations AS claimed
	SET lease_owner = $1,
		lease_generation = claimed.lease_generation + 1,
		lease_expires_at = $3,
		updated_at = $2
	FROM candidate
	WHERE claimed.job_id = candidate.job_id
	RETURNING %s`

// AdmitTransaction implements Ledger with the transaction row and every new delegation in one
// database transaction. Exact transaction replays never re-run job insertion.
func (p *Postgres) AdmitTransaction(ctx context.Context, admission TransactionAdmission) (AdmissionResult, error) {
	if err := validateAdmission(admission); err != nil {
		return AdmissionResult{}, err
	}
	result := AdmissionResult{Disposition: TransactionAccepted}
	err := p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		if _, err := p.db.Exec(txCtx, admissionCapacityLockSQL, admissionCapacityLockKey); err != nil {
			return fmt.Errorf("lock durable admission capacity: %w", err)
		}
		inserted, err := insertAppserviceTransaction(txCtx, p, admission)
		if err != nil {
			return err
		}
		if !inserted {
			result.Disposition = TransactionReplay
			return nil
		}
		capacity, err := loadAdmissionCapacity(txCtx, p, admission.RoomCapacity, admission.GlobalCapacity)
		if err != nil {
			return err
		}
		for _, delegation := range admission.Delegations {
			denial := capacity.denialReason(delegation.RoomID)
			jobID, status, err := insertDelegation(txCtx, p, admission, delegation, denial)
			if err != nil {
				return err
			}
			switch status {
			case delegationExisting:
				result.ExistingJobIDs = append(result.ExistingJobIDs, jobID)
			case delegationLegacyTombstoned:
				result.LegacyTombstonedJobIDs = append(result.LegacyTombstonedJobIDs, jobID)
			case delegationInserted:
				capacity.add(delegation.RoomID, 1)
				result.InsertedJobIDs = append(result.InsertedJobIDs, jobID)
			case delegationCapacityDenied:
				result.CapacityDenied = append(result.CapacityDenied, CapacityDenial{JobID: jobID, Reason: denial})
			default:
				return fmt.Errorf("unknown delegation admission status %d", status)
			}
		}
		for _, control := range admission.Controls {
			controlID, status, err := insertAdmittedControl(txCtx, p, admission, control)
			if err != nil {
				return err
			}
			switch status {
			case controlInserted:
				result.InsertedControlIDs = append(result.InsertedControlIDs, controlID)
			case controlExisting:
				result.ExistingControlIDs = append(result.ExistingControlIDs, controlID)
			case controlUnmatched:
				result.UnmatchedControlIDs = append(result.UnmatchedControlIDs, controlID)
			default:
				return fmt.Errorf("unknown control admission status %d", status)
			}
		}
		return nil
	})
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("admit appservice transaction %q: %w", admission.TransactionID, err)
	}
	return result, nil
}

type controlAdmissionStatus uint8

const (
	controlInserted controlAdmissionStatus = iota + 1
	controlExisting
	controlUnmatched
)

func insertAdmittedControl(
	ctx context.Context,
	p *Postgres,
	admission TransactionAdmission,
	input NewControl,
) (string, controlAdmissionStatus, error) {
	var jobID string
	err := p.db.QueryRow(ctx, `
		SELECT job_id
		FROM bridge_delegations
		WHERE matrix_placeholder_event_id = $1
		  AND room_id = $2
		  AND terminal_at IS NULL
		FOR UPDATE`, input.TargetMatrixEventID, input.RoomID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		unmatched := ControlIDFor("unmatched:"+input.TargetMatrixEventID, input.Kind, input.SourceMatrixEventID, input.Slot)
		return unmatched, controlUnmatched, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("resolve durable control target: %w", err)
	}
	controlID := ControlIDFor(jobID, input.Kind, input.SourceMatrixEventID, input.Slot)
	var logicalDuplicate bool
	if err := p.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM bridge_delegation_controls
			WHERE job_id = $1
			  AND kind = $2
			  AND slot = $3
			  AND ((error_code <> 'control_sender_rejected') = $4)
		)`, jobID, string(input.Kind), input.Slot, input.Authorized).Scan(&logicalDuplicate); err != nil {
		return "", 0, fmt.Errorf("check durable control logical duplicate: %w", err)
	}
	if logicalDuplicate {
		return controlID, controlUnmatched, nil
	}
	var count int
	if err := p.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM bridge_delegation_controls WHERE job_id = $1`, jobID).Scan(&count); err != nil {
		return "", 0, fmt.Errorf("count durable controls for job %q: %w", jobID, err)
	}
	if count >= controlCapacityLimit(admission.ControlCapacity, input.Kind) {
		return controlID, controlUnmatched, nil
	}
	control := newControl(
		controlID, jobID, admission.TransactionID, input.SourceMatrixEventID, input.SenderMXID,
		input.Kind, input.Slot,
		input.Payload, admission.CommittedAt,
	)
	control.IntakeFingerprint = controlFingerprint(input)
	if !input.Authorized {
		control.State = ControlDenied
		control.ErrorCode = input.ErrorCode
		control.Payload = nil
		control.TerminalAt = admission.CommittedAt
	}
	result, err := p.db.Exec(
		ctx, `
		INSERT INTO bridge_delegation_controls (
			control_id, job_id, appservice_transaction_id, source_matrix_event_id,
			intake_fingerprint, authorized_sender, kind, state, slot, lease_generation, recovery_count, payload,
			a2a_message_id, matrix_txn_id, matrix_event_id, error_code,
			prepared_at, created_at, updated_at, terminal_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, 0, $10, $11, $12, '', $13, NULL, $14, $14, $15)
		ON CONFLICT (control_id) DO NOTHING`,
		control.ControlID, control.JobID, admission.TransactionID, control.SourceMatrixEventID,
		control.IntakeFingerprint[:], control.AuthorizedSender, string(control.Kind), string(control.State), control.Slot,
		nonNilBytes(control.Payload), control.A2AMessageID, control.MatrixTxnID, control.ErrorCode,
		control.CreatedAt, nullableTime(control.TerminalAt),
	)
	if err != nil {
		return "", 0, fmt.Errorf("insert durable control %q: %w", controlID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", 0, fmt.Errorf("read durable control insert count: %w", err)
	}
	if rows == 1 {
		if input.Authorized {
			if _, err := p.db.Exec(ctx, `
				UPDATE bridge_delegations
				SET next_attempt_at = LEAST(next_attempt_at, $2), updated_at = $2
				WHERE job_id = $1 AND terminal_at IS NULL`, jobID, admission.CommittedAt); err != nil {
				return "", 0, fmt.Errorf("wake durable control target: %w", err)
			}
		}
		return controlID, controlInserted, nil
	}
	existing, found, err := loadControl(ctx, p, controlID)
	if err != nil {
		return "", 0, err
	}
	if !found || existing.JobID != control.JobID || existing.SourceMatrixEventID != control.SourceMatrixEventID ||
		existing.IntakeFingerprint != control.IntakeFingerprint {
		return "", 0, fmt.Errorf("%w: control_id=%q", ErrControlConflict, controlID)
	}
	return controlID, controlExisting, nil
}

// NonTerminalCount implements Ledger. The terminal timestamp is the same checked boundary used by
// admission capacity, so leased and delayed recovery work remains visible in the aggregate.
func (p *Postgres) NonTerminalCount(ctx context.Context) (int, error) {
	var count int
	if err := p.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM bridge_delegations
		WHERE terminal_at IS NULL`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count non-terminal delegations: %w", err)
	}
	return count, nil
}

func loadAdmissionCapacity(
	ctx context.Context,
	p *Postgres,
	roomLimit, globalLimit int,
) (_ admissionCapacity, returnedErr error) {
	capacity := newAdmissionCapacity(roomLimit, globalLimit)
	rows, err := p.db.Query(ctx, admissionCapacityCountSQL)
	if err != nil {
		return admissionCapacity{}, fmt.Errorf("count durable admission backlog: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("close durable admission backlog rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var roomID string
		var count int64
		if err := rows.Scan(&roomID, &count); err != nil {
			return admissionCapacity{}, fmt.Errorf("scan durable admission backlog: %w", err)
		}
		if roomID == "" || count < 0 {
			return admissionCapacity{}, fmt.Errorf("invalid durable admission backlog count")
		}
		capacity.add(roomID, count)
	}
	if err := rows.Err(); err != nil {
		return admissionCapacity{}, fmt.Errorf("read durable admission backlog: %w", err)
	}
	return capacity, nil
}

func insertAppserviceTransaction(
	ctx context.Context,
	p *Postgres,
	admission TransactionAdmission,
) (bool, error) {
	result, err := p.db.Exec(
		ctx, `
		INSERT INTO bridge_appservice_transactions (transaction_id, body_sha256, committed_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (transaction_id) DO NOTHING`,
		admission.TransactionID,
		admission.BodyHash[:],
		admission.CommittedAt,
	)
	if err != nil {
		return false, fmt.Errorf("insert transaction: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read inserted transaction count: %w", err)
	}
	if rows == 1 {
		return true, nil
	}

	var storedBytes []byte
	if err := p.db.QueryRow(ctx, `
		SELECT body_sha256
		FROM bridge_appservice_transactions
		WHERE transaction_id = $1`, admission.TransactionID).Scan(&storedBytes); err != nil {
		return false, fmt.Errorf("load replayed transaction hash: %w", err)
	}
	stored, err := transactionHashFromBytes(storedBytes)
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare(stored[:], admission.BodyHash[:]) != 1 {
		return false, &TransactionHashConflictError{
			TransactionID: admission.TransactionID,
			Stored:        stored,
			Received:      admission.BodyHash,
		}
	}
	return false, nil
}

type delegationAdmissionStatus uint8

const (
	delegationInserted delegationAdmissionStatus = iota + 1
	delegationExisting
	delegationLegacyTombstoned
	delegationCapacityDenied
)

func insertDelegation(
	ctx context.Context,
	p *Postgres,
	admission TransactionAdmission,
	delegation NewDelegation,
	capacityDenial string,
) (jobID string, status delegationAdmissionStatus, err error) {
	job := newJob(admission.TransactionID, delegation, 0, admission.CommittedAt)
	if capacityDenial != "" {
		denyJobForCapacity(&job, capacityDenial, admission.CommittedAt)
	}
	var terminalAt any
	if !job.TerminalAt.IsZero() {
		terminalAt = job.TerminalAt
	}
	err = p.db.QueryRow(
		ctx, `
			INSERT INTO bridge_delegations (
				job_id, matrix_event_id, ghost_mxid, ghost_localpart,
				appservice_transaction_id, room_id, sender_mxid, sender_origin_kind,
				sender_origin_network, matrix_origin_server_ts, target_fingerprint,
				intake_fingerprint, prompt, payload, state, next_attempt_at, error_code,
				admission_checked, admission_allowed, admission_reason,
				a2a_message_id, matrix_reply_txn_id, matrix_placeholder_txn_id,
				matrix_edit_txn_id, created_at, updated_at, terminal_at
			)
				SELECT
					$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
					$13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
					$25, $26, $27
				WHERE NOT EXISTS (
					SELECT 1
					FROM bridge_processed_events
					WHERE event_id = $2 AND processed_at >= $28
				)
			ON CONFLICT (matrix_event_id, ghost_mxid) DO NOTHING
		RETURNING job_id`,
		job.JobID,
		job.MatrixEventID,
		job.GhostMXID,
		job.GhostLocalpart,
		job.AppserviceTransactionID,
		job.RoomID,
		job.SenderMXID,
		job.SenderOriginKind,
		job.SenderOriginNetwork,
		job.OriginServerTS,
		job.TargetFingerprint,
		job.IntakeFingerprint[:],
		job.Prompt,
		nonNilBytes(job.Payload),
		job.State,
		job.NextAttemptAt,
		job.ErrorCode,
		job.AdmissionChecked,
		job.AdmissionAllowed,
		job.AdmissionReason,
		job.A2AMessageID,
		job.MatrixReplyTxnID,
		job.MatrixPlaceholderTxnID,
		job.MatrixEditTxnID,
		job.CreatedAt,
		job.UpdatedAt,
		terminalAt,
		admission.CommittedAt.Add(-retention),
	).Scan(&jobID)
	if err == nil {
		if capacityDenial != "" {
			return jobID, delegationCapacityDenied, nil
		}
		return jobID, delegationInserted, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("insert delegation %q: %w", job.JobID, err)
	}

	var storedFingerprint []byte
	if err := p.db.QueryRow(
		ctx, `
		SELECT job_id, intake_fingerprint
		FROM bridge_delegations
		WHERE matrix_event_id = $1 AND ghost_mxid = $2`,
		delegation.MatrixEventID,
		delegation.GhostMXID,
	).Scan(&jobID, &storedFingerprint); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", 0, fmt.Errorf("load existing delegation: %w", err)
		}
		var legacyTombstoned bool
		if err := p.db.QueryRow(
			ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM bridge_processed_events
				WHERE event_id = $1 AND processed_at >= $2
			)`,
			delegation.MatrixEventID,
			admission.CommittedAt.Add(-retention),
		).Scan(&legacyTombstoned); err != nil {
			return "", 0, fmt.Errorf("check legacy processed-event tombstone: %w", err)
		}
		if legacyTombstoned {
			return job.JobID, delegationLegacyTombstoned, nil
		}
		return "", 0, fmt.Errorf("delegation %q was not inserted and has no deduplication evidence", job.JobID)
	}
	stored, err := transactionHashFromBytes(storedFingerprint)
	if err != nil {
		return "", 0, fmt.Errorf("decode existing intake fingerprint: %w", err)
	}
	if subtle.ConstantTimeCompare(stored[:], job.IntakeFingerprint[:]) != 1 {
		return "", 0, &DelegationConflictError{
			MatrixEventID: delegation.MatrixEventID,
			GhostMXID:     delegation.GhostMXID,
		}
	}
	return jobID, delegationExisting, nil
}

// Claim implements Ledger.
func (p *Postgres) Claim(ctx context.Context, request ClaimRequest) (Job, bool, error) {
	if err := validateClaim(request); err != nil {
		return Job{}, false, err
	}
	query := fmt.Sprintf(claimJobSQL, qualifiedJobColumns("claimed"))
	job, err := scanJob(p.db.QueryRow(ctx, query, request.Owner, request.Now, request.Now.Add(request.LeaseDuration)))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim delegation: %w", err)
	}
	return job, true, nil
}

// ControlTarget resolves a live durable placeholder without returning recoverable content.
func (p *Postgres) ControlTarget(ctx context.Context, matrixEventID string) (ControlTarget, bool, error) {
	if matrixEventID == "" {
		return ControlTarget{}, false, fmt.Errorf("control target Matrix event ID must not be empty")
	}
	var target ControlTarget
	var stateValue string
	err := p.db.QueryRow(ctx, `
		SELECT jobs.job_id, jobs.room_id, jobs.sender_mxid, jobs.ghost_mxid, jobs.state,
			COALESCE(MAX(controls.slot) FILTER (WHERE controls.kind = 'question'), 0)
		FROM bridge_delegations AS jobs
		LEFT JOIN bridge_delegation_controls AS controls ON controls.job_id = jobs.job_id
		WHERE jobs.matrix_placeholder_event_id = $1
		  AND jobs.terminal_at IS NULL
		GROUP BY jobs.job_id`, matrixEventID).Scan(
		&target.JobID, &target.RoomID, &target.OriginalSender, &target.GhostMXID, &stateValue,
		&target.InputGeneration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlTarget{}, false, nil
	}
	if err != nil {
		return ControlTarget{}, false, fmt.Errorf("resolve durable control target: %w", err)
	}
	target.State = DelegationState(stateValue)
	if !target.State.Valid() {
		return ControlTarget{}, false, fmt.Errorf("database returned unknown delegation state %q", stateValue)
	}
	return target, true, nil
}

// ClaimControl atomically persists the next control's external-attempt boundary under the job lease.
func (p *Postgres) ClaimControl(
	ctx context.Context,
	lease LeaseToken,
	at time.Time,
) (Control, bool, error) {
	if err := validateLease(lease); err != nil {
		return Control{}, false, err
	}
	if at.IsZero() {
		return Control{}, false, fmt.Errorf("control claim time must not be zero")
	}
	query := fmt.Sprintf(`
		WITH candidate AS (
			SELECT controls.control_id
			FROM bridge_delegation_controls AS controls
			JOIN bridge_delegations AS jobs ON jobs.job_id = controls.job_id
			WHERE controls.job_id = $1
			  AND controls.state IN ('pending', 'prepared')
			  AND (controls.state = 'pending' OR controls.lease_generation < $3)
			  AND jobs.lease_owner = $2
			  AND jobs.lease_generation = $3
			  AND jobs.lease_expires_at > $4
			  AND jobs.terminal_at IS NULL
			ORDER BY controls.created_at, controls.control_id
			FOR UPDATE OF controls SKIP LOCKED
			LIMIT 1
		)
		UPDATE bridge_delegation_controls AS claimed
		SET state = 'prepared', lease_generation = $3,
			recovery_count = claimed.recovery_count + CASE WHEN claimed.state = 'prepared' THEN 1 ELSE 0 END,
			prepared_at = COALESCE(claimed.prepared_at, $4), updated_at = $4
		FROM candidate
		WHERE claimed.control_id = candidate.control_id
		RETURNING %s`, qualifiedControlColumns("claimed"))
	control, err := scanControl(p.db.QueryRow(
		ctx, query, lease.JobID, lease.Owner, int64(lease.Generation), at,
	))
	if errors.Is(err, sql.ErrNoRows) {
		job, found, loadErr := p.Job(ctx, lease.JobID)
		if loadErr != nil {
			return Control{}, false, loadErr
		}
		if !found || !leaseCurrent(job, lease, at) {
			return Control{}, false, &LeaseLostError{JobID: lease.JobID}
		}
		return Control{}, false, nil
	}
	if err != nil {
		return Control{}, false, fmt.Errorf("claim durable control: %w", err)
	}
	return control, true, nil
}

// PlanControl creates or refreshes one deterministic worker-originated pending slot.
func (p *Postgres) PlanControl(ctx context.Context, request PlanControlRequest) (Control, error) {
	if err := validatePlanControl(request); err != nil {
		return Control{}, err
	}
	var planned Control
	err := p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		jobQuery := fmt.Sprintf(planControlParentSQL, qualifiedJobColumns(""))
		job, err := scanJob(p.db.QueryRow(txCtx, jobQuery, request.Lease.JobID))
		if errors.Is(err, sql.ErrNoRows) {
			return &LeaseLostError{JobID: request.Lease.JobID}
		}
		if err != nil {
			return fmt.Errorf("lock durable control parent: %w", err)
		}
		if !leaseCurrent(job, request.Lease, request.At) {
			return &LeaseLostError{JobID: request.Lease.JobID}
		}
		controlID := ControlIDFor(request.Lease.JobID, request.Kind, "", request.Slot)
		if existing, found, err := loadControl(txCtx, p, controlID); err != nil {
			return err
		} else if found {
			if existing.State == ControlPending {
				if _, err := p.db.Exec(
					txCtx, `
					UPDATE bridge_delegation_controls SET payload = $2, updated_at = $3
					WHERE control_id = $1 AND state = 'pending'`,
					controlID, nonNilBytes(request.Payload), request.At,
				); err != nil {
					return fmt.Errorf("refresh pending durable control: %w", err)
				}
				existing.Payload = append(existing.Payload[:0], request.Payload...)
				existing.UpdatedAt = request.At
			}
			planned = existing
			return nil
		}
		var count int
		if err := p.db.QueryRow(txCtx, `
			SELECT COUNT(*) FROM bridge_delegation_controls WHERE job_id = $1`, request.Lease.JobID).Scan(&count); err != nil {
			return fmt.Errorf("count durable controls: %w", err)
		}
		if count >= controlCapacityLimit(request.Capacity, request.Kind) {
			return fmt.Errorf("%w: job_id=%q", ErrControlCapacity, request.Lease.JobID)
		}
		planned = newControl(
			controlID, request.Lease.JobID, job.AppserviceTransactionID, "", job.SenderMXID,
			request.Kind, request.Slot,
			request.Payload, request.At,
		)
		_, err = p.db.Exec(
			txCtx, `
			INSERT INTO bridge_delegation_controls (
				control_id, job_id, appservice_transaction_id, source_matrix_event_id,
				authorized_sender, kind, state, slot, lease_generation, recovery_count, payload,
				a2a_message_id, matrix_txn_id, matrix_event_id, error_code,
				prepared_at, created_at, updated_at, terminal_at
			) VALUES ($1, $2, $3, '', $4, $5, 'pending', $6, 0, 0, $7, $8, $9, '', '', NULL, $10, $10, NULL)`,
			planned.ControlID, planned.JobID, job.AppserviceTransactionID, planned.AuthorizedSender,
			string(planned.Kind), planned.Slot, nonNilBytes(planned.Payload), planned.A2AMessageID,
			planned.MatrixTxnID, request.At,
		)
		if err != nil {
			return fmt.Errorf("plan durable control: %w", err)
		}
		return nil
	})
	if err != nil {
		return Control{}, err
	}
	return planned, nil
}

// TransitionControl records one external acknowledgement or fixed failure under the job fence.
func (p *Postgres) TransitionControl(ctx context.Context, request ControlTransitionRequest) error {
	if err := validateControlTransition(request); err != nil {
		return err
	}
	args := []any{string(request.To), request.At}
	set := []string{"state = $1", "updated_at = $2"}
	guards := make([]string, 0, 1)
	add := func(column string, value any) int {
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", column, len(args)))
		return len(args)
	}
	if request.Patch.Payload != nil {
		add("payload", nonNilBytes(*request.Patch.Payload))
	}
	if request.Patch.MatrixEventID != nil {
		position := add("matrix_event_id", *request.Patch.MatrixEventID)
		guards = append(guards, fmt.Sprintf("(controls.matrix_event_id = '' OR controls.matrix_event_id = $%d)", position))
	}
	if request.Patch.ErrorCode != nil {
		add("error_code", *request.Patch.ErrorCode)
	}
	if request.To.Terminal() {
		set = append(set, "payload = '\\x'", "terminal_at = $2")
	}
	args = append(args, request.ControlID, request.Lease.JobID, request.Lease.Owner, int64(request.Lease.Generation))
	start := len(args) - 3
	guardSQL := ""
	if len(guards) > 0 {
		guardSQL = " AND " + strings.Join(guards, " AND ")
	}
	query := fmt.Sprintf(
		`
		UPDATE bridge_delegation_controls AS controls
		SET %s
		FROM bridge_delegations AS jobs
		WHERE controls.control_id = $%d
		  AND controls.job_id = $%d
		  AND controls.state = $%d
		  AND controls.lease_generation = $%d
		  AND jobs.job_id = controls.job_id
		  AND jobs.lease_owner = $%d
		  AND jobs.lease_generation = $%d
		  AND jobs.lease_expires_at > $2
		  AND jobs.terminal_at IS NULL%s`,
		strings.Join(set, ", "), start, start+1, len(args)+1, start+3, start+2, start+3, guardSQL,
	)
	args = append(args, string(request.From))
	result, err := p.db.Exec(ctx, query, args...)
	return requireFencedUpdate(result, err, request.Lease.JobID, "transition durable control")
}

// Controls returns one job's controls in deterministic order.
func (p *Postgres) Controls(ctx context.Context, jobID string) (_ []Control, returnedErr error) {
	if jobID == "" {
		return nil, fmt.Errorf("job ID must not be empty")
	}
	query := fmt.Sprintf(`SELECT %s FROM bridge_delegation_controls WHERE job_id = $1 ORDER BY created_at, control_id`,
		qualifiedControlColumns(""))
	rows, err := p.db.Query(ctx, query, jobID)
	if err != nil {
		return nil, fmt.Errorf("list durable controls: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("close durable control rows: %w", closeErr))
		}
	}()
	controls := make([]Control, 0)
	for rows.Next() {
		control, err := scanControl(rows)
		if err != nil {
			return nil, fmt.Errorf("scan durable control: %w", err)
		}
		controls = append(controls, control)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate durable controls: %w", err)
	}
	return controls, nil
}

// Heartbeat implements Ledger.
func (p *Postgres) Heartbeat(
	ctx context.Context,
	lease LeaseToken,
	now time.Time,
	duration time.Duration,
) error {
	if err := validateLease(lease); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("heartbeat time must not be zero")
	}
	if duration <= 0 {
		return fmt.Errorf("heartbeat duration must be positive")
	}
	result, err := p.db.Exec(
		ctx, `
		UPDATE bridge_delegations
		SET lease_expires_at = $4, updated_at = $5
		WHERE job_id = $1
		  AND lease_owner = $2
		  AND lease_generation = $3
		  AND terminal_at IS NULL
		  AND lease_expires_at > $5`,
		lease.JobID,
		lease.Owner,
		int64(lease.Generation),
		now.Add(duration),
		now,
	)
	return requireFencedUpdate(result, err, lease.JobID, "heartbeat delegation")
}

// RecordAdmission implements Ledger.
func (p *Postgres) RecordAdmission(ctx context.Context, request AdmissionRequest) error {
	if err := validateAdmissionRequest(request); err != nil {
		return err
	}
	return p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		result, err := p.db.Exec(
			txCtx, `
			UPDATE bridge_delegations
			SET admission_checked = true,
				admission_allowed = $4,
				admission_reason = $5,
				updated_at = $6
			WHERE job_id = $1
			  AND lease_owner = $2
			  AND lease_generation = $3
			  AND lease_expires_at > $6
			  AND state = 'pending'
			  AND admission_checked = false`,
			request.Lease.JobID,
			request.Lease.Owner,
			int64(request.Lease.Generation),
			request.Allowed,
			request.Reason,
			request.At,
		)
		if err != nil {
			return fmt.Errorf("record admission: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read admission update count: %w", err)
		}
		if rows == 1 {
			return nil
		}

		var (
			state          string
			checked        bool
			allowed        bool
			reason         string
			owner          sql.NullString
			generation     int64
			leaseExpiresAt sql.NullTime
			terminalAt     sql.NullTime
		)
		if err := p.db.QueryRow(txCtx, `
			SELECT state, admission_checked, admission_allowed, admission_reason,
				lease_owner, lease_generation, lease_expires_at, terminal_at
			FROM bridge_delegations
			WHERE job_id = $1`, request.Lease.JobID).Scan(
			&state,
			&checked,
			&allowed,
			&reason,
			&owner,
			&generation,
			&leaseExpiresAt,
			&terminalAt,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &LeaseLostError{JobID: request.Lease.JobID}
			}
			return fmt.Errorf("load persisted admission: %w", err)
		}
		current := owner.Valid && owner.String == request.Lease.Owner &&
			generation == int64(request.Lease.Generation) &&
			leaseExpiresAt.Valid && leaseExpiresAt.Time.After(request.At) &&
			!terminalAt.Valid && DelegationState(state) == StatePending
		if !current {
			return &LeaseLostError{JobID: request.Lease.JobID}
		}
		if checked && allowed == request.Allowed && reason == request.Reason {
			return nil
		}
		return fmt.Errorf("%w: job_id=%q", ErrAdmissionConflict, request.Lease.JobID)
	})
}

// RecordMatrixEvent implements Ledger without advancing the workflow state. The stable Matrix
// transaction ID makes an exact repeated response safe; a changed event ID is conflicting evidence.
func (p *Postgres) RecordMatrixEvent(ctx context.Context, request MatrixEventRequest) error {
	if err := validateMatrixEventRequest(request); err != nil {
		return err
	}
	column := matrixEventColumn(request.Stage)
	return p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		query := fmt.Sprintf(`
			UPDATE bridge_delegations
			SET %[1]s = $4, updated_at = $5
			WHERE job_id = $1
			  AND lease_owner = $2
			  AND lease_generation = $3
			  AND terminal_at IS NULL
			  AND lease_expires_at > $5
			  AND (%[1]s = '' OR %[1]s = $4)`, column)
		result, err := p.db.Exec(
			txCtx,
			query,
			request.Lease.JobID,
			request.Lease.Owner,
			int64(request.Lease.Generation),
			request.EventID,
			request.At,
		)
		if err != nil {
			return fmt.Errorf("record Matrix event: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read Matrix event update count: %w", err)
		}
		if rows == 1 {
			return nil
		}

		var (
			stored         string
			owner          sql.NullString
			generation     int64
			leaseExpiresAt sql.NullTime
			terminalAt     sql.NullTime
		)
		query = fmt.Sprintf(`
			SELECT %s, lease_owner, lease_generation, lease_expires_at, terminal_at
			FROM bridge_delegations
			WHERE job_id = $1`, column)
		if err := p.db.QueryRow(txCtx, query, request.Lease.JobID).Scan(
			&stored,
			&owner,
			&generation,
			&leaseExpiresAt,
			&terminalAt,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &LeaseLostError{JobID: request.Lease.JobID}
			}
			return fmt.Errorf("load persisted Matrix event: %w", err)
		}
		current := owner.Valid && owner.String == request.Lease.Owner &&
			generation == int64(request.Lease.Generation) &&
			leaseExpiresAt.Valid && leaseExpiresAt.Time.After(request.At) &&
			!terminalAt.Valid
		if !current {
			return &LeaseLostError{JobID: request.Lease.JobID}
		}
		if stored == request.EventID {
			return nil
		}
		return fmt.Errorf(
			"%w: job_id=%q stage=%q",
			ErrMatrixEventConflict,
			request.Lease.JobID,
			request.Stage,
		)
	})
}

// RecordDeadMan implements Ledger without advancing the workflow state. Scheduling uses a stable
// Matrix transaction ID, so an exact replay is safe while a changed delay ID is conflicting
// evidence that could otherwise strand an armed timer.
func (p *Postgres) RecordDeadMan(ctx context.Context, request DeadManRequest) error {
	if err := validateDeadManRequest(request); err != nil {
		return err
	}
	return p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		result, err := p.db.Exec(
			txCtx,
			`UPDATE bridge_delegations
			 SET matrix_dead_man_delay_id = $4, updated_at = $5
			 WHERE job_id = $1
			   AND lease_owner = $2
			   AND lease_generation = $3
			   AND terminal_at IS NULL
			   AND lease_expires_at > $5
			   AND (matrix_dead_man_delay_id = '' OR matrix_dead_man_delay_id = $4)`,
			request.Lease.JobID,
			request.Lease.Owner,
			int64(request.Lease.Generation),
			request.DelayID,
			request.At,
		)
		if err != nil {
			return fmt.Errorf("record dead-man delayed event: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read dead-man update count: %w", err)
		}
		if rows == 1 {
			return nil
		}

		var (
			stored         string
			owner          sql.NullString
			generation     int64
			leaseExpiresAt sql.NullTime
			terminalAt     sql.NullTime
		)
		if err := p.db.QueryRow(
			txCtx,
			`SELECT matrix_dead_man_delay_id, lease_owner, lease_generation, lease_expires_at, terminal_at
			 FROM bridge_delegations
			 WHERE job_id = $1`,
			request.Lease.JobID,
		).Scan(&stored, &owner, &generation, &leaseExpiresAt, &terminalAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &LeaseLostError{JobID: request.Lease.JobID}
			}
			return fmt.Errorf("load persisted dead-man delayed event: %w", err)
		}
		current := owner.Valid && owner.String == request.Lease.Owner && generation >= 0 &&
			uint64(generation) == request.Lease.Generation && leaseExpiresAt.Valid &&
			leaseExpiresAt.Time.After(request.At) && !terminalAt.Valid
		if !current {
			return &LeaseLostError{JobID: request.Lease.JobID}
		}
		if stored == request.DelayID {
			return nil
		}
		return fmt.Errorf("%w: job_id=%q", ErrDeadManConflict, request.Lease.JobID)
	})
}

// Transition implements Ledger.
func (p *Postgres) Transition(ctx context.Context, request TransitionRequest) error {
	if err := validateTransition(request); err != nil {
		return err
	}
	args := []any{string(request.To), request.At}
	set := []string{"state = $1", "attempt_count = 0", "poll_count = 0", "updated_at = $2"}
	guards := make([]string, 0, 3)
	add := func(column string, value any) int {
		args = append(args, value)
		set = append(set, fmt.Sprintf("%s = $%d", column, len(args)))
		return len(args)
	}
	if request.Patch.Prompt != nil {
		add("prompt", *request.Patch.Prompt)
	}
	if request.Patch.Payload != nil {
		add("payload", nonNilBytes(*request.Patch.Payload))
	}
	if request.Patch.ErrorCode != nil {
		add("error_code", *request.Patch.ErrorCode)
	}
	if request.Patch.A2ATaskID != nil {
		add("a2a_task_id", *request.Patch.A2ATaskID)
	}
	if request.Patch.A2AContextID != nil {
		add("a2a_context_id", *request.Patch.A2AContextID)
	}
	if request.Patch.ResultText != nil {
		add("result_text", *request.Patch.ResultText)
	}
	if request.Patch.MatrixReplyEventID != nil {
		position := add("matrix_reply_event_id", *request.Patch.MatrixReplyEventID)
		guards = append(guards, fmt.Sprintf("(matrix_reply_event_id = '' OR matrix_reply_event_id = $%d)", position))
	}
	if request.Patch.MatrixPlaceholderEventID != nil {
		position := add("matrix_placeholder_event_id", *request.Patch.MatrixPlaceholderEventID)
		guards = append(
			guards,
			fmt.Sprintf("(matrix_placeholder_event_id = '' OR matrix_placeholder_event_id = $%d)", position),
		)
	}
	if request.Patch.MatrixEditEventID != nil {
		position := add("matrix_edit_event_id", *request.Patch.MatrixEditEventID)
		guards = append(guards, fmt.Sprintf("(matrix_edit_event_id = '' OR matrix_edit_event_id = $%d)", position))
	}
	if request.Patch.TaskDeadlineAt != nil {
		if request.Patch.TaskDeadlineAt.IsZero() {
			add("task_deadline_at", nil)
		} else {
			add("task_deadline_at", *request.Patch.TaskDeadlineAt)
		}
	}
	if request.Patch.InputWaitStartedAt != nil {
		if request.Patch.InputWaitStartedAt.IsZero() {
			add("input_wait_started_at", nil)
		} else {
			add("input_wait_started_at", *request.Patch.InputWaitStartedAt)
		}
	}
	if request.Patch.InputWaitExpiresAt != nil {
		if request.Patch.InputWaitExpiresAt.IsZero() {
			add("input_wait_expires_at", nil)
		} else {
			add("input_wait_expires_at", *request.Patch.InputWaitExpiresAt)
		}
	}
	if request.To.Terminal() {
		set = append(
			set,
			"prompt = ''",
			"payload = '\\x'",
			"result_text = ''",
			"input_wait_started_at = NULL",
			"input_wait_expires_at = NULL",
			"terminal_at = $2",
			"lease_owner = NULL",
			"lease_expires_at = NULL",
		)
	}
	args = append(
		args,
		request.Lease.JobID,
		string(request.From),
		request.Lease.Owner,
		int64(request.Lease.Generation),
	)
	whereStart := len(args) - 3
	guardSQL := ""
	if len(guards) > 0 {
		guardSQL = "\n\t\t  AND " + strings.Join(guards, "\n\t\t  AND ")
	}
	query := fmt.Sprintf(
		`
		UPDATE bridge_delegations
		SET %s
		WHERE job_id = $%d
		  AND state = $%d
		  AND lease_owner = $%d
		  AND lease_generation = $%d
		  AND terminal_at IS NULL
		  AND lease_expires_at > $2%s`,
		strings.Join(set, ", "),
		whereStart,
		whereStart+1,
		whereStart+2,
		whereStart+3,
		guardSQL,
	)
	transition := func(txCtx context.Context) error {
		result, err := p.db.Exec(txCtx, query, args...)
		if err != nil {
			return fmt.Errorf("transition delegation: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("transition delegation count: %w", err)
		}
		if rows != 1 {
			if len(guards) > 0 {
				current, found, loadErr := p.Job(txCtx, request.Lease.JobID)
				if loadErr != nil {
					return fmt.Errorf("load conflicting Matrix transition evidence: %w", loadErr)
				}
				if found && current.State == request.From && leaseCurrent(current, request.Lease, request.At) {
					if stage, conflict := matrixEventPatchConflict(current, request.Patch); conflict {
						return fmt.Errorf(
							"%w: job_id=%q stage=%q",
							ErrMatrixEventConflict,
							request.Lease.JobID,
							stage,
						)
					}
				}
			}
			return &LeaseLostError{JobID: request.Lease.JobID}
		}
		if request.To.Terminal() {
			if err := p.terminalizeControls(txCtx, request.Lease.JobID, request.At); err != nil {
				return err
			}
		}
		if request.Patch.A2AContextID == nil || *request.Patch.A2AContextID == "" {
			return nil
		}
		result, err = p.db.Exec(
			txCtx, `
			INSERT INTO bridge_contexts (room_id, ghost, context_id, owners, owners_complete, updated_at)
			SELECT room_id, ghost_localpart, $2, jsonb_build_array(sender_mxid), true, $3
			FROM bridge_delegations
			WHERE job_id = $1
			ON CONFLICT (room_id, ghost) DO UPDATE
			SET context_id = EXCLUDED.context_id,
				owners = CASE
					WHEN bridge_contexts.context_id <> EXCLUDED.context_id THEN EXCLUDED.owners
					WHEN bridge_contexts.owners ? (EXCLUDED.owners ->> 0) THEN bridge_contexts.owners
					ELSE bridge_contexts.owners || EXCLUDED.owners
				END,
				owners_complete = CASE
					WHEN bridge_contexts.context_id <> EXCLUDED.context_id THEN true
					ELSE bridge_contexts.owners_complete
				END,
				updated_at = EXCLUDED.updated_at`,
			request.Lease.JobID,
			*request.Patch.A2AContextID,
			request.At,
		)
		if err != nil {
			return fmt.Errorf("store transitioned A2A context: %w", err)
		}
		rows, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read transitioned A2A context count: %w", err)
		}
		if rows != 1 {
			return fmt.Errorf("store transitioned A2A context: job %q disappeared", request.Lease.JobID)
		}
		return nil
	}
	eventPatch := request.Patch.MatrixReplyEventID != nil ||
		request.Patch.MatrixPlaceholderEventID != nil || request.Patch.MatrixEditEventID != nil
	if request.To.Terminal() || eventPatch ||
		(request.Patch.A2AContextID != nil && *request.Patch.A2AContextID != "") {
		return p.db.DoTxn(ctx, nil, transition)
	}
	return transition(ctx)
}

func (p *Postgres) terminalizeControls(ctx context.Context, jobID string, at time.Time) error {
	_, err := p.db.Exec(
		ctx, `
		UPDATE bridge_delegation_controls
		SET state = CASE WHEN terminal_at IS NULL THEN 'dead' ELSE state END,
			error_code = CASE WHEN terminal_at IS NULL THEN $3 ELSE error_code END,
			payload = '\x',
			updated_at = $2,
			terminal_at = COALESCE(terminal_at, $2)
		WHERE job_id = $1
		  AND (terminal_at IS NULL OR octet_length(payload) > 0)`,
		jobID, at, parentTerminalControlError,
	)
	if err != nil {
		return fmt.Errorf("terminalize durable controls: %w", err)
	}
	return nil
}

// ScheduleRetry implements Ledger.
func (p *Postgres) ScheduleRetry(ctx context.Context, request RetryRequest) error {
	if err := validateRetry(request); err != nil {
		return err
	}
	result, err := p.db.Exec(
		ctx, `
		UPDATE bridge_delegations
		SET next_attempt_at = $4,
			error_code = $5,
			attempt_count = CASE WHEN $7 THEN attempt_count + 1 ELSE 0 END,
			poll_count = CASE WHEN $7 THEN poll_count ELSE poll_count + 1 END,
			lease_owner = NULL,
			lease_expires_at = NULL,
			updated_at = $6
		WHERE job_id = $1
		  AND lease_owner = $2
		  AND lease_generation = $3
		  AND terminal_at IS NULL
		  AND lease_expires_at > $6`,
		request.Lease.JobID,
		request.Lease.Owner,
		int64(request.Lease.Generation),
		request.NextAttemptAt,
		request.ErrorCode,
		request.At,
		request.Kind == RetryFailure,
	)
	return requireFencedUpdate(result, err, request.Lease.JobID, "schedule delegation retry")
}

// Job implements Ledger.
func (p *Postgres) Job(ctx context.Context, jobID string) (Job, bool, error) {
	if jobID == "" {
		return Job{}, false, fmt.Errorf("job ID must not be empty")
	}
	query := fmt.Sprintf("SELECT %s FROM bridge_delegations WHERE job_id = $1", qualifiedJobColumns(""))
	job, err := scanJob(p.db.QueryRow(ctx, query, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("load delegation %q: %w", jobID, err)
	}
	return job, true, nil
}

// CleanupTerminal implements Ledger.
func (p *Postgres) CleanupTerminal(ctx context.Context, now time.Time) (CleanupResult, error) {
	if now.IsZero() {
		return CleanupResult{}, fmt.Errorf("cleanup time must not be zero")
	}
	var cleanup CleanupResult
	err := p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		result, err := p.db.Exec(txCtx, `
			UPDATE bridge_delegations
			SET prompt = '', payload = '\x', result_text = '', updated_at = $1
			WHERE terminal_at IS NOT NULL
			  AND (prompt <> '' OR octet_length(payload) > 0 OR result_text <> '')`, now)
		if err != nil {
			return fmt.Errorf("clear terminal delegation content: %w", err)
		}
		cleanup.ContentCleared, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read cleared terminal content count: %w", err)
		}

		result, err = p.db.Exec(
			txCtx, `
			UPDATE bridge_delegation_controls AS controls
			SET state = CASE WHEN controls.terminal_at IS NULL THEN 'dead' ELSE controls.state END,
				error_code = CASE WHEN controls.terminal_at IS NULL THEN $2 ELSE controls.error_code END,
				payload = '\x',
				updated_at = $1,
				terminal_at = COALESCE(controls.terminal_at, $1)
			FROM bridge_delegations AS delegations
			WHERE controls.job_id = delegations.job_id
			  AND delegations.terminal_at IS NOT NULL
			  AND (controls.terminal_at IS NULL OR octet_length(controls.payload) > 0)`,
			now, parentTerminalControlError,
		)
		if err != nil {
			return fmt.Errorf("clear terminal control content: %w", err)
		}
		clearedControls, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read cleared terminal control count: %w", err)
		}
		cleanup.ContentCleared += clearedControls

		result, err = p.db.Exec(txCtx, `
			DELETE FROM bridge_delegations
			WHERE state IN ('delivered', 'denied')
			  AND terminal_at <= $1`, now.Add(-TerminalRetention))
		if err != nil {
			return fmt.Errorf("delete expired delegation tombstones: %w", err)
		}
		cleanup.TombstonesDeleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read deleted tombstone count: %w", err)
		}

		result, err = p.db.Exec(txCtx, `
			DELETE FROM bridge_appservice_transactions AS transactions
			WHERE transactions.committed_at <= $1
			  AND NOT EXISTS (
				SELECT 1
				FROM bridge_delegations AS delegations
				WHERE delegations.appservice_transaction_id = transactions.transaction_id
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM bridge_delegation_controls AS controls
				WHERE controls.appservice_transaction_id = transactions.transaction_id
			  )`, now.Add(-TerminalRetention))
		if err != nil {
			return fmt.Errorf("delete expired unreferenced appservice transactions: %w", err)
		}
		cleanup.TransactionsDeleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read deleted appservice transaction count: %w", err)
		}

		result, err = p.db.Exec(txCtx, `
			DELETE FROM bridge_processed_events
			WHERE processed_at < $1`, now.Add(-retention))
		if err != nil {
			return fmt.Errorf("delete expired legacy processed-event tombstones: %w", err)
		}
		cleanup.LegacyTombstonesDeleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read deleted legacy tombstone count: %w", err)
		}
		return nil
	})
	if err != nil {
		return CleanupResult{}, err
	}
	return cleanup, nil
}

type rowScanner interface {
	Scan(...any) error
}

func loadControl(ctx context.Context, p *Postgres, controlID string) (Control, bool, error) {
	query := fmt.Sprintf("SELECT %s FROM bridge_delegation_controls WHERE control_id = $1", qualifiedControlColumns(""))
	control, err := scanControl(p.db.QueryRow(ctx, query, controlID))
	if errors.Is(err, sql.ErrNoRows) {
		return Control{}, false, nil
	}
	if err != nil {
		return Control{}, false, fmt.Errorf("load durable control %q: %w", controlID, err)
	}
	return control, true, nil
}

func scanControl(row rowScanner) (Control, error) {
	var (
		control           Control
		intakeFingerprint []byte
		kindValue         string
		stateValue        string
		leaseGeneration   int64
		recoveryCount     int
		terminalAt        sql.NullTime
		preparedAt        sql.NullTime
	)
	if err := row.Scan(
		&control.ControlID, &control.JobID, &control.AppserviceTransactionID,
		&control.SourceMatrixEventID, &intakeFingerprint,
		&control.AuthorizedSender, &kindValue, &stateValue, &control.Slot,
		&leaseGeneration, &recoveryCount, &control.Payload, &control.A2AMessageID, &control.MatrixTxnID,
		&control.MatrixEventID, &control.ErrorCode, &preparedAt, &control.CreatedAt, &control.UpdatedAt,
		&terminalAt,
	); err != nil {
		return Control{}, err
	}
	control.Kind = ControlKind(kindValue)
	control.State = ControlState(stateValue)
	if !control.Kind.Valid() {
		return Control{}, fmt.Errorf("database returned unknown control kind %q", kindValue)
	}
	if !control.State.Valid() {
		return Control{}, fmt.Errorf("database returned unknown control state %q", stateValue)
	}
	if leaseGeneration < 0 {
		return Control{}, fmt.Errorf("database returned negative control lease generation %d", leaseGeneration)
	}
	control.LeaseGeneration = uint64(leaseGeneration)
	control.RecoveryCount = recoveryCount
	if len(intakeFingerprint) != 0 && len(intakeFingerprint) != len(control.IntakeFingerprint) {
		return Control{}, fmt.Errorf("database returned invalid control intake fingerprint length %d", len(intakeFingerprint))
	}
	copy(control.IntakeFingerprint[:], intakeFingerprint)
	if terminalAt.Valid {
		control.TerminalAt = terminalAt.Time
	}
	if preparedAt.Valid {
		control.PreparedAt = preparedAt.Time
	}
	return control, nil
}

func scanJob(row rowScanner) (Job, error) {
	var (
		job               Job
		state             string
		intakeFingerprint []byte
		leaseOwner        sql.NullString
		leaseGeneration   int64
		leaseExpiresAt    sql.NullTime
		terminalAt        sql.NullTime
		taskDeadlineAt    sql.NullTime
		inputWaitStarted  sql.NullTime
		inputWaitExpires  sql.NullTime
	)
	if err := row.Scan(
		&job.JobID,
		&job.MatrixEventID,
		&job.GhostMXID,
		&job.GhostLocalpart,
		&job.AppserviceTransactionID,
		&job.RoomID,
		&job.IntakeSequence,
		&job.SenderMXID,
		&job.SenderOriginKind,
		&job.SenderOriginNetwork,
		&job.OriginServerTS,
		&job.TargetFingerprint,
		&intakeFingerprint,
		&job.Prompt,
		&job.Payload,
		&state,
		&leaseOwner,
		&leaseGeneration,
		&leaseExpiresAt,
		&job.AttemptCount,
		&job.PollCount,
		&job.NextAttemptAt,
		&job.ErrorCode,
		&job.AdmissionChecked,
		&job.AdmissionAllowed,
		&job.AdmissionReason,
		&job.A2AMessageID,
		&job.A2ATaskID,
		&job.A2AContextID,
		&job.ResultText,
		&job.MatrixReplyTxnID,
		&job.MatrixPlaceholderTxnID,
		&job.MatrixEditTxnID,
		&job.MatrixReplyEventID,
		&job.MatrixPlaceholderEventID,
		&job.MatrixEditEventID,
		&job.MatrixDeadManDelayID,
		&taskDeadlineAt,
		&inputWaitStarted,
		&inputWaitExpires,
		&job.CreatedAt,
		&job.UpdatedAt,
		&terminalAt,
	); err != nil {
		return Job{}, err
	}
	if !DelegationState(state).Valid() {
		return Job{}, fmt.Errorf("database returned unknown delegation state %q", state)
	}
	if leaseGeneration < 0 {
		return Job{}, fmt.Errorf("database returned negative lease generation %d", leaseGeneration)
	}
	fingerprint, err := transactionHashFromBytes(intakeFingerprint)
	if err != nil {
		return Job{}, fmt.Errorf("decode intake fingerprint: %w", err)
	}
	job.State = DelegationState(state)
	job.IntakeFingerprint = fingerprint
	job.LeaseGeneration = uint64(leaseGeneration)
	if leaseOwner.Valid {
		job.LeaseOwner = leaseOwner.String
	}
	if leaseExpiresAt.Valid {
		job.LeaseExpiresAt = leaseExpiresAt.Time
	}
	if terminalAt.Valid {
		job.TerminalAt = terminalAt.Time
	}
	if taskDeadlineAt.Valid {
		job.TaskDeadlineAt = taskDeadlineAt.Time
	}
	if inputWaitStarted.Valid {
		job.InputWaitStartedAt = inputWaitStarted.Time
	}
	if inputWaitExpires.Valid {
		job.InputWaitExpiresAt = inputWaitExpires.Time
	}
	return job, nil
}

func qualifiedJobColumns(alias string) string {
	if alias == "" {
		return strings.Join(jobColumnNames, ", ")
	}
	qualified := make([]string, len(jobColumnNames))
	for i, column := range jobColumnNames {
		qualified[i] = alias + "." + column
	}
	return strings.Join(qualified, ", ")
}

func qualifiedControlColumns(alias string) string {
	if alias == "" {
		return strings.Join(controlColumnNames, ", ")
	}
	qualified := make([]string, len(controlColumnNames))
	for i, column := range controlColumnNames {
		qualified[i] = alias + "." + column
	}
	return strings.Join(qualified, ", ")
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func transactionHashFromBytes(encoded []byte) (TransactionHash, error) {
	if len(encoded) != len(TransactionHash{}) {
		return TransactionHash{}, fmt.Errorf("SHA-256 value has %d bytes, want %d", len(encoded), len(TransactionHash{}))
	}
	var hash TransactionHash
	copy(hash[:], encoded)
	return hash, nil
}

func matrixEventColumn(stage MatrixEventStage) string {
	switch stage {
	case MatrixEventReply:
		return "matrix_reply_event_id"
	case MatrixEventPlaceholder:
		return "matrix_placeholder_event_id"
	case MatrixEventEdit:
		return "matrix_edit_event_id"
	default:
		panic("validated Matrix event stage became invalid")
	}
}

func requireFencedUpdate(result sql.Result, err error, jobID, operation string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s count: %w", operation, err)
	}
	if rows != 1 {
		return &LeaseLostError{JobID: jobID}
	}
	return nil
}
