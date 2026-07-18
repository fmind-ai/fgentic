//go:build integration

package activitystate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresDedupSurvivesStoreRestart(t *testing.T) {
	databaseURL := os.Getenv("ACTIVITY_STATE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("ACTIVITY_STATE_TEST_DATABASE_URL is required for the integration suite")
	}
	ctx := context.Background()
	first, err := OpenPostgres(ctx, databaseURL, time.Hour, 2)
	if err != nil {
		t.Fatalf("OpenPostgres first process: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, "TRUNCATE activitypub_inbox_activities"); err != nil {
		t.Fatalf("truncate activity ledger: %v", err)
	}
	job := Job{
		ActivityID: "https://partner.example/activities/restart-1",
		Route:      RouteAgent,
		Target:     "agent-docs-qa",
		ActorURI:   "https://partner.example/users/alice",
		Body:       []byte(`{"type":"Create"}`),
	}
	if _, inserted, err := first.Enqueue(ctx, job); err != nil || !inserted {
		t.Fatalf("first Enqueue = (%t, %v)", inserted, err)
	}
	if claimed, ok, err := first.Claim(ctx); err != nil || !ok || claimed.ActivityID != job.ActivityID {
		t.Fatalf("first Claim = (%+v, %t, %v)", claimed, ok, err)
	}
	location := "https://local.example/activities/reply-1"
	reply := []byte(`{"type":"Create","content":"durable reply"}`)
	if err := first.Complete(ctx, job.ActivityID, Completion{State: StateSucceeded, Location: location, Result: reply}); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	interrupted := job
	interrupted.ActivityID = "https://partner.example/activities/running-2"
	if _, inserted, err := first.Enqueue(ctx, interrupted); err != nil || !inserted {
		t.Fatalf("interrupted Enqueue = (%t, %v)", inserted, err)
	}
	if claimedJob, claimed, err := first.Claim(ctx); err != nil || !claimed || claimedJob.ActivityID != interrupted.ActivityID {
		t.Fatalf("interrupted Claim = (%+v, %t, %v)", claimedJob, claimed, err)
	}
	pending := job
	pending.ActivityID = "https://partner.example/activities/pending-3"
	if _, inserted, err := first.Enqueue(ctx, pending); err != nil || !inserted {
		t.Fatalf("pending Enqueue = (%t, %v)", inserted, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first process: %v", err)
	}

	second, err := OpenPostgres(ctx, databaseURL, time.Hour, 2)
	if err != nil {
		t.Fatalf("OpenPostgres second process: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.FailRunning(ctx); err != nil {
		t.Fatalf("FailRunning after restart: %v", err)
	}
	record, inserted, err := second.Enqueue(ctx, job)
	if err != nil || inserted {
		t.Fatalf("restart Enqueue = (%+v, %t, %v)", record, inserted, err)
	}
	if record.State != StateSucceeded || record.Location != location || string(record.Result) != string(reply) || len(record.Body) != 0 {
		t.Fatalf("restart cached outcome = %+v", record)
	}
	status, err := second.LookupStatus(ctx, record.StatusToken)
	if err != nil || string(status.Result) != string(reply) {
		t.Fatalf("restart LookupStatus = (%+v, %v)", status, err)
	}
	resultRecord, err := second.LookupResult(ctx, location)
	if err != nil || string(resultRecord.Result) != string(reply) {
		t.Fatalf("restart LookupResult = (%+v, %v)", resultRecord, err)
	}
	conflict := job
	conflict.Body = []byte(`{"type":"Create","object":"changed"}`)
	if _, _, err := second.Enqueue(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("restart conflicting Enqueue error = %v, want ErrConflict", err)
	}
	interruptedRecord, inserted, err := second.Enqueue(ctx, interrupted)
	if err != nil || inserted || interruptedRecord.State != StateFailed {
		t.Fatalf("interrupted cached outcome = (%+v, %t, %v)", interruptedRecord, inserted, err)
	}
	claimedJob, claimed, err := second.Claim(ctx)
	if err != nil || !claimed || claimedJob.ActivityID != pending.ActivityID {
		t.Fatalf("restart pending Claim = (%+v, %t, %v)", claimedJob, claimed, err)
	}
	if err := second.Complete(ctx, pending.ActivityID, Completion{State: StateIgnored}); err != nil {
		t.Fatalf("complete resumed pending activity: %v", err)
	}
	if _, claimed, err := second.Claim(ctx); err != nil || claimed {
		t.Fatalf("post-resume Claim = (%t, %v), want no replay", claimed, err)
	}

	ignored := job
	ignored.ActivityID = "https://partner.example/activities/ignored-4"
	ignored.Body = []byte(`{"type":"Create","object":{"content":"unrelated"}}`)
	ignoredRecord, inserted, err := second.Ignore(ctx, ignored)
	if err != nil || !inserted || ignoredRecord.State != StateIgnored || len(ignoredRecord.Body) != 0 {
		t.Fatalf("atomic Ignore = (%+v, %t, %v)", ignoredRecord, inserted, err)
	}
	if _, claimed, err := second.Claim(ctx); err != nil || claimed {
		t.Fatalf("claim after Ignore = (%t, %v), want no work", claimed, err)
	}
	if duplicate, inserted, err := second.Enqueue(ctx, ignored); err != nil || inserted || duplicate.StatusToken != ignoredRecord.StatusToken {
		t.Fatalf("ignored duplicate = (%+v, %t, %v)", duplicate, inserted, err)
	}

	// Concurrent unique intake cannot exceed the configured two pending/running records. The
	// transaction-scoped advisory lock makes this true across database connections and replicas.
	const attempts = 8
	type admission struct {
		job      Job
		inserted bool
		err      error
	}
	results := make(chan admission, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := job
			candidate.ActivityID = fmt.Sprintf("https://partner.example/activities/capacity-%d", i)
			_, inserted, err := second.Enqueue(ctx, candidate)
			results <- admission{job: candidate, inserted: inserted, err: err}
		}(i)
	}
	wg.Wait()
	close(results)
	insertedJobs := make(map[string]Job)
	capacityErrors := 0
	for result := range results {
		switch {
		case result.err == nil && result.inserted:
			insertedJobs[result.job.ActivityID] = result.job
		case errors.Is(result.err, ErrCapacity):
			capacityErrors++
		default:
			t.Fatalf("concurrent admission = (%t, %v)", result.inserted, result.err)
		}
	}
	if len(insertedJobs) != 2 || capacityErrors != attempts-2 {
		t.Fatalf("concurrent capacity = %d inserted, %d full", len(insertedJobs), capacityErrors)
	}
	for range insertedJobs {
		claimedJob, claimed, err := second.Claim(ctx)
		if err != nil || !claimed {
			t.Fatalf("capacity Claim = (%+v, %t, %v)", claimedJob, claimed, err)
		}
		if err := second.Complete(ctx, claimedJob.ActivityID, Completion{State: StateFailed}); err != nil {
			t.Fatalf("capacity Complete: %v", err)
		}
	}
	second.retention = time.Nanosecond
	time.Sleep(time.Millisecond)
	if err := second.Prune(ctx); err != nil {
		t.Fatalf("periodic Prune: %v", err)
	}
	if _, err := second.LookupStatus(ctx, record.StatusToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired status error = %v, want ErrNotFound", err)
	}
}
