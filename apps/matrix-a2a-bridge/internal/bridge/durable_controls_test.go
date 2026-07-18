package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

type lostControlAcknowledgementStore struct {
	state.Store
	lost bool
}

type failQuestionPlanStore struct {
	state.Store
	failed bool
}

func (s *failQuestionPlanStore) PlanControl(
	ctx context.Context,
	request state.PlanControlRequest,
) (state.Control, error) {
	if !s.failed && request.Kind == state.ControlQuestion {
		s.failed = true
		return state.Control{}, errors.New("injected crash before question outbox plan")
	}
	return s.Store.PlanControl(ctx, request)
}

func (s *lostControlAcknowledgementStore) TransitionControl(
	ctx context.Context,
	request state.ControlTransitionRequest,
) error {
	if !s.lost && request.To == state.ControlApplied {
		s.lost = true
		return errors.New("injected crash after control side effect")
	}
	return s.Store.TransitionControl(ctx, request)
}

func TestDurableInputContinuationSurvivesWorkerRestart(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{
			TaskID: "task-input", ContextID: "context-input", InputRequired: true, Text: "which namespace?",
		},
		continueResult: a2aclient.Result{
			TaskID: "task-input", ContextID: "context-input", Terminal: true, Text: "namespace is healthy",
		},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-input-restart")

	b.executeDurableJob(t.Context(), job)
	paused := loadDurableJob(t, b, job.JobID)
	if paused.State != state.StateAwaitingInput || paused.MatrixPlaceholderEventID == "" ||
		paused.InputWaitStartedAt.IsZero() || paused.InputWaitExpiresAt.IsZero() || paused.TaskDeadlineAt.IsZero() {
		t.Fatalf("paused durable task = %+v", paused)
	}
	answer := threadedTransactionEvent(
		"$durable-answer", "@alice:"+ownServer, paused.MatrixPlaceholderEventID, "kube-system",
	)
	result, err := b.AdmitAppserviceTransaction(t.Context(), "txn-durable-answer", transactionBody(t, answer))
	if err != nil || len(result.InsertedControlIDs) != 1 {
		t.Fatalf("admit continuation = (%+v, %v)", result, err)
	}

	restarted := claimDurableJob(t, b, time.Now().UTC())
	b.executeDurableJob(t.Context(), restarted)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || client.continueCount != 1 ||
		client.continueText != expectedDurableContinuationPrompt("kube-system") ||
		client.continueTaskID != "task-input" ||
		client.continueContextID != "context-input" {
		t.Fatalf("continued durable task = state %s calls %d text %q task %q context %q",
			stored.State, client.continueCount, client.continueText, client.continueTaskID, client.continueContextID)
	}
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || len(controls) != 2 || controls[0].Kind != state.ControlQuestion ||
		controls[1].Kind != state.ControlContinuation || controls[1].State != state.ControlApplied ||
		len(controls[1].Payload) != 0 {
		t.Fatalf("durable input controls = (%+v, %v)", controls, err)
	}
	events := recorder.snapshot()
	if len(events) != 3 || !strings.Contains(events[1].Body, "which namespace?") ||
		!strings.Contains(events[2].Body, "namespace is healthy") {
		t.Fatalf("durable input Matrix projection = %+v", events)
	}
}

