package bridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

// transactionLedger is the narrow durable intake surface used before the homeserver receives an
// appservice acknowledgement. Keeping it local makes the atomicity boundary explicit without
// coupling Matrix HTTP handling to the rest of the state implementation.
type transactionLedger interface {
	AdmitTransaction(context.Context, state.TransactionAdmission) (state.AdmissionResult, error)
}

// durableIntakeGate closes while an appservice transaction is durably admitted but has not yet
// been consumed by mautrix. A channel snapshot lets already-running work continue while new jobs
// wait without holding the gate mutex across transaction processing.
type durableIntakeGate struct {
	mu      sync.Mutex
	pending int
	open    chan struct{}
}

func (g *durableIntakeGate) hold() func() {
	g.mu.Lock()
	if g.pending == 0 {
		g.open = make(chan struct{})
	}
	g.pending++
	g.mu.Unlock()

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.pending--
			if g.pending == 0 {
				close(g.open)
				g.open = nil
			}
		})
	}
}

func (g *durableIntakeGate) wait(ctx context.Context) error {
	g.mu.Lock()
	if g.pending == 0 {
		g.mu.Unlock()
		return nil
	}
	open := g.open
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-open:
		return nil
	}
}

// HoldDurableExecutionUntilTransactionConsumed closes the execution gate before pre-ACK durable
// admission. The returned idempotent release must run after mautrix has consumed the transaction.
func (b *Bridge) HoldDurableExecutionUntilTransactionConsumed() func() {
	return b.durableIntakeGate.hold()
}

// NotifyDurableQueue wakes the ledger coordinator after mautrix has consumed an accepted
// transaction or exact replay. It is safe before Start and coalesces concurrent notifications.
func (b *Bridge) NotifyDurableQueue() {
	if b.durableQueue != nil {
		b.durableQueue.Notify()
	}
}

func (b *Bridge) executeDurableJobAfterTransactionConsumption(ctx context.Context, job state.Job) {
	if b.durableIntakeGate.wait(ctx) != nil {
		return
	}
	b.executeDurableJob(ctx, job)
}

// AdmitAppserviceTransaction parses the exact homeserver request bytes, derives every eligible
// per-target delegation, and commits the transaction hash plus jobs atomically. It does not enqueue
// mautrix events: the intake adapter does that only after this method succeeds, preserving all
// ordinary state, device, membership, reaction, and command handling.
func (b *Bridge) AdmitAppserviceTransaction(
	ctx context.Context,
	transactionID string,
	body []byte,
) (state.AdmissionResult, error) {
	ledger, ok := b.store.(transactionLedger)
	if !ok {
		return state.AdmissionResult{}, fmt.Errorf("bridge state does not support durable transaction admission")
	}
	delegations, err := b.delegationsFromTransaction(body)
	if err != nil {
		return state.AdmissionResult{}, err
	}
	committedAt := time.Now().UTC()
	result, err := ledger.AdmitTransaction(ctx, state.TransactionAdmission{
		TransactionID:  transactionID,
		BodyHash:       state.HashTransaction(body),
		CommittedAt:    committedAt,
		RoomCapacity:   b.cfg.RoomQueueCapacity,
		GlobalCapacity: b.cfg.GlobalQueueCapacity,
		Delegations:    delegations,
	})
	if err != nil {
		if errors.Is(err, state.ErrTransactionHashConflict) {
			b.logTransactionHashConflict(transactionID, err)
		}
		return state.AdmissionResult{}, fmt.Errorf("admit appservice transaction %q: %w", transactionID, err)
	}
	if b.durableQueue != nil {
		b.durableQueue.ObserveAdmission(len(result.InsertedJobIDs))
		if result.Disposition == state.TransactionReplay || len(result.ExistingJobIDs) > 0 {
			b.durableQueue.ReconcileAdmissionReplay(ctx)
		}
	}
	if result.Disposition == state.TransactionAccepted {
		b.recordCapacityDenials(delegations, result.CapacityDenied, committedAt)
	}
	deduplicated := len(result.ExistingJobIDs) + len(result.LegacyTombstonedJobIDs)
	if result.Disposition == state.TransactionReplay {
		// Exact transaction replay returns no per-job list, but still represents one durable dedup
		// decision for the established metric contract.
		deduplicated++
	}
	if deduplicated > 0 {
		dedupSkipsTotal.Add(float64(deduplicated))
	}
	return result, nil
}

