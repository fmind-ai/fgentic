package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	errorControlAmbiguous = "control_ack_ambiguous"
	errorControlFailed    = "control_projection_failed"
	errorInputWaitTimeout = "input_wait_timeout"
)

// executeDurableControls drains the bounded control outbox before ordinary task work. A control
// that finishes or advances the delegation returns stop=true because its handler already persisted
// the next job state. Matrix-only projections are replayable and may be drained in one lease.
func (b *Bridge) executeDurableControls(ctx context.Context, job *state.Job) (stop bool, err error) {
	for range b.durableControlCapacity() {
		control, found, err := b.store.ClaimControl(ctx, job.LeaseToken(), time.Now().UTC())
		if err != nil || !found {
			return false, err
		}
		stop, err = b.executeDurableControl(ctx, job, control)
		if stop || err != nil {
			return stop, err
		}
	}
	return false, fmt.Errorf("durable control drain exceeded configured capacity")
}

func (b *Bridge) executeDurableControl(ctx context.Context, job *state.Job, control state.Control) (bool, error) {
	if control.RecoveryCount > 0 &&
		(control.Kind == state.ControlCancel || control.Kind == state.ControlContinuation) {
		if err := b.finishControl(ctx, *job, control, state.ControlAmbiguous, errorControlAmbiguous, ""); err != nil {
			return true, err
		}
		return true, b.finishAmbiguousControl(ctx, job, control)
	}
	switch control.Kind {
	case state.ControlCancel:
		return true, b.executeDurableCancel(ctx, job, control)
	case state.ControlContinuation:
		return true, b.executeDurableContinuation(ctx, job, control)
	case state.ControlQuestion:
		return false, b.projectDurableQuestion(ctx, *job, control)
	case state.ControlProgress:
		return false, b.projectDurableProgress(ctx, *job, control)
	case state.ControlPin:
		return false, b.projectDurablePin(ctx, *job, control, true)
	case state.ControlUnpin:
		return false, b.projectDurablePin(ctx, *job, control, false)
	default:
		return false, b.finishControl(ctx, *job, control, state.ControlDead, errorControlFailed, "")
	}
}

func (b *Bridge) executeDurableCancel(ctx context.Context, job *state.Job, control state.Control) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return err
	}
	sender, ref, denial := b.revalidateDurableTask(ctx, *job, payload, evt)
	if denial != "" || job.A2ATaskID == "" {
		if denial == "" {
			denial = errorTaskInvalid
		}
		if err := b.finishControl(ctx, *job, control, state.ControlDenied, denial, ""); err != nil {
			return err
		}
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, denial)
	}
	client, ok := b.client.(durableA2AClient)
	if !ok {
		return b.finishControl(ctx, *job, control, state.ControlDead, "durable_a2a_unsupported", "")
	}
	cancelCtx, cancel := context.WithTimeout(
		withAgentPolicyContext(ctx, control.AuthorizedSender, ref), b.cfg.RequestTimeout,
	)
	err = client.CancelTask(cancelCtx, ref.Target(), job.A2ATaskID)
	cancel()
	if err != nil {
		if finishErr := b.finishControl(ctx, *job, control, state.ControlAmbiguous, errorControlAmbiguous, ""); finishErr != nil {
			return finishErr
		}
		return b.prepareDurableNotice(
			ctx, job, payload, evt, ref, sender, state.StateAmbiguous,
			fmt.Sprintf("⚠️ cancellation requested by %s; the agent acknowledgement was lost.", control.AuthorizedSender),
			outcomeAmbiguous, "task_cancel", errorControlAmbiguous,
		)
	}
	if err := b.finishControl(ctx, *job, control, state.ControlApplied, "", ""); err != nil {
		return err
	}
	payload.Audit.CanceledBy = control.AuthorizedSender
	return b.prepareDurableNotice(
		ctx, job, payload, evt, ref, sender, state.StateDelivered,
		fmt.Sprintf("🛑 canceled by %s.", control.AuthorizedSender),
		outcomeCanceled, "task_cancel", "canceled_by_room",
	)
}