func TestDurableInputRepairsQuestionPlanAfterStateCommitCrash(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-input", ContextID: "context-input", InputRequired: true, Text: "which namespace?",
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.store = &failQuestionPlanStore{Store: b.store}
	job := admitAndClaimDurableJob(t, b, "$durable-input-question-plan-crash")

	b.executeDurableJob(t.Context(), job)
	paused := loadDurableJob(t, b, job.JobID)
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || paused.State != state.StateAwaitingInput || paused.ResultText == "" || len(controls) != 0 {
		t.Fatalf("input state before question-plan recovery = job %+v controls %+v err %v", paused, controls, err)
	}

	restarted := claimDurableJob(t, b, paused.NextAttemptAt.Add(time.Second))
	b.executeDurableJob(t.Context(), restarted)
	stored := loadDurableJob(t, b, job.JobID)
	controls, err = b.store.Controls(t.Context(), job.JobID)
	if err != nil || stored.State != state.StateAwaitingInput || stored.ResultText != "" ||
		len(controls) != 1 || controls[0].Kind != state.ControlQuestion || controls[0].State != state.ControlApplied ||
		len(controls[0].Payload) != 0 {
		t.Fatalf("repaired durable question = job %+v controls %+v err %v", stored, controls, err)
	}
	events := recorder.snapshot()
	if len(events) != 2 || !strings.Contains(events[1].Body, "which namespace?") {
		t.Fatalf("repaired durable question projection = %+v", events)
	}
}

func TestDurableInputExpiresWithinSeparateWaitingBudget(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-input", ContextID: "context-input", InputRequired: true, Text: "which namespace?",
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.InputWaitTimeout = 10 * time.Millisecond
	job := admitAndClaimDurableJob(t, b, "$durable-input-expiry")

	b.executeDurableJob(t.Context(), job)
	paused := loadDurableJob(t, b, job.JobID)
	if delay := time.Until(paused.InputWaitExpiresAt); delay > 0 {
		time.Sleep(delay + time.Millisecond)
	}
	restarted := claimDurableJob(t, b, time.Now().UTC())
	b.executeDurableJob(t.Context(), restarted)

	stored := loadDurableJob(t, b, job.JobID)
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || stored.State != state.StateDelivered || stored.ErrorCode != errorInputWaitTimeout ||
		client.continueCount != 0 || len(controls) != 1 || len(controls[0].Payload) != 0 {
		t.Fatalf("expired durable input = job %+v calls %d controls %+v err %v",
			stored, client.continueCount, controls, err)
	}
	events := recorder.snapshot()
	if len(events) != 3 || !strings.Contains(events[2].Body, "got no reply within 10ms") {
		t.Fatalf("expired durable input projection = %+v", events)
	}
}

func TestDurableInputRejectsDifferentSenderWithoutConsumingAnswer(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-input", ContextID: "context-input", InputRequired: true, Text: "which namespace?",
	}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-input-wrong-sender")
	b.executeDurableJob(t.Context(), job)
	paused := loadDurableJob(t, b, job.JobID)

	answer := threadedTransactionEvent(
		"$durable-wrong-answer", "@mallory:"+ownServer, paused.MatrixPlaceholderEventID, "private answer",
	)
	result, err := b.AdmitAppserviceTransaction(t.Context(), "txn-durable-wrong-answer", transactionBody(t, answer))
	if err != nil || len(result.InsertedControlIDs) != 1 {
		t.Fatalf("admit wrong-sender continuation = (%+v, %v)", result, err)
	}
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || len(controls) != 2 || controls[1].State != state.ControlDenied ||
		controls[1].ErrorCode != "control_sender_rejected" || len(controls[1].Payload) != 0 {
		t.Fatalf("wrong-sender control = (%+v, %v)", controls, err)
	}
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateAwaitingInput || client.continueCount != 0 {
		t.Fatalf("wrong sender changed task = state %s continuation calls %d", stored.State, client.continueCount)
	}
}