// logTransactionHashConflict records the tamper signal without request content or error strings.
// Both hashes are safe correlation evidence and let an operator distinguish repeated attempts.
func (b *Bridge) logTransactionHashConflict(transactionID string, err error) {
	fields := []any{
		"audit_schema", "fgentic.appservice_transaction.v1",
		"transaction_id", transactionID,
		"outcome", "rejected",
		"terminal_reason", "transaction_hash_conflict",
	}
	var conflict *state.TransactionHashConflictError
	if errors.As(err, &conflict) {
		fields = append(
			fields,
			"stored_body_sha256", hex.EncodeToString(conflict.Stored[:]),
			"received_body_sha256", hex.EncodeToString(conflict.Received[:]),
		)
	}
	b.auditLog.Warn("appservice transaction conflict", fields...)
}

// recordCapacityDenials preserves the existing bounded-queue metric and terminal audit contract
// for jobs refused atomically by the durable ledger. The state row is already a content-free
// tombstone; the original admission evidence is used only synchronously and is never logged.
func (b *Bridge) recordCapacityDenials(
	delegations []state.NewDelegation,
	denials []state.CapacityDenial,
	committedAt time.Time,
) {
	byJobID := make(map[string]state.NewDelegation, len(delegations))
	for _, delegation := range delegations {
		byJobID[state.JobIDFor(delegation.MatrixEventID, delegation.GhostMXID)] = delegation
	}
	for _, denial := range denials {
		delegation, ok := byJobID[denial.JobID]
		if !ok {
			b.log.Error("durable capacity denial has no admission evidence",
				"job_id", denial.JobID, "reason", denial.Reason)
			continue
		}
		job := state.Job{
			JobID:               denial.JobID,
			MatrixEventID:       delegation.MatrixEventID,
			GhostMXID:           delegation.GhostMXID,
			GhostLocalpart:      delegation.GhostLocalpart,
			RoomID:              delegation.RoomID,
			SenderMXID:          delegation.SenderMXID,
			SenderOriginKind:    delegation.SenderOriginKind,
			SenderOriginNetwork: delegation.SenderOriginNetwork,
			OriginServerTS:      delegation.OriginServerTS,
			TargetFingerprint:   delegation.TargetFingerprint,
			Payload:             delegation.Payload,
			AdmissionChecked:    true,
			AdmissionAllowed:    false,
			AdmissionReason:     denial.Reason,
			State:               state.StateDenied,
			ErrorCode:           denial.Reason,
			CreatedAt:           committedAt,
			UpdatedAt:           committedAt,
			TerminalAt:          committedAt,
		}
		_, evt, err := decodeDurableJob(job)
		if err != nil {
			b.log.Error("durable capacity denial has invalid event evidence",
				"job_id", denial.JobID, "reason", "invalid_recovery_evidence")
			continue
		}
		sender, err := durableSender(job)
		if err != nil {
			b.log.Error("durable capacity denial has invalid sender evidence",
				"job_id", denial.JobID, "reason", "invalid_sender_evidence")
			continue
		}
		ref := &AgentRef{}
		if current, found := b.agents.Lookup(job.GhostLocalpart); found &&
			current.MappingID() == job.TargetFingerprint {
			ref = current
		}

		delegationsTotal.WithLabelValues(job.GhostLocalpart, outcomeQueueFull).Inc()
		b.log.Warn(
			"rejecting delegation because durable capacity is exhausted",
			"job_id", job.JobID,
			"ghost", job.GhostLocalpart,
			"room", job.RoomID,
			"reason", denial.Reason,
		)
		b.logDelegationAudit(evt, ref, job.GhostLocalpart, sender, delegationAuditResult{
			outcome:           outcomeQueueFull,
			terminalStage:     "queue",
			terminalReason:    denial.Reason,
			duration:          time.Since(committedAt),
			dedupVerdict:      dedupVerdictAccepted,
			rateLimitVerdict:  rateLimitVerdictNotChecked,
			targetFingerprint: job.TargetFingerprint,
		})
	}
}

func (b *Bridge) delegationsFromTransaction(body []byte) ([]state.NewDelegation, error) {
	var transaction appservice.Transaction
	if err := json.Unmarshal(body, &transaction); err != nil {
		return nil, fmt.Errorf("parse appservice transaction: %w", err)
	}

	delegations := make([]state.NewDelegation, 0)
	for index, evt := range transaction.Events {
		jobs, err := b.delegationsFromEvent(evt)
		if err != nil {
			return nil, fmt.Errorf("classify appservice event %d: %w", index, err)
		}
		delegations = append(delegations, jobs...)
	}
	return delegations, nil
}

