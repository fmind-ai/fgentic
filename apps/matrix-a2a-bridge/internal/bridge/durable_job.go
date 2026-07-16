package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	durablePayloadVersion = 1
	durableAuditSchema    = "fgentic.delegation_ledger.v1"

	errorA2AAckAmbiguous     = "a2a_ack_ambiguous"
	errorA2APreflightRetry   = "a2a_preflight_retry"
	errorAgentFailed         = "agent_failed"
	errorAgentMappingChanged = "agent_mapping_changed"
	errorAgentUntrusted      = "agent_card_untrusted"
	errorAuthRequired        = "auth_required_not_forwarded"
	errorInputRequired       = "input_required"
	errorInvalidPayload      = "invalid_recovery_payload"
	errorMatrixDelivery      = "matrix_delivery_failed"
	errorMatrixJoin          = "matrix_join_failed"
	errorMatrixRegister      = "matrix_registration_failed"
	errorMediaDenied         = "media_input_rejected"
	errorQuoteOverBudget     = "quote_over_budget"
	errorRateLimit           = "rate_limit_rejected"
	errorSenderPolicy        = "sender_policy_rejected"
	errorStagePolicy         = "stage_policy_rejected"
	errorDeadManRefresh      = "dead_man_refresh_failed"
	errorTaskFailed          = "task_failed"
	errorTaskInvalid         = "task_result_invalid"
	errorTaskPoll            = "task_poll_failed"
	errorTaskTimeout         = "task_timeout"
	errorStateContext        = "context_load_failed"
)

var errDeadManCleanupPending = errors.New("dead-man cleanup remains pending")

// durablePayload is the bounded recovery envelope retained until a Matrix projection reaches a
// durable terminal state. Event is the original Matrix event; Result or Notice is populated only
// after A2A becomes terminal. TerminalState tells a replayed outbox which checked state to commit.
type durablePayload struct {
	Version       int                       `json:"version"`
	Event         json.RawMessage           `json:"event"`
	AgentVersion  string                    `json:"agent_version,omitempty"`
	AgentContract string                    `json:"agent_contract_sha256,omitempty"`
	TargetRouteID string                    `json:"target_route_id,omitempty"`
	Result        *a2aclient.Result         `json:"result,omitempty"`
	Notice        string                    `json:"notice,omitempty"`
	TerminalState state.DelegationState     `json:"terminal_state,omitempty"`
	Audit         durableTerminalAuditState `json:"audit,omitempty"`
	MediaIn       int                       `json:"media_in,omitempty"`
	MediaRejected int                       `json:"media_rejected,omitempty"`
}

type durableTerminalAuditState struct {
	Outcome        string                `json:"outcome,omitempty"`
	TerminalStage  string                `json:"terminal_stage,omitempty"`
	TerminalReason string                `json:"terminal_reason,omitempty"`
	RateLimit      auditRateLimitVerdict `json:"rate_limit_verdict,omitempty"`
	// A2AAttempted is persisted before calling the A2A client so terminal audit does not infer a
	// network-side effect from an unrelated terminal state.
	A2AAttempted bool  `json:"a2a_attempted,omitempty"`
	A2AStartedAt int64 `json:"a2a_started_at_ms,omitempty"`
}

type durableA2AClient interface {
	CallWithMessageID(
		context.Context,
		a2aclient.Target,
		string,
		string,
		string,
		[]a2aclient.InboundFile,
	) (a2aclient.Result, error)
	ResumeTask(context.Context, a2aclient.Target, string) (a2aclient.Result, error)
}

// executeDurableJob advances exactly one fenced job until it either reaches a terminal state or is
// released for a bounded retry. The dispatcher owns heartbeats and cancels ctx on lease loss.
func (b *Bridge) executeDurableJob(ctx context.Context, claimed state.Job) {
	job := claimed
	var err error
	switch job.State {
	case state.StatePending:
		err = b.executePendingJob(ctx, &job)
	case state.StateA2APrepared:
		err = b.recoverPreparedJob(ctx, &job)
	case state.StateAwaitingTask:
		err = b.resumeKnownTask(ctx, &job)
	case state.StateReplyPending:
		err = b.deliverPendingReply(ctx, &job)
	default:
		err = fmt.Errorf("claimed unexpected state %q", job.State)
	}
	if err == nil || errors.Is(err, state.ErrLeaseLost) || ctx.Err() != nil {
		return
	}
	if errors.Is(err, errDeadManCleanupPending) {
		// The retry could not be persisted. Leave the reply_pending job fenced until its lease expires;
		// the next owner must retry cleanup instead of exhausting into a terminal state with an armed
		// homeserver timer.
		b.log.Error(
			"durable dead-man cleanup remains pending",
			"job_id", job.JobID,
			"state", job.State,
			"reason", "cleanup_retry_persistence_failed",
			"error_type", fmt.Sprintf("%T", err),
		)
		return
	}
	b.log.Error(
		"durable delegation worker failed",
		"job_id", job.JobID,
		"state", job.State,
		"reason", "operation_failed",
		"error_type", fmt.Sprintf("%T", err),
	)
	if retryErr := b.retryOrDead(ctx, &job, "worker_failed", err); retryErr != nil &&
		!errors.Is(retryErr, state.ErrLeaseLost) {
		b.log.Error(
			"durable delegation recovery failed",
			"job_id", job.JobID,
			"reason", "storage_error",
			"error_type", fmt.Sprintf("%T", retryErr),
		)
	}
}

