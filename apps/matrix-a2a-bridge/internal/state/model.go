package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

const (
	// TerminalRetention is the minimum non-content tombstone window for ordinary terminal jobs.
	// Ambiguous and dead-letter evidence is never removed by ordinary cleanup.
	TerminalRetention = 24 * time.Hour
	maxErrorCodeLen   = 128
)

var (
	// ErrTransactionHashConflict reports a replayed transaction ID whose exact request bytes changed.
	ErrTransactionHashConflict = errors.New("appservice transaction hash conflict")
	// ErrDelegationConflict reports an event/ghost identity that was admitted with different evidence.
	ErrDelegationConflict = errors.New("delegation identity conflict")
	// ErrInvalidTransition reports a state-machine edge that is not part of the durable contract.
	ErrInvalidTransition = errors.New("invalid delegation state transition")
	// ErrLeaseLost reports a stale, expired, or superseded lease token.
	ErrLeaseLost = errors.New("delegation lease lost")
	// ErrAdmissionConflict reports an attempt to replace a persisted admission decision.
	ErrAdmissionConflict = errors.New("delegation admission conflict")
	// ErrMatrixEventConflict reports an attempt to replace immutable Matrix send evidence.
	ErrMatrixEventConflict = errors.New("delegation Matrix event conflict")

	errorCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)
)

// TransactionHash is the SHA-256 of the exact appservice request body.
type TransactionHash [sha256.Size]byte

// HashTransaction returns the hash persisted for appservice transaction replay detection.
func HashTransaction(body []byte) TransactionHash {
	return sha256.Sum256(body)
}

// TransactionHashConflictError carries content-free evidence for a changed transaction replay.
type TransactionHashConflictError struct {
	TransactionID string
	Stored        TransactionHash
	Received      TransactionHash
}

func (e *TransactionHashConflictError) Error() string {
	return fmt.Sprintf(
		"%v: transaction %q stored=%s received=%s",
		ErrTransactionHashConflict,
		e.TransactionID,
		hex.EncodeToString(e.Stored[:]),
		hex.EncodeToString(e.Received[:]),
	)
}

func (e *TransactionHashConflictError) Unwrap() error { return ErrTransactionHashConflict }

// DelegationConflictError reports changed immutable evidence for an existing event/ghost pair.
type DelegationConflictError struct {
	MatrixEventID string
	GhostMXID     string
}

func (e *DelegationConflictError) Error() string {
	return fmt.Sprintf(
		"%v: matrix_event_id=%q ghost_mxid=%q",
		ErrDelegationConflict,
		e.MatrixEventID,
		e.GhostMXID,
	)
}

func (e *DelegationConflictError) Unwrap() error { return ErrDelegationConflict }

// InvalidTransitionError identifies the rejected state-machine edge.
type InvalidTransitionError struct {
	From DelegationState
	To   DelegationState
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("%v: %s -> %s", ErrInvalidTransition, e.From, e.To)
}

func (e *InvalidTransitionError) Unwrap() error { return ErrInvalidTransition }

// LeaseLostError identifies the fenced job without exposing any content.
type LeaseLostError struct {
	JobID string
}

func (e *LeaseLostError) Error() string {
	return fmt.Sprintf("%v: job_id=%q", ErrLeaseLost, e.JobID)
}

func (e *LeaseLostError) Unwrap() error { return ErrLeaseLost }

// DelegationState is the checked workflow state persisted in bridge_delegations.
type DelegationState string

const (
	// StatePending is durable work that has not started its first A2A attempt.
	StatePending DelegationState = "pending"
	// StateA2APrepared has persisted the A2A request identity before sending it.
	StateA2APrepared DelegationState = "a2a_prepared"
	// StateAwaitingTask has a known A2A task that must be resumed rather than reinvoked.
	StateAwaitingTask DelegationState = "awaiting_task"
	// StateReplyPending has a durable Matrix result or notice waiting for projection.
	StateReplyPending DelegationState = "reply_pending"
	// StateDelivered is a terminal, successfully projected Matrix result.
	StateDelivered DelegationState = "delivered"
	// StateDenied is a terminal policy or admission denial.
	StateDenied DelegationState = "denied"
	// StateAmbiguous is a terminal lost-A2A-ack outcome that must not be retried blindly.
	StateAmbiguous DelegationState = "ambiguous"
	// StateDead is terminal work that exhausted bounded recovery and needs operator evidence.
	StateDead DelegationState = "dead"
)

var delegationStates = [...]DelegationState{
	StatePending,
	StateA2APrepared,
	StateAwaitingTask,
	StateReplyPending,
	StateDelivered,
	StateDenied,
	StateAmbiguous,
	StateDead,
}