func (b *Bridge) executeDurableContinuation(ctx context.Context, job *state.Job, control state.Control) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return err
	}
	sender, ref, denial := b.revalidateDurableTask(ctx, *job, payload, evt)
	if denial != "" {
		if err := b.finishControl(ctx, *job, control, state.ControlDenied, denial, ""); err != nil {
			return err
		}
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, denial)
	}
	controls, err := b.store.Controls(ctx, job.JobID)
	if err != nil {
		return err
	}
	for _, previous := range controls {
		if previous.ControlID != control.ControlID && previous.Kind == state.ControlContinuation &&
			previous.Slot == control.Slot && previous.State == state.ControlApplied {
			if err := b.finishControl(ctx, *job, control, state.ControlDenied, "continuation_already_applied", ""); err != nil {
				return err
			}
			return b.releaseAfterRejectedContinuation(ctx, *job)
		}
	}
	if job.State != state.StateAwaitingInput || job.A2ATaskID == "" || job.A2AContextID == "" {
		if err := b.finishControl(ctx, *job, control, state.ControlDenied, errorTaskInvalid, ""); err != nil {
			return err
		}
		return b.releaseAfterRejectedContinuation(ctx, *job)
	}
	if !job.InputWaitExpiresAt.IsZero() && !time.Now().Before(job.InputWaitExpiresAt) {
		if err := b.finishControl(ctx, *job, control, state.ControlDenied, errorInputWaitTimeout, ""); err != nil {
			return err
		}
		return b.expireDurableInput(ctx, job, payload, evt, ref, sender)
	}
	allowed, reason := b.reserveDurableAdmission(*job, evt, sender)
	if !allowed {
		if err := b.finishControl(ctx, *job, control, state.ControlDenied, reason, ""); err != nil {
			return err
		}
		return b.releaseAfterRejectedContinuation(ctx, *job)
	}
	client, ok := b.client.(durableA2AClient)
	if !ok {
		return b.finishControl(ctx, *job, control, state.ControlDead, "durable_a2a_unsupported", "")
	}
	deadline := job.TaskDeadlineAt
	if deadline.IsZero() {
		deadline, _ = b.durableTaskDeadline(*job, ref, payload)
	}
	if !job.InputWaitStartedAt.IsZero() {
		deadline = deadline.Add(time.Since(job.InputWaitStartedAt))
	}
	a2aCtx, cancelOverall := context.WithDeadline(
		withAgentPolicyContext(ctx, control.AuthorizedSender, ref), deadline,
	)
	defer cancelOverall()
	callCtx, cancel := context.WithTimeout(a2aCtx, b.cfg.RequestTimeout)
	result, err := client.ContinueWithMessageID(
		callCtx, ref.Target(), control.A2AMessageID, provenancePrompt(evt, string(control.Payload)),
		job.A2AContextID, job.A2ATaskID,
	)
	cancel()
	if err != nil {
		if finishErr := b.finishControl(ctx, *job, control, state.ControlAmbiguous, errorControlAmbiguous, ""); finishErr != nil {
			return finishErr
		}
		return b.finishAmbiguousControl(ctx, job, control)
	}
	if result.TaskID != "" && result.TaskID != job.A2ATaskID ||
		result.ContextID != "" && result.ContextID != job.A2AContextID {
		if finishErr := b.finishControl(ctx, *job, control, state.ControlDead, errorTaskInvalid, ""); finishErr != nil {
			return finishErr
		}
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDead,
			outcomeDead, "task_input", errorTaskInvalid, 0,
		)
	}
	if err := b.finishControl(ctx, *job, control, state.ControlApplied, "", ""); err != nil {
		return err
	}
	// The next result transition persists the extended deadline together with clearing or resetting
	// the input window, which keeps the awaiting_input SQL invariant atomic.
	job.TaskDeadlineAt = deadline
	return b.acceptDurableA2AResult(ctx, job, payload, evt, ref, sender, result)
}

func (b *Bridge) releaseAfterRejectedContinuation(ctx context.Context, job state.Job) error {
	switch job.State {
	case state.StateAwaitingInput:
		return b.scheduleDurableAt(ctx, job, job.InputWaitExpiresAt, errorInputRequired)
	case state.StateAwaitingTask:
		return b.scheduleTaskPoll(ctx, job)
	default:
		return nil
	}
}