func (b *Bridge) executePendingJob(ctx context.Context, job *state.Job) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, errorInvalidPayload, err)
	}
	sender, ref, denial := b.revalidateDurableJob(*job, evt)
	if ref != nil {
		payload.AgentVersion = ref.AgentVersion()
		payload.AgentContract = ref.AgentContractSHA256()
		payload.TargetRouteID = ref.RouteID()
	}
	if denial != "" {
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, denial)
	}
	if !job.AdmissionChecked {
		allowed, reason := b.reserveDurableAdmission(*job, evt, sender)
		if err := b.store.RecordAdmission(ctx, state.AdmissionRequest{
			Lease: job.LeaseToken(), At: time.Now().UTC(), Allowed: allowed, Reason: reason,
		}); err != nil {
			// The database may have committed even when its acknowledgement was lost. Retaining an
			// allowed reservation fails closed; refunding it could let recovery dispatch persisted
			// work without any invocation-budget debit.
			return fmt.Errorf("record durable admission: %w", err)
		}
		job.AdmissionChecked = true
		job.AdmissionAllowed = allowed
		job.AdmissionReason = reason
	}
	if !job.AdmissionAllowed {
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, job.AdmissionReason)
	}

	ghost := id.UserID(job.GhostMXID)
	intent := b.as.Intent(ghost)
	if err := intent.EnsureRegistered(ctx); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixRegister, fmt.Errorf("ensure durable ghost registered: %w", err))
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixJoin, fmt.Errorf("ensure durable ghost joined: %w", err))
	}

	inboundFiles, mediaRejected, mediaOK := b.collectInboundMedia(ctx, intent, evt, ref)
	payload.MediaIn = len(inboundFiles)
	payload.MediaRejected = mediaRejected
	if !mediaOK {
		return b.prepareDurableDeniedNotice(ctx, job, payload, evt, ref, sender,
			failureMessage(errorMediaDenied, job.GhostLocalpart, 0), "media_admission", errorMediaDenied)
	}
	contextID, err := b.store.Context(ctx, job.RoomID, job.GhostLocalpart)
	if err != nil {
		return b.retryOrDead(ctx, job, errorStateContext, fmt.Errorf("load durable conversation context: %w", err))
	}
	// Persist the attempt boundary together with the prepared state. Recovery can then distinguish
	// pre-A2A failures from work that may have crossed the remote side-effect boundary.
	payload.Audit.A2AAttempted = true
	if payload.Audit.A2AStartedAt == 0 {
		payload.Audit.A2AStartedAt = time.Now().UTC().UnixMilli()
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode prepared durable payload: %w", err)
	}
	if err := b.transitionDurable(ctx, job, state.StateA2APrepared, state.TransitionPatch{
		A2AContextID: stringPointer(contextID), ErrorCode: stringPointer(""), Payload: &encodedPayload,
	}); err != nil {
		return err
	}
	return b.callPreparedJob(ctx, job, payload, evt, ref, sender, contextID, inboundFiles)
}

func (b *Bridge) recoverPreparedJob(ctx context.Context, job *state.Job) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, errorInvalidPayload, err)
	}
	if job.ErrorCode != errorA2APreflightRetry {
		// Once an attempt was prepared, an unknown outcome outranks later policy or mapping changes:
		// those changes cannot prove that the original remote side effect did not happen.
		sender, senderErr := durableSender(*job)
		if senderErr != nil {
			sender = matrixSender(evt.Sender)
		}
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, nil, sender, state.StateAmbiguous,
			outcomeAmbiguous, "message_send", errorA2AAckAmbiguous, 0,
		)
	}
	sender, ref, denial := b.revalidateDurableJob(*job, evt)
	if denial != "" {
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, denial)
	}
	ghost := id.UserID(job.GhostMXID)
	intent := b.as.Intent(ghost)
	if err := intent.EnsureRegistered(ctx); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixRegister,
			fmt.Errorf("ensure recovered durable ghost registered: %w", err))
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixJoin,
			fmt.Errorf("ensure recovered durable ghost joined: %w", err))
	}
	inboundFiles, mediaRejected, mediaOK := b.collectInboundMedia(ctx, intent, evt, ref)
	payload.MediaIn = len(inboundFiles)
	payload.MediaRejected = mediaRejected
	if !mediaOK {
		return b.prepareDurableDeniedNotice(ctx, job, payload, evt, ref, sender,
			failureMessage(errorMediaDenied, job.GhostLocalpart, 0), "media_admission", errorMediaDenied)
	}
	contextID := job.A2AContextID
	return b.callPreparedJob(ctx, job, payload, evt, ref, sender, contextID, inboundFiles)
}

func (b *Bridge) callPreparedJob(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	contextID string,
	inboundFiles []a2aclient.InboundFile,
) error {
	client, ok := b.client.(durableA2AClient)
	if !ok {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, "durable_a2a_unsupported",
			fmt.Errorf("A2A client does not support durable calls"))
	}
	a2aCtx := withAgentPolicyContext(ctx, job.SenderMXID, ref)
	deadline, _ := b.durableTaskDeadline(*job, ref, payload)
	a2aCtx, cancelDelegation := context.WithDeadline(a2aCtx, deadline)
	defer cancelDelegation()
	callCtx, cancel := context.WithTimeout(a2aCtx, b.cfg.RequestTimeout)
	started := time.Now()
	result, err := client.CallWithMessageID(
		callCtx,
		ref.Target(),
		job.A2AMessageID,
		provenancePrompt(evt, job.Prompt),
		contextID,
		inboundFiles,
	)
	cancel()
	a2aLatency.WithLabelValues(job.GhostLocalpart).Observe(time.Since(started).Seconds())
	if err != nil {
		if errors.Is(err, a2aclient.ErrSendAcknowledgementAmbiguous) {
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateAmbiguous,
				outcomeAmbiguous, "message_send", errorA2AAckAmbiguous, 0,
			)
		}
		if errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
			return b.denyDurableJob(ctx, job, payload, evt, ref, sender, errorAgentUntrusted)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateDelivered,
				outcomeTimeout, "message_send", errorRequestTimeout, b.cfg.RequestTimeout,
			)
		}
		return b.retryOrDead(ctx, job, errorA2APreflightRetry,
			errors.New("A2A preflight failed before message dispatch"))
	}
	return b.acceptDurableA2AResult(ctx, job, payload, evt, ref, sender, result)
}

