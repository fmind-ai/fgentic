package activitystate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryDeduplicatesAndSurvivesGatewayRestart(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0).UTC()
	store := newMemory(time.Hour, 8, func() time.Time { return clock })
	job := Job{
		ActivityID: "https://partner.example/activities/1",
		Route:      RouteAgent,
		Target:     "agent-docs-qa",
		ActorURI:   "https://partner.example/users/alice",
		Body:       []byte(`{"type":"Create"}`),
	}
	record, inserted, err := store.Enqueue(context.Background(), job)
	if err != nil || !inserted || record.State != StatePending {
		t.Fatalf("first enqueue = (%+v, %t, %v)", record, inserted, err)
	}
	claimed, ok, err := store.Claim(context.Background())
	if err != nil || !ok || claimed.ActivityID != job.ActivityID {
		t.Fatalf("claim = (%+v, %t, %v)", claimed, ok, err)
	}
	reply := []byte(`{"type":"Create","content":"reply"}`)
	if err := store.Complete(context.Background(), job.ActivityID, Completion{
		State: StateSucceeded, Location: "https://local.example/replies/1", Result: reply,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// A new gateway process reuses the same durable store and returns the prior content-free outcome
	// without another claim.
	record, inserted, err = store.Enqueue(context.Background(), job)
	if err != nil || inserted {
		t.Fatalf("retry enqueue = (%+v, %t, %v)", record, inserted, err)
	}
	if record.State != StateSucceeded || record.Location != "https://local.example/replies/1" || string(record.Result) != string(reply) || len(record.Body) != 0 {
		t.Fatalf("cached record = %+v", record)
	}
	status, err := store.LookupStatus(context.Background(), record.StatusToken)
	if err != nil || string(status.Result) != string(reply) {
		t.Fatalf("LookupStatus = (%+v, %v)", status, err)
	}
	resultRecord, err := store.LookupResult(context.Background(), record.Location)
	if err != nil || string(resultRecord.Result) != string(reply) {
		t.Fatalf("LookupResult = (%+v, %v)", resultRecord, err)
	}
	if _, ok, err := store.Claim(context.Background()); err != nil || ok {
		t.Fatalf("duplicate claim = (%t, %v), want no work", ok, err)
	}
}

func TestMemoryRejectsActivityIDCollision(t *testing.T) {
	store := NewMemory(time.Hour, 8)
	job := Job{ActivityID: "activity-1", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("original")}
	if _, inserted, err := store.Enqueue(context.Background(), job); err != nil || !inserted {
		t.Fatalf("first Enqueue = (%t, %v)", inserted, err)
	}
	job.Body = []byte("changed")
	if _, _, err := store.Enqueue(context.Background(), job); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Enqueue error = %v, want ErrConflict", err)
	}
}

func TestMemoryNeverReplaysInterruptedWork(t *testing.T) {
	store := NewMemory(time.Hour, 8)
	job := Job{ActivityID: "activity-1", Route: RouteGroup, Target: "collab", ActorURI: "actor", Body: []byte("body")}
	if _, inserted, err := store.Enqueue(context.Background(), job); err != nil || !inserted {
		t.Fatalf("Enqueue = (%t, %v)", inserted, err)
	}
	if _, claimed, err := store.Claim(context.Background()); err != nil || !claimed {
		t.Fatalf("Claim = (%t, %v)", claimed, err)
	}
	if err := store.FailRunning(context.Background()); err != nil {
		t.Fatalf("FailRunning: %v", err)
	}
	if _, claimed, err := store.Claim(context.Background()); err != nil || claimed {
		t.Fatalf("post-restart Claim = (%t, %v), want no replay", claimed, err)
	}
	record, inserted, err := store.Enqueue(context.Background(), job)
	if err != nil || inserted || record.State != StateFailed {
		t.Fatalf("cached interrupted record = (%+v, %t, %v)", record, inserted, err)
	}
}

func TestMemoryPrunesOnlyExpiredTerminalOutcomes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newMemory(time.Hour, 8, func() time.Time { return now })
	terminalJob := Job{ActivityID: "old", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("body")}
	pendingJob := Job{ActivityID: "pending", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("body")}
	for _, job := range []Job{terminalJob, pendingJob} {
		if _, _, err := store.Enqueue(context.Background(), job); err != nil {
			t.Fatalf("Enqueue(%s): %v", job.ActivityID, err)
		}
	}
	if _, _, err := store.Claim(context.Background()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Complete(context.Background(), terminalJob.ActivityID, Completion{State: StateIgnored}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	now = now.Add(2 * time.Hour)
	probe := Job{ActivityID: "probe", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("body")}
	if _, _, err := store.Enqueue(context.Background(), probe); err != nil {
		t.Fatalf("probe Enqueue: %v", err)
	}
	if _, inserted, err := store.Enqueue(context.Background(), terminalJob); err != nil || !inserted {
		t.Fatalf("expired terminal re-enqueue = (%t, %v), want inserted", inserted, err)
	}
	if record, inserted, err := store.Enqueue(context.Background(), pendingJob); err != nil || inserted || record.State != StatePending {
		t.Fatalf("pending re-enqueue = (%+v, %t, %v)", record, inserted, err)
	}
}

func TestMemoryQueueCapacityIsAtomicAndTerminalWorkFreesIt(t *testing.T) {
	store := NewMemory(time.Hour, 1)
	first := Job{ActivityID: "first", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("first")}
	second := Job{ActivityID: "second", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("second")}
	if _, inserted, err := store.Enqueue(context.Background(), first); err != nil || !inserted {
		t.Fatalf("first Enqueue = (%t, %v)", inserted, err)
	}
	if _, _, err := store.Enqueue(context.Background(), second); !errors.Is(err, ErrCapacity) {
		t.Fatalf("full Enqueue error = %v, want ErrCapacity", err)
	}
	if _, inserted, err := store.Enqueue(context.Background(), first); err != nil || inserted {
		t.Fatalf("duplicate while full = (%t, %v), want cached", inserted, err)
	}
	if _, claimed, err := store.Claim(context.Background()); err != nil || !claimed {
		t.Fatalf("Claim = (%t, %v)", claimed, err)
	}
	if err := store.Complete(context.Background(), first.ActivityID, Completion{State: StateFailed}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, inserted, err := store.Enqueue(context.Background(), second); err != nil || !inserted {
		t.Fatalf("enqueue after terminal = (%t, %v)", inserted, err)
	}
}

func TestMemoryIgnoreIsNeverClaimableAndUsesBodyHashForDedup(t *testing.T) {
	store := NewMemory(time.Hour, 1)
	job := Job{ActivityID: "ignored", Route: RouteAgent, Target: "agent", ActorURI: "actor", Body: []byte("body")}
	record, inserted, err := store.Ignore(context.Background(), job)
	if err != nil || !inserted || record.State != StateIgnored || len(record.Body) != 0 || len(record.BodyHash) != 32 {
		t.Fatalf("Ignore = (%+v, %t, %v)", record, inserted, err)
	}
	if _, claimed, err := store.Claim(context.Background()); err != nil || claimed {
		t.Fatalf("Claim ignored = (%t, %v)", claimed, err)
	}
	if _, inserted, err := store.Enqueue(context.Background(), job); err != nil || inserted {
		t.Fatalf("duplicate ignored Enqueue = (%t, %v)", inserted, err)
	}
	job.Body = []byte("changed")
	if _, _, err := store.Enqueue(context.Background(), job); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed ignored body error = %v, want ErrConflict", err)
	}
}
