package state

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestDelegationStateTransitionMatrix(t *testing.T) {
	legal := map[[2]DelegationState]bool{
		{StatePending, StateA2APrepared}:       true,
		{StatePending, StateReplyPending}:      true,
		{StatePending, StateDenied}:            true,
		{StatePending, StateDead}:              true,
		{StateA2APrepared, StateAwaitingTask}:  true,
		{StateA2APrepared, StateReplyPending}:  true,
		{StateA2APrepared, StateDenied}:        true,
		{StateA2APrepared, StateAmbiguous}:     true,
		{StateA2APrepared, StateDead}:          true,
		{StateAwaitingTask, StateReplyPending}: true,
		{StateAwaitingTask, StateDenied}:       true,
		{StateAwaitingTask, StateDead}:         true,
		{StateReplyPending, StateDelivered}:    true,
		{StateReplyPending, StateDenied}:       true,
		{StateReplyPending, StateAmbiguous}:    true,
		{StateReplyPending, StateDead}:         true,
	}
	for _, from := range delegationStates {
		if !from.Valid() {
			t.Fatalf("persisted state %q is not valid", from)
		}
		for _, to := range delegationStates {
			want := legal[[2]DelegationState{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %t, want %t", from, to, got, want)
			}
			request := TransitionRequest{
				Lease: LeaseToken{JobID: "job", Owner: "worker", Generation: 1},
				From:  from,
				To:    to,
				At:    time.Unix(1, 0),
			}
			err := validateTransition(request)
			if want && err != nil {
				t.Errorf("validateTransition(%s, %s): %v", from, to, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("validateTransition(%s, %s) error = %v, want ErrInvalidTransition", from, to, err)
			}
		}
	}
	if DelegationState("unknown").Valid() {
		t.Fatal("unknown state reported valid")
	}
}

func TestDelegationStateTerminalStates(t *testing.T) {
	for _, state := range delegationStates {
		want := state == StateDelivered || state == StateDenied || state == StateAmbiguous || state == StateDead
		if got := state.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %t, want %t", state, got, want)
		}
	}
}

func TestDeterministicProtocolIDs(t *testing.T) {
	jobID := JobIDFor("$event", "@agent:example.test")
	if jobID == "" || jobID != JobIDFor("$event", "@agent:example.test") {
		t.Fatalf("job ID is not stable: %q", jobID)
	}
	if jobID == JobIDFor("$other", "@agent:example.test") ||
		jobID == JobIDFor("$event", "@other:example.test") {
		t.Fatal("job ID does not bind both event and ghost")
	}
	messageID := A2AMessageIDFor(jobID)
	if messageID == "" || messageID != A2AMessageIDFor(jobID) {
		t.Fatalf("A2A message ID is not stable: %q", messageID)
	}
	seen := map[string]bool{}
	for _, stage := range []string{"reply", "placeholder", "edit", "recovery"} {
		transactionID := MatrixTransactionIDFor(jobID, stage)
		if transactionID == "" || seen[transactionID] {
			t.Fatalf("Matrix transaction ID for %q is empty or reused: %q", stage, transactionID)
		}
		seen[transactionID] = true
		if transactionID != MatrixTransactionIDFor(jobID, stage) {
			t.Fatalf("Matrix transaction ID for %q is not stable", stage)
		}
	}
}

func TestMatrixEventStages(t *testing.T) {
	for _, stage := range []MatrixEventStage{MatrixEventReply, MatrixEventPlaceholder, MatrixEventEdit} {
		if !stage.Valid() {
			t.Errorf("stage %q is not valid", stage)
		}
	}
	if MatrixEventStage("recovery").Valid() {
		t.Fatal("unknown Matrix event stage reported valid")
	}
}

func TestLeaseGenerationFitsPostgresBigint(t *testing.T) {
	base := LeaseToken{JobID: "job", Owner: "worker", Generation: math.MaxInt64}
	if err := validateLease(base); err != nil {
		t.Fatalf("validate max bigint lease: %v", err)
	}
	base.Generation++
	if err := validateLease(base); err == nil {
		t.Fatal("lease generation larger than bigint was accepted")
	}
}

func TestTerminalTransitionRejectsContentPatch(t *testing.T) {
	result := "must already be durable in reply_pending"
	err := validateTransition(TransitionRequest{
		Lease: LeaseToken{JobID: "job", Owner: "worker", Generation: 1},
		From:  StateReplyPending,
		To:    StateDelivered,
		At:    time.Unix(1, 0),
		Patch: TransitionPatch{ResultText: &result},
	})
	if err == nil {
		t.Fatal("terminal transition accepted a content-bearing patch")
	}
}

func TestRetryKindMustBeExplicit(t *testing.T) {
	request := RetryRequest{
		Lease:         LeaseToken{JobID: "job", Owner: "worker", Generation: 1},
		At:            time.Unix(1, 0),
		NextAttemptAt: time.Unix(2, 0),
		ErrorCode:     "transient_failure",
	}
	if err := validateRetry(request); err == nil {
		t.Fatal("retry with zero kind was accepted")
	}
	for _, kind := range []RetryKind{RetryFailure, RetryPoll} {
		request.Kind = kind
		if err := validateRetry(request); err != nil {
			t.Errorf("validate retry kind %q: %v", kind, err)
		}
	}
}

func TestTransitionMatrixEventPatchMustNotBeEmpty(t *testing.T) {
	empty := ""
	err := validateTransition(TransitionRequest{
		Lease: LeaseToken{JobID: "job", Owner: "worker", Generation: 1},
		From:  StateReplyPending,
		To:    StateDelivered,
		At:    time.Unix(1, 0),
		Patch: TransitionPatch{MatrixReplyEventID: &empty},
	})
	if err == nil {
		t.Fatal("transition accepted an empty Matrix event ID patch")
	}
}