func (b *Bridge) projectDurableQuestion(ctx context.Context, job state.Job, control state.Control) error {
	if job.MatrixPlaceholderEventID == "" {
		return b.finishControl(ctx, job, control, state.ControlDead, errorControlFailed, "")
	}
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	// The input-required question is a non-terminal pause (the task resumes on a threaded reply), so it
	// carries no result block (nil meta).
	eventID, err := b.editDurableNotice(
		ctx, intent, id.RoomID(job.RoomID), id.EventID(job.MatrixPlaceholderEventID),
		string(control.Payload), control.MatrixTxnID, nil,
	)
	if err != nil {
		return err
	}
	return b.finishControl(ctx, job, control, state.ControlApplied, "", eventID.String())
}

func (b *Bridge) projectDurableProgress(ctx context.Context, job state.Job, control state.Control) error {
	if job.MatrixPlaceholderEventID == "" || len(control.Payload) == 0 {
		return b.finishControl(ctx, job, control, state.ControlDead, errorControlFailed, "")
	}
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: string(control.Payload)}
	content.RelatesTo = &event.RelatesTo{Type: event.RelThread, EventID: id.EventID(job.MatrixPlaceholderEventID)}
	response, err := sendMessageEvent(
		ctx, b.as.Intent(id.UserID(job.GhostMXID)), id.RoomID(job.RoomID), event.EventMessage, automatedContent(content),
		mautrix.ReqSendEvent{TransactionID: control.MatrixTxnID},
	)
	if err != nil {
		return err
	}
	if response == nil || response.EventID == "" {
		return fmt.Errorf("project durable progress: empty Matrix event ID")
	}
	return b.finishControl(ctx, job, control, state.ControlApplied, "", response.EventID.String())
}

func (b *Bridge) projectDurablePin(ctx context.Context, job state.Job, control state.Control, pin bool) error {
	if job.MatrixPlaceholderEventID == "" {
		return b.finishControl(ctx, job, control, state.ControlDead, errorControlFailed, "")
	}
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	roomID := id.RoomID(job.RoomID)
	placeholder := id.EventID(job.MatrixPlaceholderEventID)
	var current event.PinnedEventsEventContent
	if err := intent.Client.StateEvent(ctx, roomID, event.StatePinnedEvents, "", &current); err != nil &&
		!errors.Is(err, mautrix.MNotFound) {
		return b.finishControl(ctx, job, control, state.ControlDead, "pin_state_unavailable", "")
	}
	next := slices.Clone(current.Pinned)
	changed := false
	if pin && !slices.Contains(next, placeholder) {
		next = append(next, placeholder)
		changed = true
	} else if !pin && slices.Contains(next, placeholder) {
		next = slices.DeleteFunc(next, func(item id.EventID) bool { return item == placeholder })
		changed = true
	}
	if !changed {
		return b.finishControl(ctx, job, control, state.ControlApplied, "", "")
	}
	response, err := sendStateEvent(
		ctx, intent, roomID, event.StatePinnedEvents, "", &event.PinnedEventsEventContent{Pinned: next},
		mautrix.ReqSendEvent{TransactionID: control.MatrixTxnID},
	)
	if err != nil {
		// Pin visibility is optional. Persist the fixed failure instead of retrying or failing work.
		return b.finishControl(ctx, job, control, state.ControlDead, "pin_power_rejected", "")
	}
	eventID := ""
	if response != nil {
		eventID = response.EventID.String()
	}
	return b.finishControl(ctx, job, control, state.ControlApplied, "", eventID)
}

func (b *Bridge) finishControl(
	ctx context.Context,
	job state.Job,
	control state.Control,
	to state.ControlState,
	errorCode, matrixEventID string,
) error {
	patch := state.ControlTransitionPatch{ErrorCode: stringPointer(errorCode)}
	if matrixEventID != "" {
		patch.MatrixEventID = stringPointer(matrixEventID)
	}
	if err := b.store.TransitionControl(ctx, state.ControlTransitionRequest{
		Lease: job.LeaseToken(), ControlID: control.ControlID, From: state.ControlPrepared,
		To: to, At: time.Now().UTC(), Patch: patch,
	}); err != nil {
		return fmt.Errorf("finish durable %s control: %w", control.Kind, err)
	}
	b.auditLog.Info(
		"delegation control transition",
		"audit_schema", durableAuditSchema,
		"job_id", job.JobID,
		"control_id", control.ControlID,
		"source_matrix_event_id", control.SourceMatrixEventID,
		"control_kind", control.Kind,
		"authorized_sender", control.AuthorizedSender,
		"control_state", to,
		"recovery_count", control.RecoveryCount,
		"error_code", errorCode,
	)
	return nil
}

