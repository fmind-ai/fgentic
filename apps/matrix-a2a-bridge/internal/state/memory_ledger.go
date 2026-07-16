package state

import (
	"context"
	"crypto/subtle"
	"fmt"
	"time"
)

type memoryTransaction struct {
	hash        TransactionHash
	committedAt time.Time
}

// AdmitTransaction implements Ledger atomically under the memory store lock. Memory remains a
// development-only fallback, but matching the production state machine keeps local behavior honest.
func (m *Memory) AdmitTransaction(_ context.Context, admission TransactionAdmission) (AdmissionResult, error) {
	if err := validateAdmission(admission); err != nil {
		return AdmissionResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.transactions[admission.TransactionID]; ok {
		if subtle.ConstantTimeCompare(existing.hash[:], admission.BodyHash[:]) != 1 {
			return AdmissionResult{}, &TransactionHashConflictError{
				TransactionID: admission.TransactionID,
				Stored:        existing.hash,
				Received:      admission.BodyHash,
			}
		}
		return AdmissionResult{Disposition: TransactionReplay}, nil
	}

	result := AdmissionResult{Disposition: TransactionAccepted}
	type plannedDelegation struct {
		delegation NewDelegation
		denial     string
	}
	planned := make([]plannedDelegation, 0, len(admission.Delegations))
	expiredLegacy := make(map[string]struct{})
	capacity := newAdmissionCapacity(admission.RoomCapacity, admission.GlobalCapacity)
	for _, job := range m.jobs {
		if job.TerminalAt.IsZero() {
			capacity.add(job.RoomID, 1)
		}
	}
	legacyCutoff := admission.CommittedAt.Add(-retention)
	for _, delegation := range admission.Delegations {
		key := [2]string{delegation.MatrixEventID, delegation.GhostMXID}
		if existingID, ok := m.jobByTarget[key]; ok {
			existing := m.jobs[existingID]
			if existing.IntakeFingerprint != intakeFingerprint(delegation) {
				return AdmissionResult{}, &DelegationConflictError{
					MatrixEventID: delegation.MatrixEventID,
					GhostMXID:     delegation.GhostMXID,
				}
			}
			result.ExistingJobIDs = append(result.ExistingJobIDs, existingID)
			continue
		}
		if processedAt, ok := m.processed[delegation.MatrixEventID]; ok {
			if processedAt.Before(legacyCutoff) {
				expiredLegacy[delegation.MatrixEventID] = struct{}{}
			} else {
				result.LegacyTombstonedJobIDs = append(
					result.LegacyTombstonedJobIDs,
					JobIDFor(delegation.MatrixEventID, delegation.GhostMXID),
				)
				continue
			}
		}
		jobID := JobIDFor(delegation.MatrixEventID, delegation.GhostMXID)
		denial := capacity.denialReason(delegation.RoomID)
		planned = append(planned, plannedDelegation{delegation: delegation, denial: denial})
		if denial == "" {
			capacity.add(delegation.RoomID, 1)
			result.InsertedJobIDs = append(result.InsertedJobIDs, jobID)
		} else {
			result.CapacityDenied = append(result.CapacityDenied, CapacityDenial{JobID: jobID, Reason: denial})
		}
	}

	// Only mutate after the whole batch has passed conflict checks.
	for eventID := range expiredLegacy {
		delete(m.processed, eventID)
	}
	m.transactions[admission.TransactionID] = memoryTransaction{
		hash:        admission.BodyHash,
		committedAt: admission.CommittedAt,
	}
	for _, item := range planned {
		delegation := item.delegation
		key := [2]string{delegation.MatrixEventID, delegation.GhostMXID}
		m.nextSequence++
		job := newJob(admission.TransactionID, delegation, m.nextSequence, admission.CommittedAt)
		if item.denial != "" {
			denyJobForCapacity(&job, item.denial, admission.CommittedAt)
		}
		m.jobs[job.JobID] = job
		m.jobOrder = append(m.jobOrder, job.JobID)
		m.jobByTarget[key] = job.JobID
	}
	return result, nil
}

// NonTerminalCount implements Ledger. Leased and delayed jobs remain part of the durable backlog;
// only a checked terminal transition removes a job from this aggregate.
func (m *Memory) NonTerminalCount(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, job := range m.jobs {
		if job.TerminalAt.IsZero() {
			count++
		}
	}
	return count, nil
}

// Claim implements Ledger. The oldest non-terminal job in each room blocks every later room job,
// even while delayed or actively leased, while unrelated rooms remain concurrently claimable.
func (m *Memory) Claim(_ context.Context, request ClaimRequest) (Job, bool, error) {
	if err := validateClaim(request); err != nil {
		return Job{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	blockedRooms := make(map[string]struct{})
	for _, jobID := range m.jobOrder {
		job, ok := m.jobs[jobID]
		if !ok || !job.TerminalAt.IsZero() {
			continue
		}
		if _, blocked := blockedRooms[job.RoomID]; blocked {
			continue
		}
		blockedRooms[job.RoomID] = struct{}{}
		if job.NextAttemptAt.After(request.Now) {
			continue
		}
		if job.LeaseOwner != "" && job.LeaseExpiresAt.After(request.Now) {
			continue
		}

		job.LeaseOwner = request.Owner
		job.LeaseGeneration++
		job.LeaseExpiresAt = request.Now.Add(request.LeaseDuration)
		job.UpdatedAt = request.Now
		m.jobs[jobID] = job
		return cloneJob(job), true, nil
	}
	return Job{}, false, nil
}

// Heartbeat implements Ledger.
func (m *Memory) Heartbeat(_ context.Context, lease LeaseToken, now time.Time, duration time.Duration) error {
	if err := validateLease(lease); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("heartbeat time must not be zero")
	}
	if duration <= 0 {
		return fmt.Errorf("heartbeat duration must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[lease.JobID]
	if !ok || !leaseCurrent(job, lease, now) {
		return &LeaseLostError{JobID: lease.JobID}
	}
	job.LeaseExpiresAt = now.Add(duration)
	job.UpdatedAt = now
	m.jobs[job.JobID] = job
	return nil
}

// RecordAdmission implements Ledger. Repeating the same decision under the current lease is
// idempotent; a different decision can never overwrite the persisted spend boundary.
func (m *Memory) RecordAdmission(_ context.Context, request AdmissionRequest) error {
	if err := validateAdmissionRequest(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || job.State != StatePending || !leaseCurrent(job, request.Lease, request.At) {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	if job.AdmissionChecked {
		if job.AdmissionAllowed == request.Allowed && job.AdmissionReason == request.Reason {
			return nil
		}
		return fmt.Errorf("%w: job_id=%q", ErrAdmissionConflict, request.Lease.JobID)
	}
	job.AdmissionChecked = true
	job.AdmissionAllowed = request.Allowed
	job.AdmissionReason = request.Reason
	job.UpdatedAt = request.At
	m.jobs[job.JobID] = job
	return nil
}

// RecordMatrixEvent implements Ledger. Matrix event IDs are immutable evidence for the stable
// transaction IDs already stored on the job, so exact repeats succeed and changed responses fail.
func (m *Memory) RecordMatrixEvent(_ context.Context, request MatrixEventRequest) error {
	if err := validateMatrixEventRequest(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || !leaseCurrent(job, request.Lease, request.At) {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	stored := matrixEventID(job, request.Stage)
	if stored != "" && stored != request.EventID {
		return fmt.Errorf(
			"%w: job_id=%q stage=%q",
			ErrMatrixEventConflict,
			request.Lease.JobID,
			request.Stage,
		)
	}
	setMatrixEventID(&job, request.Stage, request.EventID)
	job.UpdatedAt = request.At
	m.jobs[job.JobID] = job
	return nil
}

// RecordDeadMan implements Ledger. Exact repeats are idempotent; a different delayed-event ID
// would leave the previously scheduled stale-task notice armed without a durable cancellation key.
func (m *Memory) RecordDeadMan(_ context.Context, request DeadManRequest) error {
	if err := validateDeadManRequest(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || !leaseCurrent(job, request.Lease, request.At) {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	if job.MatrixDeadManDelayID != "" && job.MatrixDeadManDelayID != request.DelayID {
		return fmt.Errorf("%w: job_id=%q", ErrDeadManConflict, request.Lease.JobID)
	}
	job.MatrixDeadManDelayID = request.DelayID
	job.UpdatedAt = request.At
	m.jobs[job.JobID] = job
	return nil
}

// Transition implements Ledger.
func (m *Memory) Transition(_ context.Context, request TransitionRequest) error {
	if err := validateTransition(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || job.State != request.From || !leaseCurrent(job, request.Lease, request.At) {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	if stage, conflict := matrixEventPatchConflict(job, request.Patch); conflict {
		return fmt.Errorf(
			"%w: job_id=%q stage=%q",
			ErrMatrixEventConflict,
			request.Lease.JobID,
			stage,
		)
	}
	applyPatch(&job, request.Patch)
	job.State = request.To
	job.AttemptCount = 0
	job.PollCount = 0
	job.UpdatedAt = request.At
	if request.Patch.A2AContextID != nil && *request.Patch.A2AContextID != "" {
		m.contexts[[2]string{job.RoomID, job.GhostLocalpart}] = *request.Patch.A2AContextID
	}
	if request.To.Terminal() {
		clearJobContent(&job)
		job.TerminalAt = request.At
		clearLease(&job)
	}
	m.jobs[job.JobID] = job
	return nil
}

// ScheduleRetry implements Ledger.
func (m *Memory) ScheduleRetry(_ context.Context, request RetryRequest) error {
	if err := validateRetry(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || !leaseCurrent(job, request.Lease, request.At) {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	job.NextAttemptAt = request.NextAttemptAt
	job.ErrorCode = request.ErrorCode
	if request.Kind == RetryFailure {
		job.AttemptCount++
	} else {
		job.AttemptCount = 0
		job.PollCount++
	}
	job.UpdatedAt = request.At
	clearLease(&job)
	m.jobs[job.JobID] = job
	return nil
}

// Job implements Ledger.
func (m *Memory) Job(_ context.Context, jobID string) (Job, bool, error) {
	if jobID == "" {
		return Job{}, false, fmt.Errorf("job ID must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[jobID]
	return cloneJob(job), ok, nil
}

// CleanupTerminal implements Ledger. It immediately scrubs any residual terminal content, removes
// ordinary delivered/denied tombstones after 24 hours, and retains ambiguous/dead evidence.
func (m *Memory) CleanupTerminal(_ context.Context, now time.Time) (CleanupResult, error) {
	if now.IsZero() {
		return CleanupResult{}, fmt.Errorf("cleanup time must not be zero")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := now.Add(-TerminalRetention)
	var result CleanupResult
	deleted := make(map[string]struct{})
	for jobID, job := range m.jobs {
		if !job.State.Terminal() || job.TerminalAt.IsZero() {
			continue
		}
		if job.Prompt != "" || len(job.Payload) > 0 || job.ResultText != "" {
			clearJobContent(&job)
			job.UpdatedAt = now
			m.jobs[jobID] = job
			result.ContentCleared++
		}
		if (job.State == StateDelivered || job.State == StateDenied) && !job.TerminalAt.After(cutoff) {
			delete(m.jobs, jobID)
			delete(m.jobByTarget, [2]string{job.MatrixEventID, job.GhostMXID})
			deleted[jobID] = struct{}{}
			result.TombstonesDeleted++
		}
	}
	if len(deleted) > 0 {
		order := m.jobOrder[:0]
		for _, jobID := range m.jobOrder {
			if _, removed := deleted[jobID]; !removed {
				order = append(order, jobID)
			}
		}
		m.jobOrder = order
	}
	referencedTransactions := make(map[string]struct{}, len(m.jobs))
	for _, job := range m.jobs {
		referencedTransactions[job.AppserviceTransactionID] = struct{}{}
	}
	for transactionID, transaction := range m.transactions {
		if _, referenced := referencedTransactions[transactionID]; referenced || transaction.committedAt.After(cutoff) {
			continue
		}
		delete(m.transactions, transactionID)
		result.TransactionsDeleted++
	}
	for eventID, processedAt := range m.processed {
		if processedAt.Before(cutoff) {
			delete(m.processed, eventID)
			result.LegacyTombstonesDeleted++
		}
	}
	return result, nil
}

func leaseCurrent(job Job, lease LeaseToken, at time.Time) bool {
	return job.TerminalAt.IsZero() &&
		job.LeaseOwner == lease.Owner &&
		job.LeaseGeneration == lease.Generation &&
		job.LeaseExpiresAt.After(at)
}

func applyPatch(job *Job, patch TransitionPatch) {
	if patch.Prompt != nil {
		job.Prompt = *patch.Prompt
	}
	if patch.Payload != nil {
		job.Payload = append(job.Payload[:0], (*patch.Payload)...)
	}
	if patch.ErrorCode != nil {
		job.ErrorCode = *patch.ErrorCode
	}
	if patch.A2ATaskID != nil {
		job.A2ATaskID = *patch.A2ATaskID
	}
	if patch.A2AContextID != nil {
		job.A2AContextID = *patch.A2AContextID
	}
	if patch.ResultText != nil {
		job.ResultText = *patch.ResultText
	}
	if patch.MatrixReplyEventID != nil {
		job.MatrixReplyEventID = *patch.MatrixReplyEventID
	}
	if patch.MatrixPlaceholderEventID != nil {
		job.MatrixPlaceholderEventID = *patch.MatrixPlaceholderEventID
	}
	if patch.MatrixEditEventID != nil {
		job.MatrixEditEventID = *patch.MatrixEditEventID
	}
}

func clearJobContent(job *Job) {
	job.Prompt = ""
	job.Payload = nil
	job.ResultText = ""
}

func clearLease(job *Job) {
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
}

func matrixEventID(job Job, stage MatrixEventStage) string {
	switch stage {
	case MatrixEventReply:
		return job.MatrixReplyEventID
	case MatrixEventPlaceholder:
		return job.MatrixPlaceholderEventID
	case MatrixEventEdit:
		return job.MatrixEditEventID
	default:
		panic("validated Matrix event stage became invalid")
	}
}

func setMatrixEventID(job *Job, stage MatrixEventStage, eventID string) {
	switch stage {
	case MatrixEventReply:
		job.MatrixReplyEventID = eventID
	case MatrixEventPlaceholder:
		job.MatrixPlaceholderEventID = eventID
	case MatrixEventEdit:
		job.MatrixEditEventID = eventID
	default:
		panic("validated Matrix event stage became invalid")
	}
}

func matrixEventPatchConflict(job Job, patch TransitionPatch) (MatrixEventStage, bool) {
	checks := []struct {
		stage    MatrixEventStage
		stored   string
		received *string
	}{
		{MatrixEventReply, job.MatrixReplyEventID, patch.MatrixReplyEventID},
		{MatrixEventPlaceholder, job.MatrixPlaceholderEventID, patch.MatrixPlaceholderEventID},
		{MatrixEventEdit, job.MatrixEditEventID, patch.MatrixEditEventID},
	}
	for _, check := range checks {
		if check.received != nil && check.stored != "" && check.stored != *check.received {
			return check.stage, true
		}
	}
	return "", false
}