func (b *Bridge) acceptDurableA2AResult(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	result a2aclient.Result,
) error {
	contextID := orDefault(result.ContextID, job.A2AContextID)
	if !result.Terminal {
		if result.TaskID == "" {
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateDead,
				outcomeDead, "task_poll", errorTaskInvalid, 0,
			)
		}
		if result.AuthRequired {
			captureDurableA2AEvidence(job, result)
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateDelivered,
				outcomeFailed, "task_auth", errorAuthRequired, 0,
			)
		}
		if result.InputRequired {
			captureDurableA2AEvidence(job, result)
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateDelivered,
				outcomeFailed, "task_input", errorInputRequired, 0,
			)
		}
		if err := b.transitionDurable(ctx, job, state.StateAwaitingTask, state.TransitionPatch{
			A2ATaskID: stringPointer(result.TaskID), A2AContextID: stringPointer(contextID),
			ErrorCode: stringPointer(""),
		}); err != nil {
			return err
		}
		if err := b.ensureDurablePlaceholder(ctx, job, evt); err != nil {
			return b.retryOrDead(ctx, job, errorMatrixDelivery, err)
		}
		if err := b.ensureDurableDeadMan(ctx, job, evt); err != nil {
			return err
		}
		return b.scheduleTaskPoll(ctx, *job, false)
	}
	if result.Failed {
		captureDurableA2AEvidence(job, result)
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDelivered,
			outcomeFailed, "message_result", errorAgentFailed, 0,
		)
	}
	if emptyAgentReply(result) {
		captureDurableA2AEvidence(job, result)
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDelivered,
			outcomeFailed, "message_result", errorEmptyReply, 0,
		)
	}
	return b.prepareDurableResult(ctx, job, payload, result, state.StateDelivered,
		outcomeOK, "message_result", "completed")
}

func (b *Bridge) resumeKnownTask(ctx context.Context, job *state.Job) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, errorInvalidPayload, err)
	}
	sender, ref, denial := b.revalidateDurableTask(*job, payload, evt)
	if denial != "" {
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, denial)
	}
	if job.A2ATaskID == "" {
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDead,
			outcomeDead, "task_poll", errorTaskInvalid, 0,
		)
	}
	deadline, timeout := b.durableTaskDeadline(*job, ref, payload)
	if !time.Now().Before(deadline) {
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDelivered,
			outcomeTimeout, "task_poll", errorTaskTimeout, timeout,
		)
	}
	if err := b.ensureDurablePlaceholder(ctx, job, evt); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixDelivery, err)
	}
	if err := b.ensureDurableDeadMan(ctx, job, evt); err != nil {
		return err
	}
	deadManRefreshFailed := b.restartDurableDeadManOnPoll(ctx, job)
	client, ok := b.client.(durableA2AClient)
	if !ok {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, "durable_a2a_unsupported",
			fmt.Errorf("A2A client does not support durable task resume"))
	}
	overallCtx, cancelOverall := context.WithDeadline(
		withAgentPolicyContext(ctx, job.SenderMXID, ref), deadline,
	)
	defer cancelOverall()
	pollCtx, cancel := context.WithTimeout(overallCtx, b.cfg.RequestTimeout)
	result, err := client.ResumeTask(pollCtx, ref.Target(), job.A2ATaskID)
	cancel()
	if err != nil {
		if errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
			return b.denyDurableJob(ctx, job, payload, evt, ref, sender, errorAgentUntrusted)
		}
		return b.retryOrDead(ctx, job, errorTaskPoll, errors.New("A2A task poll failed"))
	}
	if result.TaskID != "" && result.TaskID != job.A2ATaskID {
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDead,
			outcomeDead, "task_poll", errorTaskInvalid, 0,
		)
	}
	if !result.Terminal {
		if result.AuthRequired {
			captureDurableA2AEvidence(job, result)
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateDelivered,
				outcomeFailed, "task_auth", errorAuthRequired, 0,
			)
		}
		if result.InputRequired {
			captureDurableA2AEvidence(job, result)
			return b.prepareDurableFailureNotice(
				ctx, job, payload, evt, ref, sender, state.StateDelivered,
				outcomeFailed, "task_input", errorInputRequired, 0,
			)
		}
		return b.scheduleTaskPoll(ctx, *job, deadManRefreshFailed)
	}
	if result.Failed {
		captureDurableA2AEvidence(job, result)
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDelivered,
			outcomeFailed, "task_result", errorTaskFailed, 0,
		)
	}
	if emptyAgentReply(result) {
		captureDurableA2AEvidence(job, result)
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDelivered,
			outcomeFailed, "task_result", errorEmptyReply, 0,
		)
	}
	return b.prepareDurableResult(ctx, job, payload, result, state.StateDelivered,
		outcomeOK, "task_result", "completed")
}