func TestDurableInputAppliesOnlyOneAuthorizedAnswerPerQuestion(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{
			TaskID: "task-input", ContextID: "context-input", InputRequired: true, Text: "which namespace?",
		},
		continueResult: a2aclient.Result{
			TaskID: "task-input", ContextID: "context-input", Text: "checking the namespace",
		},
	}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.pollInitial = 0
	job := admitAndClaimDurableJob(t, b, "$durable-input-race")
	b.executeDurableJob(t.Context(), job)
	paused := loadDurableJob(t, b, job.JobID)

	for index, answer := range []string{"kube-system", "default"} {
		eventID := fmt.Sprintf("$durable-racing-answer-%d", index)
		evt := threadedTransactionEvent(
			eventID, "@alice:"+ownServer, paused.MatrixPlaceholderEventID, answer,
		)
		result, err := b.AdmitAppserviceTransaction(
			t.Context(), fmt.Sprintf("txn-durable-racing-answer-%d", index), transactionBody(t, evt),
		)
		wantInserted, wantUnmatched := 1, 0
		if index > 0 {
			wantInserted, wantUnmatched = 0, 1
		}
		if err != nil || len(result.InsertedControlIDs) != wantInserted ||
			len(result.UnmatchedControlIDs) != wantUnmatched {
			t.Fatalf("admit racing continuation %d = (%+v, %v)", index, result, err)
		}
	}

	first := claimDurableJob(t, b, time.Now().UTC())
	b.executeDurableJob(t.Context(), first)

	stored := loadDurableJob(t, b, job.JobID)
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil {
		t.Fatalf("load controls: %v", err)
	}
	continuations := make([]state.Control, 0, 2)
	for _, control := range controls {
		if control.Kind == state.ControlContinuation {
			continuations = append(continuations, control)
		}
	}
	if stored.State != state.StateAwaitingTask || client.continueCount != 1 ||
		client.continueText != expectedDurableContinuationPrompt("kube-system") || len(continuations) != 1 ||
		continuations[0].State != state.ControlApplied || len(continuations[0].Payload) != 0 {
		t.Fatalf("racing continuations = job %s calls %d text %q controls %+v",
			stored.State, client.continueCount, client.continueText, continuations)
	}
}

func TestRecoveredPreparedCancelIsNeverResent(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-cancel", ContextID: "context-cancel", Text: "working",
	}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-cancel-crash")
	b.executeDurableJob(t.Context(), job)
	running := loadDurableJob(t, b, job.JobID)

	cancel := cancelTransactionEvent(
		"$durable-cancel", "@alice:"+ownServer, running.MatrixPlaceholderEventID,
	)
	result, err := b.AdmitAppserviceTransaction(t.Context(), "txn-durable-cancel", transactionBody(t, cancel))
	if err != nil || len(result.InsertedControlIDs) != 1 {
		t.Fatalf("admit cancel = (%+v, %v)", result, err)
	}
	firstOwner := claimDurableJob(t, b, time.Now().UTC())
	prepared, found, err := b.store.ClaimControl(t.Context(), firstOwner.LeaseToken(), time.Now().UTC())
	if err != nil || !found || prepared.Kind != state.ControlCancel {
		t.Fatalf("prepare cancel before crash = (%+v, %t, %v)", prepared, found, err)
	}

	takeoverAt := firstOwner.LeaseExpiresAt.Add(time.Millisecond)
	restarted := claimDurableJob(t, b, takeoverAt)
	b.executeDurableJob(t.Context(), restarted)
	stored := loadDurableJob(t, b, job.JobID)
	if client.cancelTasks != nil || stored.State != state.StateAmbiguous {
		t.Fatalf("recovered cancel = calls %v state %s, want no resend/ambiguous", client.cancelTasks, stored.State)
	}
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || len(controls) != 1 || controls[0].RecoveryCount != 1 ||
		controls[0].State != state.ControlAmbiguous {
		t.Fatalf("recovered cancel control = (%+v, %v)", controls, err)
	}
}