func (b *Bridge) delegationsFromEvent(evt *event.Event) ([]state.NewDelegation, error) {
	if evt == nil || evt.StateKey != nil || evt.Type.Type != event.EventMessage.Type || b.isOwnUser(evt.Sender) {
		return nil, nil
	}

	// mautrix parses Content only while dispatching the transaction. Durable classification runs
	// before that point, so apply the same message-event class and parse the raw content here.
	evt.Type.Class = event.MessageEventType
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		// Matrix transactions multiplex unrelated traffic. A malformed message that cannot be
		// classified as an eligible delegation must not poison the whole transaction and force the
		// homeserver into an infinite retry loop; mautrix will log and ignore it on normal dispatch.
		return nil, nil
	}
	msg := evt.Content.AsMessage()
	if msg == nil {
		return nil, nil
	}
	switch {
	case msg.MsgType == event.MsgText:
		if isAgentDirectoryCommand(msg.Body) {
			return nil, nil
		}
	case msg.MsgType.IsMedia():
		// Media is eligible only when it addresses a target; resolveTargets below decides that.
	default:
		return nil, nil
	}

	targets := b.resolveTargets(evt, msg)
	localparts := append(append([]string(nil), targets.allowed...), targets.deniedBridged...)
	if len(localparts) == 0 {
		return nil, nil
	}
	if evt.ID == "" || evt.RoomID == "" || evt.Sender == "" {
		return nil, fmt.Errorf("eligible event is missing event ID, room ID, or sender")
	}

	recoveryEvent := minimalRecoveryEvent(evt, msg)
	eventPayload, err := json.Marshal(&recoveryEvent)
	if err != nil {
		return nil, fmt.Errorf("encode recoverable event %q: %w", evt.ID, err)
	}
	payload, err := json.Marshal(durablePayload{Version: durablePayloadVersion, Event: eventPayload})
	if err != nil {
		return nil, fmt.Errorf("encode durable payload for event %q: %w", evt.ID, err)
	}
	prompt := b.stripMentions(msg.Body)
	jobs := make([]state.NewDelegation, 0, len(localparts))
	for _, localpart := range localparts {
		ref := targets.refs[localpart]
		if ref == nil {
			return nil, fmt.Errorf("eligible event %q target %q has no immutable mapping", evt.ID, localpart)
		}
		jobs = append(jobs, state.NewDelegation{
			MatrixEventID:       evt.ID.String(),
			GhostMXID:           id.NewUserID(localpart, b.cfg.ServerName).String(),
			GhostLocalpart:      localpart,
			RoomID:              evt.RoomID.String(),
			SenderMXID:          evt.Sender.String(),
			SenderOriginKind:    string(targets.sender.origin.kind),
			SenderOriginNetwork: targets.sender.origin.network,
			OriginServerTS:      evt.Timestamp,
			TargetFingerprint:   ref.MappingID(),
			Prompt:              prompt,
			Payload:             payload,
		})
	}
	return jobs, nil
}

// minimalRecoveryEvent retains only the immutable Matrix identity and content required to project
// a deterministic reply or re-resolve one referenced media event. Prompt text is already stored in
// the dedicated bounded field, so arbitrary unsigned keys, formatted bodies, mentions, and client
// extensions are deliberately excluded from every per-target recovery envelope.
func minimalRecoveryEvent(evt *event.Event, msg *event.MessageEventContent) event.Event {
	content := &event.MessageEventContent{MsgType: msg.MsgType}
	if replyTo := msg.RelatesTo.GetReplyTo(); replyTo != "" {
		content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: replyTo}}
	}
	if msg.MsgType.IsMedia() {
		content.Body = msg.Body
		content.FileName = msg.FileName
		content.URL = msg.URL
		if msg.Info != nil {
			content.Info = &event.FileInfo{MimeType: msg.Info.MimeType, Size: msg.Info.Size}
		}
		if msg.File != nil {
			// Only the presence of encrypted-file metadata is needed: policy rejects it before use.
			content.File = &event.EncryptedFileInfo{}
		}
	}
	return event.Event{
		Sender: evt.Sender, Type: event.EventMessage, Timestamp: evt.Timestamp,
		ID: evt.ID, RoomID: evt.RoomID, Content: event.Content{Parsed: content},
	}
}