// Valid reports whether the state is part of the persisted schema contract.
func (s DelegationState) Valid() bool {
	for _, candidate := range delegationStates {
		if s == candidate {
			return true
		}
	}
	return false
}

// Terminal reports whether the state is a durable terminal outcome.
func (s DelegationState) Terminal() bool {
	switch s {
	case StateDelivered, StateDenied, StateAmbiguous, StateDead:
		return true
	default:
		return false
	}
}

// CanTransition reports whether from -> to is a legal workflow edge. Retrying or heartbeating a
// state uses the dedicated APIs rather than a self-transition.
func CanTransition(from, to DelegationState) bool {
	switch from {
	case StatePending:
		return to == StateA2APrepared || to == StateReplyPending || to == StateDenied || to == StateDead
	case StateA2APrepared:
		return to == StateAwaitingTask || to == StateReplyPending || to == StateDenied ||
			to == StateAmbiguous || to == StateDead
	case StateAwaitingTask:
		return to == StateReplyPending || to == StateDenied || to == StateDead
	case StateReplyPending:
		return to == StateDelivered || to == StateDenied || to == StateAmbiguous || to == StateDead
	default:
		return false
	}
}

// TransactionDisposition describes whether an appservice transaction was newly committed or was
// an exact replay. A changed replay is an error, never a disposition.
type TransactionDisposition uint8

const (
	// TransactionAccepted means the transaction and all new jobs committed atomically.
	TransactionAccepted TransactionDisposition = iota + 1
	// TransactionReplay means the same transaction ID and exact body hash were already committed.
	TransactionReplay
)

// TransactionAdmission is one all-or-nothing appservice transaction plus its eligible jobs.
type TransactionAdmission struct {
	TransactionID  string
	BodyHash       TransactionHash
	CommittedAt    time.Time
	RoomCapacity   int
	GlobalCapacity int
	Delegations    []NewDelegation
}

// NewDelegation is the immutable intake evidence persisted before the homeserver receives HTTP 200.
type NewDelegation struct {
	MatrixEventID       string
	GhostMXID           string
	GhostLocalpart      string
	RoomID              string
	SenderMXID          string
	SenderOriginKind    string
	SenderOriginNetwork string
	OriginServerTS      int64
	TargetFingerprint   string
	Prompt              string
	Payload             []byte
	AdmissionChecked    bool
	AdmissionAllowed    bool
	AdmissionReason     string
}

// AdmissionResult summarizes the atomic admission without returning content-bearing jobs.
type AdmissionResult struct {
	Disposition            TransactionDisposition
	InsertedJobIDs         []string
	ExistingJobIDs         []string
	LegacyTombstonedJobIDs []string
	CapacityDenied         []CapacityDenial
}

// CapacityDenial is the content-free terminal evidence for an eligible job refused before ACK
// because the durable non-terminal backlog was already full.
type CapacityDenial struct {
	JobID  string
	Reason string
}

const (
	// QueueRoomCapacityRejected preserves the legacy per-room queue boundary.
	QueueRoomCapacityRejected = "queue_room_capacity_rejected"
	// QueueGlobalCapacityRejected preserves the legacy process-wide queue boundary.
	QueueGlobalCapacityRejected = "queue_global_capacity_rejected"
)

// LeaseToken fences every state mutation by owner and monotonically increasing generation.
type LeaseToken struct {
	JobID      string
	Owner      string
	Generation uint64
}

// MatrixEventStage identifies the idempotent Matrix outbox transaction whose response is recorded.
type MatrixEventStage string

const (
	// MatrixEventReply records the durable final-reply transaction response.
	MatrixEventReply MatrixEventStage = "reply"
	// MatrixEventPlaceholder records the durable long-task placeholder transaction response.
	MatrixEventPlaceholder MatrixEventStage = "placeholder"
	// MatrixEventEdit records the durable placeholder-edit transaction response.
	MatrixEventEdit MatrixEventStage = "edit"
)

// Valid reports whether the stage maps to one of the persisted Matrix event ID fields.
func (s MatrixEventStage) Valid() bool {
	switch s {
	case MatrixEventReply, MatrixEventPlaceholder, MatrixEventEdit:
		return true
	default:
		return false
	}
}

// ClaimRequest asks for the globally oldest claimable job while preserving per-room FIFO.
type ClaimRequest struct {
	Owner         string
	Now           time.Time
	LeaseDuration time.Duration
}