func (b *Bridge) ensureDurablePlaceholder(
	ctx context.Context,
	job *state.Job,
	evt *event.Event,
) error {
	if job.MatrixPlaceholderEventID != "" {
		return nil
	}
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	eventID, err := b.sendDurableNotice(ctx, intent, evt, workingText, job.MatrixPlaceholderTxnID)
	if err != nil {
		return err
	}
	if err := b.store.RecordMatrixEvent(ctx, state.MatrixEventRequest{
		Lease: job.LeaseToken(), At: time.Now().UTC(), Stage: state.MatrixEventPlaceholder,
		EventID: eventID.String(),
	}); err != nil {
		return fmt.Errorf("record durable Matrix placeholder: %w", err)
	}
	job.MatrixPlaceholderEventID = eventID.String()
	return nil
}

func (b *Bridge) prepareDurableResult(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	result a2aclient.Result,
	terminalState state.DelegationState,
	outcome, terminalStage, terminalReason string,
) error {
	payload.Result = &result
	payload.Notice = ""
	payload.TerminalState = terminalState
	payload.Audit = durableTerminalAuditState{
		Outcome: outcome, TerminalStage: terminalStage, TerminalReason: terminalReason,
		A2AAttempted: payload.Audit.A2AAttempted, A2AStartedAt: payload.Audit.A2AStartedAt,
		RateLimit: payload.Audit.RateLimit,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode durable result: %w", err)
	}
	contextID := orDefault(result.ContextID, job.A2AContextID)
	if err := b.transitionDurable(ctx, job, state.StateReplyPending, state.TransitionPatch{
		Payload: &encoded, ResultText: stringPointer(result.Text), A2ATaskID: stringPointer(result.TaskID),
		A2AContextID: stringPointer(contextID), ErrorCode: stringPointer(terminalReason),
	}); err != nil {
		return err
	}
	return b.deliverPendingReply(ctx, job)
}

func (b *Bridge) prepareDurableNotice(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	terminalState state.DelegationState,
	notice, outcome, terminalStage, terminalReason string,
) error {
	_ = evt
	_ = ref
	_ = sender
	payload.Result = nil
	payload.Notice = notice
	payload.TerminalState = terminalState
	payload.Audit = durableTerminalAuditState{
		Outcome: outcome, TerminalStage: terminalStage, TerminalReason: terminalReason,
		A2AAttempted: payload.Audit.A2AAttempted, A2AStartedAt: payload.Audit.A2AStartedAt,
		RateLimit: payload.Audit.RateLimit,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode durable notice: %w", err)
	}
	patch := state.TransitionPatch{Payload: &encoded, ResultText: &notice, ErrorCode: &terminalReason}
	if job.A2ATaskID != "" {
		patch.A2ATaskID = stringPointer(job.A2ATaskID)
	}
	if job.A2AContextID != "" {
		patch.A2AContextID = stringPointer(job.A2AContextID)
	}
	if err := b.transitionDurable(ctx, job, state.StateReplyPending, patch); err != nil {
		return err
	}
	return b.deliverPendingReply(ctx, job)
}

// prepareDurableFailureNotice projects catalog copy only after reserving notice-plane capacity.
// Replacing an existing placeholder does not create another timeline event, so it must always
// reach a terminal edit. The ledger transition and audit remain durable when a new notice is suppressed.
func (b *Bridge) prepareDurableFailureNotice(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	terminalState state.DelegationState,
	outcome, terminalStage, terminalReason string,
	timeout time.Duration,
) error {
	payload.Audit = durableTerminalAuditState{
		Outcome: outcome, TerminalStage: terminalStage, TerminalReason: terminalReason,
		A2AAttempted: payload.Audit.A2AAttempted, A2AStartedAt: payload.Audit.A2AStartedAt,
		RateLimit: payload.Audit.RateLimit,
	}
	if job.MatrixPlaceholderEventID == "" && !b.allowNotice(sender, evt.RoomID, job.GhostLocalpart) {
		return b.finishDurableWithoutReplyWithEvidence(
			ctx, job, terminalState, terminalReason, nil, payload, evt, ref, sender,
		)
	}
	return b.prepareDurableNotice(
		ctx, job, payload, evt, ref, sender, terminalState,
		failureMessage(terminalReason, job.GhostLocalpart, timeout),
		outcome, terminalStage, terminalReason,
	)
}

// prepareDurableDeniedNotice applies the independent notice budget before persisting a denial
// outbox. Exhausting that budget never revives or dispatches the denied delegation.
func (b *Bridge) prepareDurableDeniedNotice(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	notice, terminalStage, terminalReason string,
) error {
	if job.MatrixPlaceholderEventID == "" && !b.allowNotice(sender, evt.RoomID, job.GhostLocalpart) {
		payload.Audit = durableTerminalAuditState{
			Outcome: outcomeDenied, TerminalStage: terminalStage, TerminalReason: terminalReason,
			A2AAttempted: payload.Audit.A2AAttempted, A2AStartedAt: payload.Audit.A2AStartedAt,
			RateLimit: durableRateLimitVerdict(*job),
		}
		return b.finishDurableWithoutReplyWithEvidence(
			ctx, job, state.StateDenied, terminalReason, nil, payload, evt, ref, sender,
		)
	}
	payload.Audit.RateLimit = durableRateLimitVerdict(*job)
	return b.prepareDurableNotice(
		ctx, job, payload, evt, ref, sender, state.StateDenied,
		notice, outcomeDenied, terminalStage, terminalReason,
	)
}