func TestCancelSideEffectBeforeLedgerAcknowledgementRunsAtMostOnce(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-cancel", ContextID: "context-cancel", Text: "working",
	}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-cancel-lost-db-ack")
	b.executeDurableJob(t.Context(), job)
	running := loadDurableJob(t, b, job.JobID)
	cancel := cancelTransactionEvent(
		"$durable-cancel-lost-db-ack", "@alice:"+ownServer, running.MatrixPlaceholderEventID,
	)
	admitted, err := b.AdmitAppserviceTransaction(
		t.Context(), "txn-durable-cancel-lost-db-ack", transactionBody(t, cancel),
	)
	if err != nil || len(admitted.InsertedControlIDs) != 1 {
		t.Fatalf("admit cancel = (%+v, %v)", admitted, err)
	}
	b.store = &lostControlAcknowledgementStore{Store: b.store}
	ready := loadDurableJob(t, b, job.JobID)
	claimed := claimDurableJob(t, b, ready.NextAttemptAt.Add(time.Hour))
	b.executeDurableJob(t.Context(), claimed)
	if len(client.cancelTasks) != 1 {
		t.Fatalf("cancel calls after injected crash = %v, want one", client.cancelTasks)
	}
	retry := loadDurableJob(t, b, job.JobID)
	restarted := claimDurableJob(t, b, retry.NextAttemptAt.Add(2*time.Hour))
	b.executeDurableJob(t.Context(), restarted)
	stored := loadDurableJob(t, b, job.JobID)
	if len(client.cancelTasks) != 1 || stored.State != state.StateAmbiguous {
		t.Fatalf("recovered lost cancel acknowledgement = calls %v state %s", client.cancelTasks, stored.State)
	}
}

func TestDurableProgressIsBoundedAcrossPollLeases(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-progress", ContextID: "context-progress", Text: "phase one"},
		polls: []scriptedPoll{
			{result: a2aclient.Result{TaskID: "task-progress", ContextID: "context-progress", Text: "phase two"}},
			{result: a2aclient.Result{TaskID: "task-progress", ContextID: "context-progress", Text: "phase three"}},
			{result: a2aclient.Result{
				TaskID: "task-progress", ContextID: "context-progress", Terminal: true, Text: "complete",
			}},
		},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.MaxTaskProgressPosts = 2
	job := admitAndClaimDurableJob(t, b, "$durable-progress")
	b.executeDurableJob(t.Context(), job)
	for attempt := 0; attempt < 3; attempt++ {
		claimed := claimDurableJob(t, b, time.Now().Add(time.Duration(attempt+1)*time.Second))
		b.executeDurableJob(t.Context(), claimed)
	}
	stored := loadDurableJob(t, b, job.JobID)
	controls, err := b.store.Controls(t.Context(), job.JobID)
	progress := 0
	for _, control := range controls {
		if control.Kind == state.ControlProgress {
			progress++
		}
	}
	if err != nil || stored.State != state.StateDelivered || progress != 2 {
		t.Fatalf("durable progress = state %s controls %+v err %v", stored.State, controls, err)
	}
	events := recorder.snapshot()
	threaded := 0
	for _, item := range events {
		if item.RelatesTo != nil && item.RelatesTo.Type == "m.thread" {
			threaded++
		}
	}
	if threaded != 2 {
		t.Fatalf("threaded durable progress = %d, want 2; events=%+v", threaded, events)
	}
}

func TestDurablePinConvergesBeforeTerminalDelivery(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-pin", ContextID: "context-pin"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			TaskID: "task-pin", ContextID: "context-pin", Terminal: true, Text: "complete",
		}}},
	}
	b, _, _, _, pins := pinningHarness(t, client)
	configureDurableTestBridge(b)
	b.pollInitial = 0
	b.cfg.MaxTaskProgressPosts = 0
	job := admitAndClaimDurableJob(t, b, "$durable-pin")

	b.executeDurableJob(t.Context(), job)
	running := loadDurableJob(t, b, job.JobID)
	pinned, writes := pins.snapshot()
	if running.State != state.StateAwaitingTask || writes != 1 ||
		len(pinned) != 1 || pinned[0] != running.MatrixPlaceholderEventID {
		t.Fatalf("durable pin = job %s placeholder %q pinned %v writes %d",
			running.State, running.MatrixPlaceholderEventID, pinned, writes)
	}

	polling := claimDurableJob(t, b, time.Now().UTC())
	b.executeDurableJob(t.Context(), polling)

	stored := loadDurableJob(t, b, job.JobID)
	pinned, writes = pins.snapshot()
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || stored.State != state.StateDelivered || writes != 2 || len(pinned) != 0 {
		t.Fatalf("durable unpin = job %s pinned %v writes %d controls %+v err %v",
			stored.State, pinned, writes, controls, err)
	}
	if len(controls) != 2 || controls[0].Kind != state.ControlPin ||
		controls[0].State != state.ControlApplied || controls[1].Kind != state.ControlUnpin ||
		controls[1].State != state.ControlApplied {
		t.Fatalf("durable pin controls = %+v", controls)
	}
}