func (b *Bridge) finishAmbiguousControl(ctx context.Context, job *state.Job, control state.Control) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return err
	}
	sender, ref, _ := b.revalidateDurableTask(ctx, *job, payload, evt)
	notice := fmt.Sprintf("⚠️ %s requested by %s may have reached the agent; the acknowledgement was lost.",
		strings.ReplaceAll(string(control.Kind), "_", " "), control.AuthorizedSender)
	return b.prepareDurableNotice(
		ctx, job, payload, evt, ref, sender, state.StateAmbiguous,
		notice, outcomeAmbiguous, "task_control", errorControlAmbiguous,
	)
}

func (b *Bridge) pauseDurableForInput(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	result a2aclient.Result,
) error {
	captureDurableA2AEvidence(job, result)
	if job.A2ATaskID == "" || job.A2AContextID == "" {
		return b.prepareDurableFailureNotice(
			ctx, job, payload, evt, ref, sender, state.StateDead,
			outcomeDead, "task_input", errorTaskInvalid, 0,
		)
	}
	if err := b.ensureDurablePlaceholder(ctx, job, evt); err != nil {
		return b.retryOrDead(ctx, job, errorMatrixDelivery, err)
	}
	if err := b.ensureDurableDeadMan(ctx, job, evt); err != nil {
		return err
	}
	now := time.Now().UTC()
	deadline := job.TaskDeadlineAt
	if deadline.IsZero() {
		deadline, _ = b.durableTaskDeadline(*job, ref, payload)
	}
	question := strings.TrimSpace(result.Text)
	if question == "" {
		question = "The agent needs more information to continue."
	}
	question = fmt.Sprintf("❓ %s\n\n(reply in this thread within %s to continue)", question, b.cfg.InputWaitTimeout)
	expires := now.Add(b.cfg.InputWaitTimeout)
	if err := b.transitionDurable(ctx, job, state.StateAwaitingInput, state.TransitionPatch{
		A2ATaskID: stringPointer(job.A2ATaskID), A2AContextID: stringPointer(job.A2AContextID),
		TaskDeadlineAt: &deadline, InputWaitStartedAt: &now, InputWaitExpiresAt: &expires,
		ResultText: &question, ErrorCode: stringPointer(errorInputRequired),
	}); err != nil {
		return err
	}
	if err := b.ensureDurableQuestion(ctx, job); err != nil {
		return err
	}
	delegationsTotal.WithLabelValues(job.GhostLocalpart, outcomeInputRequired).Inc()
	return b.scheduleDurableAt(ctx, *job, expires, errorInputRequired)
}

func (b *Bridge) resumeAwaitingInput(ctx context.Context, job *state.Job) error {
	payload, evt, err := decodeDurableJob(*job)
	if err != nil {
		return b.finishDurableWithoutReply(ctx, job, state.StateDead, errorInvalidPayload, err)
	}
	sender, ref, denial := b.revalidateDurableTask(ctx, *job, payload, evt)
	if denial != "" {
		return b.denyDurableJob(ctx, job, payload, evt, ref, sender, denial)
	}
	if err := b.ensureDurableQuestion(ctx, job); err != nil {
		return err
	}
	if job.InputWaitExpiresAt.IsZero() || !time.Now().Before(job.InputWaitExpiresAt) {
		return b.expireDurableInput(ctx, job, payload, evt, ref, sender)
	}
	return b.scheduleDurableAt(ctx, *job, job.InputWaitExpiresAt, errorInputRequired)
}