func (b *Bridge) deliverPendingReply(ctx context.Context, job *state.Job) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, errorInvalidPayload, err)
	}
	sender, ref, denial := b.revalidateDurableJob(*job, evt)
	if denial != "" && payload.TerminalState == state.StateDelivered {
		// A result already accepted from the bound target must not be redirected after a mapping change;
		// it is still safe to project into the original room as that ghost.
		b.log.Warn("projecting persisted result after mapping changed", "job_id", job.JobID, "reason", denial)
	}
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	var eventID id.EventID
	var stage state.MatrixEventStage
	var mediaOut, mediaRejected int
	switch {
	case job.MatrixEditEventID != "":
		stage = state.MatrixEventEdit
		eventID = id.EventID(job.MatrixEditEventID)
	case job.MatrixReplyEventID != "":
		stage = state.MatrixEventReply
		eventID = id.EventID(job.MatrixReplyEventID)
	case payload.Result != nil:
		result := *payload.Result
		mappingRejected := 0
		if denial != "" && len(result.Files) > 0 {
			mappingRejected = len(result.Files)
			result.Text += fmt.Sprintf("\n\n⚠️ %d artifact(s) withheld because the agent mapping changed.", len(result.Files))
			result.Files = nil
		}
		if ref == nil {
			// prepareReply only consults the mapping for file policy. Files are removed above when
			// the bound mapping disappeared, so a zero reference is safe for text-only projection.
			ref = &AgentRef{}
		}
		var edit bool
		eventID, edit, mediaOut, mediaRejected, err = b.deliverDurableReply(ctx, intent, evt, *job, ref, result)
		mediaRejected += mappingRejected
		if edit {
			stage = state.MatrixEventEdit
		} else {
			stage = state.MatrixEventReply
		}
	case job.MatrixPlaceholderEventID != "":
		stage = state.MatrixEventEdit
		eventID, err = b.editDurableNotice(
			ctx, intent, evt.RoomID, id.EventID(job.MatrixPlaceholderEventID), payload.Notice, job.MatrixEditTxnID,
		)
	default:
		stage = state.MatrixEventReply
		eventID, err = b.sendDurableNotice(ctx, intent, evt, payload.Notice, job.MatrixReplyTxnID)
	}
	if err != nil {
		return b.retryOrDead(ctx, job, errorMatrixDelivery, err)
	}
	patch := state.TransitionPatch{}
	switch stage {
	case state.MatrixEventReply:
		patch.MatrixReplyEventID = stringPointer(eventID.String())
	case state.MatrixEventEdit:
		patch.MatrixEditEventID = stringPointer(eventID.String())
	}
	// Persist accepted Matrix evidence before cancellation. Recovery can then skip re-projecting the
	// terminal event and keep retrying cleanup without losing the user-visible result at dead-letter.
	if err := b.store.RecordMatrixEvent(ctx, state.MatrixEventRequest{
		Lease: job.LeaseToken(), At: time.Now().UTC(), Stage: stage, EventID: eventID.String(),
	}); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixDelivery, fmt.Errorf("record terminal Matrix event: %w", err))
	}
	applyDurablePatch(job, patch)
	// Keep the stale-task guard armed until Matrix has accepted and durably recorded the terminal
	// replacement. If either boundary fails, recovery retains the honest fallback.
	if err := b.cancelDurableDeadMan(ctx, job); err != nil {
		// Cleanup is the one recovery boundary that must outlive DELEGATION_MAX_ATTEMPTS: marking the
		// job terminal while Synapse still owns the timer would allow a stale warning after success.
		// The accepted Matrix event is already durable, so retries only repeat the idempotent cancel.
		if retryErr := b.scheduleDurableRetryWithCode(ctx, *job, errorMatrixDelivery, err); retryErr != nil {
			return errors.Join(
				errDeadManCleanupPending,
				fmt.Errorf("cancel durable dead-man event: %w", err),
				retryErr,
			)
		}
		return nil
	}
	terminalState := payload.TerminalState
	if !terminalState.Terminal() {
		terminalState = state.StateDead
		patch.ErrorCode = stringPointer(errorInvalidPayload)
	}
	if err := b.transitionDurable(ctx, job, terminalState, patch); err != nil {
		return err
	}
	if payload.Result != nil {
		replyEventID := eventID
		if stage == state.MatrixEventEdit {
			replyEventID = id.EventID(job.MatrixPlaceholderEventID)
		}
		b.replies.record(agentReplyRef{
			room: id.RoomID(job.RoomID), event: replyEventID, ghost: job.GhostLocalpart,
		})
	}
	b.recordDurableTerminal(*job, evt, ref, sender, payload, eventID, mediaOut, mediaRejected)
	return nil
}

func (b *Bridge) revalidateDurableJob(job state.Job, evt *event.Event) (senderIdentity, *AgentRef, string) {
	return b.revalidateDurableTarget(job, evt, func(ref *AgentRef) bool {
		return ref.MatchesMappingID(job.TargetFingerprint)
	})
}

func (b *Bridge) revalidateDurableTask(
	job state.Job,
	payload durablePayload,
	evt *event.Event,
) (senderIdentity, *AgentRef, string) {
	return b.revalidateDurableTarget(job, evt, func(ref *AgentRef) bool {
		if payload.TargetRouteID == "" {
			return ref.MatchesMappingID(job.TargetFingerprint)
		}
		return ref.RouteID() == payload.TargetRouteID
	})
}