// Job is the complete durable delegation record. Content-bearing Prompt, Payload, and ResultText
// are cleared when the job reaches a terminal state.
type Job struct {
	JobID                    string
	MatrixEventID            string
	GhostMXID                string
	GhostLocalpart           string
	AppserviceTransactionID  string
	RoomID                   string
	IntakeSequence           int64
	SenderMXID               string
	SenderOriginKind         string
	SenderOriginNetwork      string
	OriginServerTS           int64
	TargetFingerprint        string
	IntakeFingerprint        TransactionHash
	Prompt                   string
	Payload                  []byte
	State                    DelegationState
	LeaseOwner               string
	LeaseGeneration          uint64
	LeaseExpiresAt           time.Time
	AttemptCount             int
	PollCount                int
	NextAttemptAt            time.Time
	ErrorCode                string
	AdmissionChecked         bool
	AdmissionAllowed         bool
	AdmissionReason          string
	A2AMessageID             string
	A2ATaskID                string
	A2AContextID             string
	ResultText               string
	MatrixReplyTxnID         string
	MatrixPlaceholderTxnID   string
	MatrixEditTxnID          string
	MatrixReplyEventID       string
	MatrixPlaceholderEventID string
	MatrixEditEventID        string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	TerminalAt               time.Time
}

// LeaseToken returns the current lease fence, or a zero token for an unleased job.
func (j Job) LeaseToken() LeaseToken {
	if j.LeaseOwner == "" || j.LeaseGeneration == 0 || j.LeaseExpiresAt.IsZero() {
		return LeaseToken{}
	}
	return LeaseToken{JobID: j.JobID, Owner: j.LeaseOwner, Generation: j.LeaseGeneration}
}

// TransitionPatch atomically records protocol and Matrix outbox evidence with a state transition.
// Pointer fields distinguish "leave unchanged" from an intentional empty value.
type TransitionPatch struct {
	Prompt                   *string
	Payload                  *[]byte
	ErrorCode                *string
	A2ATaskID                *string
	A2AContextID             *string
	ResultText               *string
	MatrixReplyEventID       *string
	MatrixPlaceholderEventID *string
	MatrixEditEventID        *string
}

// TransitionRequest performs one legal, lease-fenced state transition.
type TransitionRequest struct {
	Lease LeaseToken
	From  DelegationState
	To    DelegationState
	At    time.Time
	Patch TransitionPatch
}

// RetryKind distinguishes a failed recovery attempt from healthy scheduled polling.
type RetryKind string

const (
	// RetryFailure increments the consecutive recovery-failure count.
	RetryFailure RetryKind = "failure"
	// RetryPoll resets the failure count because a working task is healthy scheduled work.
	RetryPoll RetryKind = "poll"
)

// Valid reports whether the retry kind has defined attempt-count semantics.
func (kind RetryKind) Valid() bool {
	return kind == RetryFailure || kind == RetryPoll
}

// RetryRequest schedules the current state for a later claim and releases its lease.
type RetryRequest struct {
	Lease         LeaseToken
	At            time.Time
	NextAttemptAt time.Time
	ErrorCode     string
	Kind          RetryKind
}

// AdmissionRequest persists the invocation admission decision once while the job remains pending.
type AdmissionRequest struct {
	Lease   LeaseToken
	At      time.Time
	Allowed bool
	Reason  string
}

// MatrixEventRequest persists the response event ID for one stable Matrix transaction without
// advancing the workflow state. Repeating the same event ID is idempotent; replacing it is not.
type MatrixEventRequest struct {
	Lease   LeaseToken
	At      time.Time
	Stage   MatrixEventStage
	EventID string
}

// CleanupResult reports ordinary terminal cleanup. Ambiguous and dead jobs are never deleted.
type CleanupResult struct {
	ContentCleared          int64
	TombstonesDeleted       int64
	LegacyTombstonesDeleted int64
	TransactionsDeleted     int64
}

// JobIDFor derives the stable delegation identity from the Matrix event and target ghost.
func JobIDFor(matrixEventID, ghostMXID string) string {
	sum := sha256.Sum256([]byte("fgentic-delegation-v1\x00" + matrixEventID + "\x00" + ghostMXID))
	return hex.EncodeToString(sum[:])
}

// A2AMessageIDFor derives a stable, sender-controlled A2A message ID from the durable job identity.
func A2AMessageIDFor(jobID string) string {
	sum := sha256.Sum256([]byte("fgentic-a2a-message-v1\x00" + jobID))
	return hex.EncodeToString(sum[:])
}

// MatrixTransactionIDFor derives a distinct stable Matrix transaction ID for one outbox stage.
func MatrixTransactionIDFor(jobID, stage string) string {
	sum := sha256.Sum256([]byte("fgentic-matrix-outbox-v1\x00" + stage + "\x00" + jobID))
	return hex.EncodeToString(sum[:])
}