func TestDurableControlCapacityReservesTerminalUnpin(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-pin-capacity", ContextID: "context-pin-capacity",
	}}
	b, _, _, _, pins := pinningHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ControlCapacityPerJob = 5
	b.cfg.PinInFlightTasks = true
	b.cfg.MaxTaskProgressPosts = 0
	job := admitAndClaimDurableJob(t, b, "$durable-pin-capacity")
	b.executeDurableJob(t.Context(), job)
	running := loadDurableJob(t, b, job.JobID)
	claimed := claimDurableJob(t, b, running.NextAttemptAt.Add(time.Millisecond))

	for slot := 1; slot <= 3; slot++ {
		if _, err := b.store.PlanControl(t.Context(), state.PlanControlRequest{
			Lease: claimed.LeaseToken(), At: time.Now().UTC(), Kind: state.ControlPin,
			Slot: slot, Capacity: b.durableControlCapacity(),
		}); err != nil {
			t.Fatalf("fill ordinary control slot %d: %v", slot, err)
		}
	}
	if _, err := b.store.PlanControl(t.Context(), state.PlanControlRequest{
		Lease: claimed.LeaseToken(), At: time.Now().UTC(), Kind: state.ControlPin,
		Slot: 4, Capacity: b.durableControlCapacity(),
	}); !errors.Is(err, state.ErrControlCapacity) {
		t.Fatalf("ordinary control consumed terminal reserve: %v", err)
	}
	if err := b.ensureDurablePin(t.Context(), &claimed, false); err != nil {
		t.Fatalf("terminal unpin at capacity: %v", err)
	}

	pinned, _ := pins.snapshot()
	controls, err := b.store.Controls(t.Context(), job.JobID)
	if err != nil || len(pinned) != 0 || len(controls) != 5 ||
		controls[len(controls)-1].Kind != state.ControlUnpin ||
		controls[len(controls)-1].State != state.ControlApplied {
		t.Fatalf("terminal unpin reserve = pinned %v controls %+v err %v", pinned, controls, err)
	}
}

func expectedDurableContinuationPrompt(content string) string {
	return fmt.Sprintf(`--- BEGIN FGENTIC BRIDGE PROVENANCE ---
sender_mxid: "@alice:fgentic.fmind.ai"
sender_homeserver: "fgentic.fmind.ai"
room_id: "!room:fgentic.fmind.ai"
--- END FGENTIC BRIDGE PROVENANCE ---
--- BEGIN UNTRUSTED MATRIX CONTENT ---
%s
--- END UNTRUSTED MATRIX CONTENT ---`, content)
}

func claimDurableJob(t *testing.T, b *Bridge, now time.Time) state.Job {
	t.Helper()
	job, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "restarted-worker", Now: now, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim durable job at %s = (%+v, %t, %v)", now, job, found, err)
	}
	return job
}

func threadedTransactionEvent(eventID, sender, target, body string) map[string]any {
	evt := transactionEvent(eventID, sender, body)
	evt["content"] = map[string]any{
		"msgtype": "m.text", "body": body,
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": target},
	}
	return evt
}

func cancelTransactionEvent(eventID, sender, target string) map[string]any {
	return map[string]any{
		"event_id": eventID, "room_id": "!room:" + ownServer,
		"sender": sender, "type": "m.reaction", "origin_server_ts": int64(1),
		"content": map[string]any{
			"m.relates_to": map[string]any{
				"rel_type": "m.annotation", "event_id": target, "key": cancelReactionKey,
			},
		},
	}
}