func (b *Bridge) revalidateDurableTarget(
	job state.Job,
	evt *event.Event,
	targetMatches func(*AgentRef) bool,
) (senderIdentity, *AgentRef, string) {
	queuedSender, err := durableSender(job)
	if err != nil {
		return matrixSender(evt.Sender), nil, errorInvalidPayload
	}
	currentSender, ref, ok := b.agents.SnapshotSenderTarget(evt.Sender, job.GhostLocalpart)
	sender := revalidateSender(queuedSender, currentSender)
	if !ok || ref == nil {
		return sender, nil, errorAgentMappingChanged
	}
	if !targetMatches(ref) {
		// Never attribute persisted work to a replacement mapping. The immutable fingerprint remains
		// the only trustworthy actor evidence once the original mapping disappears.
		return sender, nil, errorAgentMappingChanged
	}
	if !ref.AllowsSender(sender, b.cfg.ServerName) {
		return sender, ref, errorSenderPolicy
	}
	if ref.IsDev() && !b.isStagingRoom(evt.RoomID) {
		return sender, ref, errorStagePolicy
	}
	if ref.Target().IsRemote() && (b.client == nil || !b.client.IsReady(ref.Target())) {
		return sender, ref, errorAgentUntrusted
	}
	if ref.Target().IsRemote() && ref.MaxCost() > 0 && b.client != nil {
		switch b.client.QuoteAdmission(ref.Target(), ref.MaxCost()) {
		case a2aclient.QuoteOverBudget, a2aclient.QuoteMissing:
			return sender, ref, errorQuoteOverBudget
		}
	}
	return sender, ref, ""
}

func (b *Bridge) reserveDurableAdmission(
	job state.Job,
	evt *event.Event,
	sender senderIdentity,
) (allowed bool, reason string) {
	senderToken, ok := b.senderLimits.reserve(sender.rateLimitKey(job.GhostLocalpart))
	if !ok {
		return false, errorRateLimit
	}
	_, ok = b.roomLimits.reserve(evt.RoomID.String())
	if !ok {
		// This refusal is known before the durable write begins, so the independent sender budget
		// can be restored without creating an ambiguous persisted-admission boundary.
		senderToken.cancel()
		return false, errorRateLimit
	}
	return true, ""
}

func (b *Bridge) denyDurableJob(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	reason string,
) error {
	if !job.AdmissionChecked {
		if err := b.store.RecordAdmission(ctx, state.AdmissionRequest{
			Lease: job.LeaseToken(), At: time.Now().UTC(), Allowed: false, Reason: reason,
		}); err != nil {
			return err
		}
		job.AdmissionChecked = true
		job.AdmissionAllowed = false
		job.AdmissionReason = reason
	}
	notice := failureMessage(reason, job.GhostLocalpart, 0)
	outcome := outcomeDenied
	if reason == errorRateLimit {
		outcome = outcomeRateLimited
	}
	payload.Audit.RateLimit = durableRateLimitVerdict(*job)
	if job.MatrixPlaceholderEventID == "" && !b.allowNotice(sender, evt.RoomID, job.GhostLocalpart) {
		payload.Audit = durableTerminalAuditState{
			Outcome: outcome, TerminalStage: durableDenialStage(reason), TerminalReason: reason,
			A2AAttempted: payload.Audit.A2AAttempted, A2AStartedAt: payload.Audit.A2AStartedAt,
			RateLimit: payload.Audit.RateLimit,
		}
		return b.finishDurableWithoutReplyWithEvidence(
			ctx, job, state.StateDenied, reason, nil, payload, evt, ref, sender,
		)
	}
	return b.prepareDurableNotice(ctx, job, payload, evt, ref, sender, state.StateDenied,
		notice, outcome, durableDenialStage(reason), reason)
}

func durableDenialStage(reason string) string {
	switch reason {
	case errorAgentUntrusted:
		return "agent_card"
	case errorMediaDenied:
		return "media_admission"
	default:
		return "admission"
	}
}

func (b *Bridge) finishDurableWithoutReply(
	ctx context.Context,
	job *state.Job,
	terminal state.DelegationState,
	errorCode string,
	cause error,
) error {
	payload, evt, decodeErr := decodeDurableJob(*job)
	if decodeErr != nil {
		payload = durablePayload{Version: durablePayloadVersion}
		evt = &event.Event{
			ID: id.EventID(job.MatrixEventID), RoomID: id.RoomID(job.RoomID),
			Sender: id.UserID(job.SenderMXID), Timestamp: job.OriginServerTS,
		}
	}
	sender, senderErr := durableSender(*job)
	if senderErr != nil {
		sender = matrixSender(evt.Sender)
	}
	var ref *AgentRef
	_, currentRef, ok := b.agents.SnapshotSenderTarget(evt.Sender, job.GhostLocalpart)
	if ok && currentRef != nil && currentRef.MatchesMappingID(job.TargetFingerprint) {
		ref = currentRef
	}
	if payload.Audit.Outcome == "" {
		payload.Audit = durableTerminalAuditState{
			Outcome: durableTerminalOutcome(terminal), TerminalStage: "recovery", TerminalReason: errorCode,
			A2AAttempted: payload.Audit.A2AAttempted, A2AStartedAt: payload.Audit.A2AStartedAt,
			RateLimit: durableRateLimitVerdict(*job),
		}
	}
	return b.finishDurableWithoutReplyWithEvidence(
		ctx, job, terminal, errorCode, cause, payload, evt, ref, sender,
	)
}

