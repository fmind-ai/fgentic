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
	type plannedControl struct {
		control NewControl
		jobID   string
	}
	plannedControls := make([]plannedControl, 0, len(admission.Controls))
	plannedControlCounts := make(map[string]int)
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
	for _, control := range admission.Controls {
		jobID, ok := m.controlTargetJobLocked(control.TargetMatrixEventID, control.RoomID)
		if !ok {
			result.UnmatchedControlIDs = append(result.UnmatchedControlIDs,
				ControlIDFor("unmatched:"+control.TargetMatrixEventID, control.Kind, control.SourceMatrixEventID, control.Slot))
			continue
		}
		key := [3]string{jobID, control.SourceMatrixEventID, string(control.Kind)}
		if existingID, ok := m.controlBySource[key]; ok {
			existing := m.controls[existingID]
			if existing.IntakeFingerprint != controlFingerprint(control) {
				return AdmissionResult{}, fmt.Errorf(
					"%w: source_matrix_event_id=%q kind=%q",
					ErrControlConflict, control.SourceMatrixEventID, control.Kind,
				)
			}
			result.ExistingControlIDs = append(result.ExistingControlIDs, existingID)
			continue
		}
		logicalDuplicate := false
		for _, controlID := range m.controlOrder {
			existing := m.controls[controlID]
			if existing.JobID == jobID && sameControlAdmissionClass(existing, control) {
				logicalDuplicate = true
				break
			}
		}
		if !logicalDuplicate {
			for _, planned := range plannedControls {
				if planned.jobID == jobID && sameNewControlAdmissionClass(planned.control, control) {
					logicalDuplicate = true
					break
				}
			}
		}
		if logicalDuplicate {
			result.UnmatchedControlIDs = append(result.UnmatchedControlIDs,
				ControlIDFor(jobID, control.Kind, control.SourceMatrixEventID, control.Slot))
			continue
		}
		if m.controlCountLocked(jobID)+plannedControlCounts[jobID] >=
			controlCapacityLimit(admission.ControlCapacity, control.Kind) {
			result.UnmatchedControlIDs = append(result.UnmatchedControlIDs,
				ControlIDFor(jobID, control.Kind, control.SourceMatrixEventID, control.Slot))
			continue
		}
		plannedControls = append(plannedControls, plannedControl{control: control, jobID: jobID})
		plannedControlCounts[jobID]++
		result.InsertedControlIDs = append(result.InsertedControlIDs,
			ControlIDFor(jobID, control.Kind, control.SourceMatrixEventID, control.Slot))
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
	for _, item := range plannedControls {
		controlID := ControlIDFor(item.jobID, item.control.Kind, item.control.SourceMatrixEventID, item.control.Slot)
		control := newControl(
			controlID,
			item.jobID,
			admission.TransactionID,
			item.control.SourceMatrixEventID,
			item.control.SenderMXID,
			item.control.Kind,
			item.control.Slot,
			item.control.Payload,
			admission.CommittedAt,
		)
		control.IntakeFingerprint = controlFingerprint(item.control)
		if !item.control.Authorized {
			control.State = ControlDenied
			control.ErrorCode = item.control.ErrorCode
			control.Payload = nil
			control.TerminalAt = admission.CommittedAt
		}
		m.controls[controlID] = control
		m.controlOrder = append(m.controlOrder, controlID)
		m.controlBySource[[3]string{item.jobID, item.control.SourceMatrixEventID, string(item.control.Kind)}] = controlID
		job := m.jobs[item.jobID]
		if item.control.Authorized && job.NextAttemptAt.After(admission.CommittedAt) {
			job.NextAttemptAt = admission.CommittedAt
			job.UpdatedAt = admission.CommittedAt
			m.jobs[item.jobID] = job
		}
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

// ControlTarget resolves a durable placeholder without returning any content-bearing evidence.
func (m *Memory) ControlTarget(_ context.Context, matrixEventID string) (ControlTarget, bool, error) {
	if matrixEventID == "" {
		return ControlTarget{}, false, fmt.Errorf("control target Matrix event ID must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	jobID, ok := m.controlTargetJobLocked(matrixEventID, "")
	if !ok {
		return ControlTarget{}, false, nil
	}
	job := m.jobs[jobID]
	inputGeneration := 0
	for _, controlID := range m.controlOrder {
		control := m.controls[controlID]
		if control.JobID == jobID && control.Kind == ControlQuestion && control.Slot >= inputGeneration {
			inputGeneration = control.Slot
		}
	}
	return ControlTarget{
		JobID: job.JobID, RoomID: job.RoomID, OriginalSender: job.SenderMXID,
		GhostMXID: job.GhostMXID, State: job.State, InputGeneration: inputGeneration,
	}, true, nil
}

// ClaimControl persists the external-attempt boundary under the current parent-job fence.
func (m *Memory) ClaimControl(_ context.Context, lease LeaseToken, at time.Time) (Control, bool, error) {
	if err := validateLease(lease); err != nil {
		return Control{}, false, err
	}
	if at.IsZero() {
		return Control{}, false, fmt.Errorf("control claim time must not be zero")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[lease.JobID]
	if !ok || !leaseCurrent(job, lease, at) {
		return Control{}, false, &LeaseLostError{JobID: lease.JobID}
	}
	for _, controlID := range m.controlOrder {
		control := m.controls[controlID]
		if control.JobID != lease.JobID ||
			(control.State != ControlPending &&
				(control.State != ControlPrepared || control.LeaseGeneration >= lease.Generation)) {
			continue
		}
		if control.State == ControlPending {
			control.PreparedAt = at
		} else {
			control.RecoveryCount++
		}
		control.State = ControlPrepared
		control.LeaseGeneration = lease.Generation
		control.UpdatedAt = at
		m.controls[controlID] = control
		return cloneControl(control), true, nil
	}
	return Control{}, false, nil
}

// PlanControl creates one deterministic worker-originated outbox slot under the current lease.
func (m *Memory) PlanControl(_ context.Context, request PlanControlRequest) (Control, error) {
	if err := validatePlanControl(request); err != nil {
		return Control{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || !leaseCurrent(job, request.Lease, request.At) {
		return Control{}, &LeaseLostError{JobID: request.Lease.JobID}
	}
	controlID := ControlIDFor(request.Lease.JobID, request.Kind, "", request.Slot)
	if existing, ok := m.controls[controlID]; ok {
		if existing.Kind != request.Kind || existing.Slot != request.Slot {
			return Control{}, fmt.Errorf("%w: control_id=%q", ErrControlConflict, controlID)
		}
		if existing.State == ControlPending {
			existing.Payload = append(existing.Payload[:0], request.Payload...)
			existing.UpdatedAt = request.At
			m.controls[controlID] = existing
		}
		return cloneControl(existing), nil
	}
	if m.controlCountLocked(request.Lease.JobID) >= controlCapacityLimit(request.Capacity, request.Kind) {
		return Control{}, fmt.Errorf("%w: job_id=%q", ErrControlCapacity, request.Lease.JobID)
	}
	control := newControl(
		controlID, request.Lease.JobID, job.AppserviceTransactionID, "", job.SenderMXID,
		request.Kind, request.Slot,
		request.Payload, request.At,
	)
	m.controls[controlID] = control
	m.controlOrder = append(m.controlOrder, controlID)
	return cloneControl(control), nil
}

// TransitionControl records an acknowledgement or fixed failure under the parent-job fence.
func (m *Memory) TransitionControl(_ context.Context, request ControlTransitionRequest) error {
	if err := validateControlTransition(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[request.Lease.JobID]
	if !ok || !leaseCurrent(job, request.Lease, request.At) {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	control, ok := m.controls[request.ControlID]
	if !ok || control.JobID != request.Lease.JobID || control.State != request.From ||
		control.LeaseGeneration != request.Lease.Generation {
		return &LeaseLostError{JobID: request.Lease.JobID}
	}
	if request.Patch.MatrixEventID != nil && control.MatrixEventID != "" &&
		control.MatrixEventID != *request.Patch.MatrixEventID {
		return fmt.Errorf("%w: control_id=%q", ErrMatrixEventConflict, request.ControlID)
	}
	if request.Patch.Payload != nil {
		control.Payload = append(control.Payload[:0], (*request.Patch.Payload)...)
	}
	if request.Patch.MatrixEventID != nil {
		control.MatrixEventID = *request.Patch.MatrixEventID
	}
	if request.Patch.ErrorCode != nil {
		control.ErrorCode = *request.Patch.ErrorCode
	}
	control.State = request.To
	control.UpdatedAt = request.At
	if request.To.Terminal() {
		control.Payload = nil
		control.TerminalAt = request.At
	}
	m.controls[request.ControlID] = control
	return nil
}

// Controls returns copies in stable creation order for recovery and tests.
func (m *Memory) Controls(_ context.Context, jobID string) ([]Control, error) {
	if jobID == "" {
		return nil, fmt.Errorf("job ID must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	controls := make([]Control, 0)
	for _, controlID := range m.controlOrder {
		if control := m.controls[controlID]; control.JobID == jobID {
			controls = append(controls, cloneControl(control))
		}
	}
	return controls, nil
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
		job.InputWaitStartedAt = time.Time{}
		job.InputWaitExpiresAt = time.Time{}
		job.TerminalAt = request.At
		clearLease(&job)
		m.terminalizeControlsLocked(job.JobID, request.At)
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
	terminalJobs := make(map[string]struct{})
	for jobID, job := range m.jobs {
		if !job.State.Terminal() || job.TerminalAt.IsZero() {
			continue
		}
		terminalJobs[jobID] = struct{}{}
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
	deletedControls := make(map[string]struct{})
	for controlID, control := range m.controls {
		_, parentTerminal := terminalJobs[control.JobID]
		if parentTerminal && (!control.State.Terminal() || len(control.Payload) > 0) {
			if !control.State.Terminal() {
				control.State = ControlDead
				control.ErrorCode = parentTerminalControlError
				control.TerminalAt = now
			}
			control.Payload = nil
			control.UpdatedAt = now
			m.controls[controlID] = control
			result.ContentCleared++
		}
		if _, removedJob := deleted[control.JobID]; removedJob {
			delete(m.controls, controlID)
			delete(m.controlBySource, [3]string{control.JobID, control.SourceMatrixEventID, string(control.Kind)})
			deletedControls[controlID] = struct{}{}
		}
	}
	if len(deletedControls) > 0 {
		order := m.controlOrder[:0]
		for _, controlID := range m.controlOrder {
			if _, removed := deletedControls[controlID]; !removed {
				order = append(order, controlID)
			}
		}
		m.controlOrder = order
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
	for _, control := range m.controls {
		referencedTransactions[control.AppserviceTransactionID] = struct{}{}
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
	if patch.TaskDeadlineAt != nil {
		job.TaskDeadlineAt = *patch.TaskDeadlineAt
	}
	if patch.InputWaitStartedAt != nil {
		job.InputWaitStartedAt = *patch.InputWaitStartedAt
	}
	if patch.InputWaitExpiresAt != nil {
		job.InputWaitExpiresAt = *patch.InputWaitExpiresAt
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

func (m *Memory) terminalizeControlsLocked(jobID string, at time.Time) {
	for controlID, control := range m.controls {
		if control.JobID != jobID {
			continue
		}
		if !control.State.Terminal() {
			control.State = ControlDead
			control.ErrorCode = parentTerminalControlError
			control.TerminalAt = at
		}
		control.Payload = nil
		control.UpdatedAt = at
		m.controls[controlID] = control
	}
}

func (m *Memory) controlTargetJobLocked(matrixEventID, roomID string) (string, bool) {
	for _, jobID := range m.jobOrder {
		job := m.jobs[jobID]
		if job.MatrixPlaceholderEventID == matrixEventID && (roomID == "" || job.RoomID == roomID) &&
			!job.State.Terminal() {
			return jobID, true
		}
	}
	return "", false
}

func (m *Memory) controlCountLocked(jobID string) int {
	count := 0
	for _, control := range m.controls {
		if control.JobID == jobID {
			count++
		}
	}
	return count
}

func sameControlAdmissionClass(existing Control, incoming NewControl) bool {
	return existing.Kind == incoming.Kind && existing.Slot == incoming.Slot &&
		controlWasAuthorized(existing) == incoming.Authorized
}

func sameNewControlAdmissionClass(existing, incoming NewControl) bool {
	return existing.Kind == incoming.Kind && existing.Slot == incoming.Slot &&
		existing.Authorized == incoming.Authorized
}

func controlWasAuthorized(control Control) bool {
	return control.ErrorCode != "control_sender_rejected"
}

func newControl(
	controlID, jobID, appserviceTransactionID, sourceEventID, sender string,
	kind ControlKind,
	slot int,
	payload []byte,
	at time.Time,
) Control {
	return Control{
		ControlID: controlID, JobID: jobID, AppserviceTransactionID: appserviceTransactionID,
		SourceMatrixEventID: sourceEventID,
		AuthorizedSender:    sender, Kind: kind, State: ControlPending, Slot: slot,
		Payload: append([]byte(nil), payload...), A2AMessageID: ControlA2AMessageIDFor(controlID),
		MatrixTxnID: ControlMatrixTransactionIDFor(controlID), CreatedAt: at, UpdatedAt: at,
	}
}

func cloneControl(control Control) Control {
	control.Payload = append([]byte(nil), control.Payload...)
	return control
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