func (b *Bridge) ensureDurableQuestion(ctx context.Context, job *state.Job) error {
	question := strings.TrimSpace(job.ResultText)
	if question == "" {
		return nil
	}
	controls, err := b.store.Controls(ctx, job.JobID)
	if err != nil {
		return fmt.Errorf("list durable input questions: %w", err)
	}
	slot := 0
	currentPlanned := false
	for _, control := range controls {
		if control.Kind != state.ControlQuestion {
			continue
		}
		if control.Slot >= slot {
			slot = control.Slot + 1
		}
		if !control.CreatedAt.Before(job.InputWaitStartedAt) {
			currentPlanned = true
		}
	}
	if !currentPlanned {
		if _, err := b.store.PlanControl(ctx, state.PlanControlRequest{
			Lease: job.LeaseToken(), At: time.Now().UTC(), Kind: state.ControlQuestion, Slot: slot,
			Capacity: b.durableControlCapacity(), Payload: []byte(question),
		}); err != nil {
			return fmt.Errorf("plan durable input question: %w", err)
		}
	}
	empty := ""
	if err := b.transitionDurable(ctx, job, state.StateAwaitingInput, state.TransitionPatch{
		ResultText: &empty,
	}); err != nil {
		return fmt.Errorf("clear durable input question recovery payload: %w", err)
	}
	if stop, err := b.executeDurableControls(ctx, job); stop || err != nil {
		return err
	}
	return nil
}

func (b *Bridge) expireDurableInput(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
) error {
	return b.prepareDurableNotice(
		ctx, job, payload, evt, ref, sender, state.StateDelivered,
		fmt.Sprintf("⌛ agent %q got no reply within %s — the task was dropped.", job.GhostLocalpart, b.cfg.InputWaitTimeout),
		outcomeTimeout, "task_input", errorInputWaitTimeout,
	)
}

func (b *Bridge) scheduleDurableAt(ctx context.Context, job state.Job, next time.Time, code string) error {
	now := time.Now().UTC()
	if next.Before(now) {
		next = now
	}
	if err := b.store.ScheduleRetry(ctx, state.RetryRequest{
		Lease: job.LeaseToken(), At: now, NextAttemptAt: next, ErrorCode: code, Kind: state.RetryPoll,
	}); err != nil {
		return fmt.Errorf("schedule durable control wait: %w", err)
	}
	return nil
}

func (b *Bridge) surfaceDurableProgress(ctx context.Context, job *state.Job, text string) error {
	text = strings.TrimSpace(text)
	if b.cfg.MaxTaskProgressPosts <= 0 || text == "" || job.MatrixPlaceholderEventID == "" {
		return nil
	}
	controls, err := b.store.Controls(ctx, job.JobID)
	if err != nil {
		return err
	}
	posted := 0
	for _, control := range controls {
		if control.Kind == state.ControlProgress {
			posted++
		}
	}
	if posted >= b.cfg.MaxTaskProgressPosts {
		return nil
	}
	if _, err := b.store.PlanControl(ctx, state.PlanControlRequest{
		Lease: job.LeaseToken(), At: time.Now().UTC(), Kind: state.ControlProgress, Slot: posted,
		Capacity: b.durableControlCapacity(), Payload: []byte(text),
	}); err != nil {
		return fmt.Errorf("plan durable progress: %w", err)
	}
	stop, err := b.executeDurableControls(ctx, job)
	if stop {
		return state.ErrLeaseLost
	}
	return err
}

func (b *Bridge) ensureDurablePin(ctx context.Context, job *state.Job, pin bool) error {
	if !b.cfg.PinInFlightTasks || job.MatrixPlaceholderEventID == "" {
		return nil
	}
	kind := state.ControlUnpin
	if pin {
		kind = state.ControlPin
	}
	if _, err := b.store.PlanControl(ctx, state.PlanControlRequest{
		Lease: job.LeaseToken(), At: time.Now().UTC(), Kind: kind, Slot: 0,
		Capacity: b.durableControlCapacity(),
	}); err != nil {
		return fmt.Errorf("plan durable %s: %w", kind, err)
	}
	stop, err := b.executeDurableControls(ctx, job)
	if stop {
		return state.ErrLeaseLost
	}
	return err
}

func (b *Bridge) durableControlCapacity() int {
	if b.cfg.ControlCapacityPerJob >= 5 {
		return b.cfg.ControlCapacityPerJob
	}
	// Unit fixtures historically construct Config directly; production parsing rejects this zero.
	return 16
}
