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
	"created_at",
	"updated_at",
	"terminal_at",
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
		return nil
	})
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("admit appservice transaction %q: %w", admission.TransactionID, err)
	}
	return result, nil
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
	if request.To.Terminal() {
		set = append(
			set,
			"prompt = ''",
			"payload = '\\x'",
			"result_text = ''",
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
		if request.Patch.A2AContextID == nil || *request.Patch.A2AContextID == "" {
			return nil
		}
		result, err = p.db.Exec(
			txCtx, `
			INSERT INTO bridge_contexts (room_id, ghost, context_id, updated_at)
			SELECT room_id, ghost_localpart, $2, $3
			FROM bridge_delegations
			WHERE job_id = $1
			ON CONFLICT (room_id, ghost) DO UPDATE
			SET context_id = EXCLUDED.context_id, updated_at = EXCLUDED.updated_at`,
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
	if eventPatch || (request.Patch.A2AContextID != nil && *request.Patch.A2AContextID != "") {
		return p.db.DoTxn(ctx, nil, transition)
	}
	return transition(ctx)
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

func scanJob(row rowScanner) (Job, error) {
	var (
		job               Job
		state             string
		intakeFingerprint []byte
		leaseOwner        sql.NullString
		leaseGeneration   int64
		leaseExpiresAt    sql.NullTime
		terminalAt        sql.NullTime
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