func intakeFingerprint(delegation NewDelegation) TransactionHash {
	// Struct field order is stable, and encoding/json length-prefixes strings and base64-encodes the
	// byte payload. The domain prefix keeps this immutable evidence hash distinct from request hashes.
	// Normalize nil and empty payloads because both persist as an empty, non-NULL BYTEA value.
	delegation.Payload = nonNilBytes(delegation.Payload)
	encoded, err := json.Marshal(delegation)
	if err != nil {
		panic(fmt.Sprintf("encode validated delegation evidence: %v", err))
	}
	return sha256.Sum256(append([]byte("fgentic-intake-evidence-v1\x00"), encoded...))
}

func validateAdmission(admission TransactionAdmission) error {
	if admission.TransactionID == "" {
		return fmt.Errorf("transaction ID must not be empty")
	}
	if admission.CommittedAt.IsZero() {
		return fmt.Errorf("transaction committed time must not be zero")
	}
	if admission.RoomCapacity < 1 || admission.GlobalCapacity < 1 {
		return fmt.Errorf("durable room and global capacities must be positive")
	}
	seen := make(map[[2]string]NewDelegation, len(admission.Delegations))
	for i, delegation := range admission.Delegations {
		if err := validateNewDelegation(delegation); err != nil {
			return fmt.Errorf("delegation %d: %w", i, err)
		}
		key := [2]string{delegation.MatrixEventID, delegation.GhostMXID}
		if previous, ok := seen[key]; ok {
			if !sameAdmissionEvidence(previous, delegation) {
				return &DelegationConflictError{MatrixEventID: delegation.MatrixEventID, GhostMXID: delegation.GhostMXID}
			}
			return fmt.Errorf("delegation %d repeats event/ghost pair %q/%q", i, delegation.MatrixEventID, delegation.GhostMXID)
		}
		seen[key] = delegation
	}
	return nil
}

func validateNewDelegation(delegation NewDelegation) error {
	fields := []struct {
		name  string
		value string
	}{
		{"matrix event ID", delegation.MatrixEventID},
		{"ghost MXID", delegation.GhostMXID},
		{"ghost localpart", delegation.GhostLocalpart},
		{"room ID", delegation.RoomID},
		{"sender MXID", delegation.SenderMXID},
		{"target fingerprint", delegation.TargetFingerprint},
	}
	for _, field := range fields {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}
	}
	if delegation.AdmissionAllowed && !delegation.AdmissionChecked {
		return fmt.Errorf("allowed admission must be checked")
	}
	if delegation.AdmissionReason != "" && !errorCodePattern.MatchString(delegation.AdmissionReason) {
		return fmt.Errorf("admission reason must match %s", errorCodePattern)
	}
	return nil
}

func validateClaim(request ClaimRequest) error {
	if request.Owner == "" {
		return fmt.Errorf("lease owner must not be empty")
	}
	if request.Now.IsZero() {
		return fmt.Errorf("claim time must not be zero")
	}
	if request.LeaseDuration <= 0 {
		return fmt.Errorf("lease duration must be positive")
	}
	return nil
}

func validateLease(lease LeaseToken) error {
	if lease.JobID == "" || lease.Owner == "" || lease.Generation == 0 {
		return fmt.Errorf("lease token must contain job ID, owner, and positive generation")
	}
	if lease.Generation > math.MaxInt64 {
		return fmt.Errorf("lease generation exceeds database range")
	}
	return nil
}

func validateTransition(request TransitionRequest) error {
	if err := validateLease(request.Lease); err != nil {
		return err
	}
	if request.At.IsZero() {
		return fmt.Errorf("transition time must not be zero")
	}
	if !request.From.Valid() || !request.To.Valid() || !CanTransition(request.From, request.To) {
		return &InvalidTransitionError{From: request.From, To: request.To}
	}
	if request.Patch.ErrorCode != nil && *request.Patch.ErrorCode != "" &&
		!errorCodePattern.MatchString(*request.Patch.ErrorCode) {
		return fmt.Errorf("error code must be empty or match %s", errorCodePattern)
	}
	if request.To.Terminal() &&
		(request.Patch.Prompt != nil || request.Patch.Payload != nil || request.Patch.ResultText != nil) {
		return fmt.Errorf("terminal transition cannot persist content")
	}
	eventPatches := []struct {
		stage   MatrixEventStage
		eventID *string
	}{
		{MatrixEventReply, request.Patch.MatrixReplyEventID},
		{MatrixEventPlaceholder, request.Patch.MatrixPlaceholderEventID},
		{MatrixEventEdit, request.Patch.MatrixEditEventID},
	}
	for _, patch := range eventPatches {
		if patch.eventID != nil && *patch.eventID == "" {
			return fmt.Errorf("matrix %s event ID must not be empty", patch.stage)
		}
	}
	return nil
}