func (b *Bridge) finishDurableWithoutReplyWithEvidence(
	ctx context.Context,
	job *state.Job,
	terminal state.DelegationState,
	errorCode string,
	cause error,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
) error {
	patch := state.TransitionPatch{ErrorCode: &errorCode}
	if err := b.transitionDurable(ctx, job, terminal, patch); err != nil {
		return err
	}
	eventID := durableTerminalMatrixEventID(*job)
	b.recordDurableTerminal(*job, evt, ref, sender, payload, eventID, 0, 0)
	if cause != nil {
		message := "durable delegation reached terminal state without Matrix projection"
		if eventID != "" {
			message = "durable delegation reached terminal state after Matrix projection cleanup failed"
		}
		b.log.Error(
			message,
			"job_id", job.JobID,
			"state", terminal,
			"error_code", errorCode,
			"matrix_event_id", eventID,
			"error_type", fmt.Sprintf("%T", cause),
		)
	}
	return nil
}

func durableTerminalMatrixEventID(job state.Job) id.EventID {
	if job.MatrixEditEventID != "" {
		return id.EventID(job.MatrixEditEventID)
	}
	return id.EventID(job.MatrixReplyEventID)
}

func durableTerminalOutcome(terminal state.DelegationState) string {
	switch terminal {
	case state.StateDenied:
		return outcomeDenied
	case state.StateAmbiguous:
		return outcomeAmbiguous
	default:
		return outcomeDead
	}
}

func (b *Bridge) transitionDurable(
	ctx context.Context,
	job *state.Job,
	to state.DelegationState,
	patch state.TransitionPatch,
) error {
	from := job.State
	if err := b.store.Transition(ctx, state.TransitionRequest{
		Lease: job.LeaseToken(), From: from, To: to, At: time.Now().UTC(), Patch: patch,
	}); err != nil {
		return fmt.Errorf("transition durable job %s %s -> %s: %w", job.JobID, from, to, err)
	}
	applyDurablePatch(job, patch)
	job.State = to
	job.AttemptCount = 0
	job.PollCount = 0
	durableStateTransitions.WithLabelValues(string(from), string(to)).Inc()
	b.auditLog.Info(
		"delegation ledger transition",
		"audit_schema", durableAuditSchema,
		"job_id", job.JobID,
		"matrix_event_id", job.MatrixEventID,
		"room_id", job.RoomID,
		"ghost", job.GhostLocalpart,
		"target_fingerprint", job.TargetFingerprint,
		"from_state", from,
		"to_state", to,
		"lease_generation", job.LeaseGeneration,
		"error_code", job.ErrorCode,
	)
	return nil
}

func (b *Bridge) retryOrDead(ctx context.Context, job *state.Job, code string, cause error) error {
	if job.AttemptCount+1 >= b.cfg.DelegationMaxAttempts {
		return b.deadLetterDurableJob(ctx, job, code, cause)
	}
	return b.scheduleDurableRetryWithCode(ctx, *job, code, cause)
}

func (b *Bridge) deadLetterDurableJob(
	ctx context.Context,
	job *state.Job,
	code string,
	cause error,
) error {
	if job.State == state.StateReplyPending {
		terminal := state.StateDead
		terminalCode := code
		if payload, _, err := decodeDurableJob(*job); err == nil {
			switch payload.TerminalState {
			case state.StateAmbiguous, state.StateDenied, state.StateDead:
				terminal = payload.TerminalState
				if job.ErrorCode != "" {
					terminalCode = job.ErrorCode
				}
			case state.StateDelivered:
				// Cancellation cleanup cannot erase an accepted Matrix result. The persisted event
				// remains the delivery proof even if the homeserver timer could not be cancelled.
				if durableTerminalMatrixEventID(*job) != "" {
					terminal = state.StateDelivered
				}
			}
		}
		return b.finishDurableWithoutReply(ctx, job, terminal, terminalCode, cause)
	}
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, errorInvalidPayload, err)
	}
	sender, ref, _ := b.revalidateDurableJob(*job, evt)
	return b.prepareDurableFailureNotice(
		ctx,
		job,
		payload,
		evt,
		ref,
		sender,
		state.StateDead,
		outcomeDead,
		"recovery",
		code,
		0,
	)
}

func (b *Bridge) scheduleDurableRetryWithCode(
	ctx context.Context,
	job state.Job,
	code string,
	cause error,
) error {
	delay := durableBackoff(b.cfg.DelegationRetryInitial, b.cfg.DelegationRetryMax, job.AttemptCount+1)
	now := time.Now().UTC()
	if err := b.store.ScheduleRetry(ctx, state.RetryRequest{
		Lease: job.LeaseToken(), At: now, NextAttemptAt: now.Add(delay), ErrorCode: code,
		Kind: state.RetryFailure,
	}); err != nil {
		return fmt.Errorf("schedule durable retry: %w", err)
	}
	if cause != nil {
		b.log.Warn(
			"durable delegation scheduled for retry",
			"job_id", job.JobID,
			"state", job.State,
			"error_code", code,
			"delay", delay,
			"error_type", fmt.Sprintf("%T", cause),
		)
	}
	return nil
}

func (b *Bridge) scheduleTaskPoll(ctx context.Context, job state.Job, deadManRefreshFailed bool) error {
	now := time.Now().UTC()
	delay := durableBackoff(b.pollInitial, b.pollMax, job.PollCount+1)
	errorCode := "task_working"
	if deadManRefreshFailed {
		// Persist the failed refresh in the poll cursor so the next worker retries it even when
		// the ordinary modulo cadence would skip that poll after a process restart.
		delay = min(delay, b.deadManRefreshRetryInterval())
		errorCode = errorDeadManRefresh
	}
	if err := b.store.ScheduleRetry(ctx, state.RetryRequest{
		Lease: job.LeaseToken(), At: now, NextAttemptAt: now.Add(delay), ErrorCode: errorCode,
		Kind: state.RetryPoll,
	}); err != nil {
		return fmt.Errorf("schedule durable task poll: %w", err)
	}
	return nil
}

func durableBackoff(initial, maximum time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := min(attempt-1, 30)
	if exponent >= 63 || initial > maximum/time.Duration(1<<exponent) {
		return maximum
	}
	return min(initial*time.Duration(1<<exponent), maximum)
}

func (b *Bridge) durableTaskDeadline(
	job state.Job,
	ref *AgentRef,
	payload durablePayload,
) (time.Time, time.Duration) {
	limit := agentRequestTimeout(ref, b.cfg.TaskTimeout)
	startedAt := job.CreatedAt
	if payload.Audit.A2AStartedAt > 0 {
		startedAt = time.UnixMilli(payload.Audit.A2AStartedAt)
	}
	return startedAt.Add(limit), limit
}

func decodeDurableJob(job state.Job) (durablePayload, *event.Event, error) {
	var payload durablePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return durablePayload{}, nil, fmt.Errorf("decode durable payload: %w", err)
	}
	if payload.Version != durablePayloadVersion || len(payload.Event) == 0 {
		return durablePayload{}, nil, fmt.Errorf("unsupported durable payload version %d", payload.Version)
	}
	var evt event.Event
	if err := json.Unmarshal(payload.Event, &evt); err != nil {
		return durablePayload{}, nil, fmt.Errorf("decode durable Matrix event: %w", err)
	}
	evt.Type.Class = event.MessageEventType
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		return durablePayload{}, nil, fmt.Errorf("parse durable Matrix event content: %w", err)
	}
	if evt.ID.String() != job.MatrixEventID || evt.RoomID.String() != job.RoomID ||
		evt.Sender.String() != job.SenderMXID || evt.Timestamp != job.OriginServerTS {
		return durablePayload{}, nil, fmt.Errorf("durable Matrix event evidence does not match ledger")
	}
	return payload, &evt, nil
}

func durableSender(job state.Job) (senderIdentity, error) {
	mxid := id.UserID(job.SenderMXID)
	if mxid == "" {
		return senderIdentity{}, fmt.Errorf("empty durable sender MXID")
	}
	kind := senderOriginKind(job.SenderOriginKind)
	if kind != senderOriginMatrix && kind != senderOriginBridge {
		return senderIdentity{}, fmt.Errorf("invalid durable sender origin kind %q", kind)
	}
	if job.SenderOriginNetwork == "" {
		return senderIdentity{}, fmt.Errorf("empty durable sender origin network")
	}
	return senderIdentity{mxid: mxid, origin: senderOrigin{kind: kind, network: job.SenderOriginNetwork}}, nil
}

func applyDurablePatch(job *state.Job, patch state.TransitionPatch) {
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

func (b *Bridge) recordDurableTerminal(
	job state.Job,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	payload durablePayload,
	eventID id.EventID,
	mediaOut, mediaRejected int,
) {
	outcome := payload.Audit.Outcome
	if outcome == "" {
		outcome = outcomeDead
	}
	delegationsTotal.WithLabelValues(job.GhostLocalpart, outcome).Inc()
	if job.State == state.StateAmbiguous || job.State == state.StateDead {
		durableRecoveryOutcomes.WithLabelValues(string(job.State)).Inc()
	}
	if ref == nil {
		ref = &AgentRef{}
	}
	rateLimitVerdict := payload.Audit.RateLimit
	if rateLimitVerdict == "" {
		rateLimitVerdict = durableRateLimitVerdict(job)
	}
	b.logDelegationAudit(evt, ref, job.GhostLocalpart, sender, delegationAuditResult{
		outcome:           outcome,
		terminalStage:     payload.Audit.TerminalStage,
		terminalReason:    payload.Audit.TerminalReason,
		dedupVerdict:      dedupVerdictAccepted,
		rateLimitVerdict:  rateLimitVerdict,
		a2aAttempted:      payload.Audit.A2AAttempted,
		a2aUserID:         job.SenderMXID,
		contextID:         job.A2AContextID,
		taskID:            job.A2ATaskID,
		replyEventID:      eventID,
		activated:         durableActivatedExtensions(payload),
		mediaIn:           payload.MediaIn,
		mediaOut:          mediaOut,
		mediaRejected:     payload.MediaRejected + mediaRejected,
		targetFingerprint: job.TargetFingerprint,
		agentVersion:      payload.AgentVersion,
		agentContract:     payload.AgentContract,
		duration:          time.Since(job.CreatedAt),
	})
}

func durableRateLimitVerdict(job state.Job) auditRateLimitVerdict {
	if !job.AdmissionChecked {
		return rateLimitVerdictNotChecked
	}
	if !job.AdmissionAllowed && job.AdmissionReason == errorRateLimit {
		return rateLimitVerdictRejected
	}
	if !job.AdmissionAllowed {
		return rateLimitVerdictNotChecked
	}
	return rateLimitVerdictAllowed
}

func durableActivatedExtensions(payload durablePayload) []string {
	if payload.Result == nil {
		return nil
	}
	return payload.Result.ActivatedExtensions
}

func stringPointer(value string) *string { return &value }

func captureDurableA2AEvidence(job *state.Job, result a2aclient.Result) {
	if result.TaskID != "" {
		job.A2ATaskID = result.TaskID
	}
	if result.ContextID != "" {
		job.A2AContextID = result.ContextID
	}
}