func validateRetry(request RetryRequest) error {
	if err := validateLease(request.Lease); err != nil {
		return err
	}
	if request.At.IsZero() || request.NextAttemptAt.IsZero() {
		return fmt.Errorf("retry timestamps must not be zero")
	}
	if request.NextAttemptAt.Before(request.At) {
		return fmt.Errorf("next attempt must not precede retry time")
	}
	if !errorCodePattern.MatchString(request.ErrorCode) || len(request.ErrorCode) > maxErrorCodeLen {
		return fmt.Errorf("error code must match %s", errorCodePattern)
	}
	if !request.Kind.Valid() {
		return fmt.Errorf("retry kind must be failure or poll")
	}
	return nil
}

func validateAdmissionRequest(request AdmissionRequest) error {
	if err := validateLease(request.Lease); err != nil {
		return err
	}
	if request.At.IsZero() {
		return fmt.Errorf("admission time must not be zero")
	}
	if request.Reason != "" && !errorCodePattern.MatchString(request.Reason) {
		return fmt.Errorf("admission reason must be empty or match %s", errorCodePattern)
	}
	return nil
}

func validateMatrixEventRequest(request MatrixEventRequest) error {
	if err := validateLease(request.Lease); err != nil {
		return err
	}
	if request.At.IsZero() {
		return fmt.Errorf("matrix event time must not be zero")
	}
	if !request.Stage.Valid() {
		return fmt.Errorf("invalid Matrix event stage %q", request.Stage)
	}
	if request.EventID == "" {
		return fmt.Errorf("matrix event ID must not be empty")
	}
	return nil
}

func sameAdmissionEvidence(left, right NewDelegation) bool {
	return left.MatrixEventID == right.MatrixEventID &&
		left.GhostMXID == right.GhostMXID &&
		left.GhostLocalpart == right.GhostLocalpart &&
		left.RoomID == right.RoomID &&
		left.SenderMXID == right.SenderMXID &&
		left.SenderOriginKind == right.SenderOriginKind &&
		left.SenderOriginNetwork == right.SenderOriginNetwork &&
		left.OriginServerTS == right.OriginServerTS &&
		left.TargetFingerprint == right.TargetFingerprint &&
		left.Prompt == right.Prompt &&
		string(left.Payload) == string(right.Payload) &&
		left.AdmissionChecked == right.AdmissionChecked &&
		left.AdmissionAllowed == right.AdmissionAllowed &&
		left.AdmissionReason == right.AdmissionReason
}

func newJob(transactionID string, delegation NewDelegation, sequence int64, at time.Time) Job {
	jobID := JobIDFor(delegation.MatrixEventID, delegation.GhostMXID)
	return Job{
		JobID:                   jobID,
		MatrixEventID:           delegation.MatrixEventID,
		GhostMXID:               delegation.GhostMXID,
		GhostLocalpart:          delegation.GhostLocalpart,
		AppserviceTransactionID: transactionID,
		RoomID:                  delegation.RoomID,
		IntakeSequence:          sequence,
		SenderMXID:              delegation.SenderMXID,
		SenderOriginKind:        delegation.SenderOriginKind,
		SenderOriginNetwork:     delegation.SenderOriginNetwork,
		OriginServerTS:          delegation.OriginServerTS,
		TargetFingerprint:       delegation.TargetFingerprint,
		IntakeFingerprint:       intakeFingerprint(delegation),
		Prompt:                  delegation.Prompt,
		Payload:                 nonNilBytes(delegation.Payload),
		State:                   StatePending,
		NextAttemptAt:           at,
		AdmissionChecked:        delegation.AdmissionChecked,
		AdmissionAllowed:        delegation.AdmissionAllowed,
		AdmissionReason:         delegation.AdmissionReason,
		A2AMessageID:            A2AMessageIDFor(jobID),
		MatrixReplyTxnID:        MatrixTransactionIDFor(jobID, "reply"),
		MatrixPlaceholderTxnID:  MatrixTransactionIDFor(jobID, "placeholder"),
		MatrixEditTxnID:         MatrixTransactionIDFor(jobID, "edit"),
		CreatedAt:               at,
		UpdatedAt:               at,
	}
}

func cloneJob(job Job) Job {
	job.Payload = nonNilBytes(job.Payload)
	return job
}

func nonNilBytes(value []byte) []byte {
	return append([]byte{}, value...)
}
