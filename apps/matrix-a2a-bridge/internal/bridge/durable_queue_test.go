package bridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

func TestNewDurableQueueValidatesLeaseWorkerContract(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*durableQueueConfig)
	}{
		{name: "owner", mutate: func(config *durableQueueConfig) { config.Owner = "" }},
		{name: "concurrency", mutate: func(config *durableQueueConfig) { config.Concurrency = 0 }},
		{name: "lease", mutate: func(config *durableQueueConfig) { config.LeaseDuration = 0 }},
		{name: "heartbeat zero", mutate: func(config *durableQueueConfig) { config.HeartbeatInterval = 0 }},
		{name: "heartbeat at half lease", mutate: func(config *durableQueueConfig) {
			config.HeartbeatInterval = config.LeaseDuration / 2
		}},
		{name: "idle interval", mutate: func(config *durableQueueConfig) { config.IdleClaimInterval = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testDurableQueueConfig(1)
			tt.mutate(&config)
			queue, err := newDurableQueue(
				t.Context(),
				newFakeQueueLedger(),
				slog.Default(),
				config,
				func(context.Context, state.Job) {},
			)
			if err == nil {
				queue.Stop()
				t.Fatal("newDurableQueue succeeded with invalid config")
			}
		})
	}

	// One nanosecond is strictly less than half a three-nanosecond lease. This pins the exact
	// inequality instead of accidentally rejecting odd-duration values through integer truncation.
	config := testDurableQueueConfig(1)
	config.LeaseDuration = 3 * time.Nanosecond
	config.HeartbeatInterval = time.Nanosecond
	queue, err := newDurableQueue(
		t.Context(),
		newFakeQueueLedger(),
		slog.Default(),
		config,
		func(context.Context, state.Job) {},
	)
	if err != nil {
		t.Fatalf("newDurableQueue rejected heartbeat below half lease: %v", err)
	}
	queue.Stop()
}

func TestDurableQueueConcurrencyCap(t *testing.T) {
	const (
		limit = 3
		jobs  = 12
	)
	ledger := newFakeQueueLedger(fakeJobs(jobs)...)
	release := make(chan struct{})
	started := make(chan struct{}, jobs)
	done := make(chan struct{}, jobs)
	var inFlight atomic.Int32
	var peak atomic.Int32
	queue := newTestDurableQueue(t, ledger, limit, func(context.Context, state.Job) {
		current := inFlight.Add(1)
		updatePeak(&peak, current)
		started <- struct{}{}
		<-release
		inFlight.Add(-1)
		done <- struct{}{}
	})

	for range limit {
		waitSignal(t, started, "initial callbacks")
	}
	select {
	case <-started:
		t.Fatalf("more than %d callbacks started before capacity was released", limit)
	default:
	}
	close(release)
	for range jobs {
		waitSignal(t, done, "completed callbacks")
	}
	queue.Stop()

	if got := peak.Load(); got != limit {
		t.Fatalf("peak callbacks = %d, want %d", got, limit)
	}
}

func TestDurableQueueNotifyClaimsWithoutIdleDelay(t *testing.T) {
	ledger := newFakeQueueLedger()
	started := make(chan struct{}, 1)
	queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {
		started <- struct{}{}
	})
	waitSignal(t, ledger.claimed, "initial empty claim")

	ledger.enqueue(fakeJob("notified"))
	queue.Notify()
	waitSignal(t, started, "notified callback")
	queue.Stop()
}

func TestDurableQueueMetricsRecoverStartupBacklogAndResetOnStop(t *testing.T) {
	ledger := newFakeQueueLedger(fakeJob("delayed"), fakeJob("leased"))
	ledger.claimFn = func(ctx context.Context, _ state.ClaimRequest) (state.Job, bool, error) {
		<-ctx.Done()
		return state.Job{}, false, ctx.Err()
	}
	queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {})
	waitSignal(t, ledger.claimed, "blocked startup claim")
	assertDurableGauges(t, 2, 0)

	queue.Stop()
	assertDurableGauges(t, 0, 0)
}

func TestDurableQueueMetricsObserveCommittedAdmissionBeforeNotify(t *testing.T) {
	ledger := newFakeQueueLedger()
	queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {})
	waitSignal(t, ledger.claimed, "initial empty claim")

	ledger.enqueue(fakeJob("admitted"))
	queue.ObserveAdmission(1)
	assertDurableGauges(t, 1, 0)
}

func TestDurableQueueMetricsReplayReconciliationHealsLostAdmissionResponse(t *testing.T) {
	ledger := newFakeQueueLedger()
	transitioned := make(chan struct{})
	release := make(chan struct{})
	queue := newTestDurableQueue(t, ledger, 1, func(_ context.Context, job state.Job) {
		if err := ledger.Transition(context.Background(), state.TransitionRequest{
			Lease: job.LeaseToken(), From: job.State, To: state.StateDead, At: time.Now(),
		}); err != nil {
			t.Errorf("terminal Transition: %v", err)
		}
		close(transitioned)
		<-release
	})
	waitSignal(t, ledger.claimed, "initial empty claim")

	// Model a committed admission whose database acknowledgement was lost: the job exists, but the
	// process never observed its inserted ID. An exact transaction replay performs this floor repair.
	ledger.enqueue(fakeJob("lost-admission-response"))
	queue.ReconcileAdmissionReplay(t.Context())
	assertDurableGauges(t, 1, 0)

	queue.Notify()
	waitClosed(t, transitioned, "terminal callback before local return")
	assertDurableGauges(t, 0, 1)
	queue.ReconcileAdmissionReplay(t.Context())
	queue.metrics.mu.Lock()
	nonTerminal, active := queue.metrics.nonTerminal, queue.metrics.active
	queue.metrics.mu.Unlock()
	if nonTerminal != 1 || active != 1 {
		t.Fatalf("replay floor during terminal callback = total %d active %d, want 1/1", nonTerminal, active)
	}

	close(release)
	waitDurableQueueSettled(t, 0)
	queue.Stop()
}

func TestDurableQueueMetricsReplayReconciliationUsesBoundedContext(t *testing.T) {
	ledger := newFakeQueueLedger()
	queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {})
	waitSignal(t, ledger.claimed, "initial empty claim")

	deadline := make(chan time.Duration, 1)
	ledger.setCountFunc(func(ctx context.Context) (int, error) {
		boundedUntil, ok := ctx.Deadline()
		if !ok {
			return 0, errors.New("metric reconciliation context has no deadline")
		}
		deadline <- time.Until(boundedUntil)
		return 0, errors.New("forced metrics-only count failure")
	})
	queue.ReconcileAdmissionReplay(context.Background())
	remaining := waitValue(t, deadline, "metrics-only reconciliation deadline")
	if remaining <= 0 || remaining > durableMetricReadTimeout {
		t.Fatalf("reconciliation deadline = %s, want within (0, %s]", remaining, durableMetricReadTimeout)
	}
}

func TestDurableQueueMetricsTrackRetryAndPersistedTerminalOutcome(t *testing.T) {
	t.Run("non-terminal callback returns to queue", func(t *testing.T) {
		ledger := newFakeQueueLedger(fakeJob("retry"))
		started := make(chan struct{})
		release := make(chan struct{})
		queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {
			close(started)
			<-release
		})
		waitClosed(t, started, "retry callback")
		assertDurableGauges(t, 0, 1)
		close(release)
		waitDurableQueueSettled(t, 1)
		queue.Stop()
	})

	t.Run("persisted terminal transition leaves queue", func(t *testing.T) {
		ledger := newFakeQueueLedger(fakeJob("terminal"))
		queue := newTestDurableQueue(t, ledger, 1, func(_ context.Context, job state.Job) {
			if err := ledger.Transition(context.Background(), state.TransitionRequest{
				Lease: job.LeaseToken(), From: job.State, To: state.StateDead, At: time.Now(),
			}); err != nil {
				t.Errorf("terminal Transition: %v", err)
			}
		})
		waitDurableQueueSettled(t, 0)
		queue.Stop()
	})

	t.Run("lost terminal transition response follows database state", func(t *testing.T) {
		ledger := newFakeQueueLedger(fakeJob("lost-transition-response"))
		ledger.transitionErrAfterCommit = errors.New("lost database response")
		transitionReturned := make(chan error, 1)
		queue := newTestDurableQueue(t, ledger, 1, func(_ context.Context, job state.Job) {
			transitionReturned <- ledger.Transition(context.Background(), state.TransitionRequest{
				Lease: job.LeaseToken(), From: job.State, To: state.StateDead, At: time.Now(),
			})
		})
		if err := waitValue(t, transitionReturned, "lost transition response"); err == nil {
			t.Fatal("terminal transition unexpectedly returned nil")
		}
		waitDurableQueueSettled(t, 0)
		queue.Stop()
	})
}

func TestDurableQueueMetricsClampClaimBeforeAdmissionObservation(t *testing.T) {
	ledger := newFakeQueueLedger()
	job := fakeJob("claim-before-observe")
	var claimed atomic.Bool
	ledger.claimFn = func(context.Context, state.ClaimRequest) (state.Job, bool, error) {
		if !claimed.CompareAndSwap(false, true) {
			return state.Job{}, false, nil
		}
		return job, true, nil
	}
	started := make(chan struct{})
	release := make(chan struct{})
	queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {
		close(started)
		<-release
	})
	waitClosed(t, started, "callback claimed before admission observation")
	assertDurableGauges(t, 0, 1)

	queue.ObserveAdmission(1)
	assertDurableGauges(t, 0, 1)
	close(release)
	waitDurableQueueSettled(t, 1)
	queue.Stop()
}

func TestDurableQueueStopWaitsWithoutClaimingMoreWork(t *testing.T) {
	processCtx, cancelProcess := context.WithCancel(t.Context())
	defer cancelProcess()
	ledger := newFakeQueueLedger(fakeJob("first"), fakeJob("second"))
	started := make(chan string, 2)
	release := make(chan struct{})
	canceled := make(chan bool, 1)
	queue := mustNewDurableQueue(
		t,
		processCtx,
		ledger,
		testDurableQueueConfig(1),
		func(ctx context.Context, job state.Job) {
			started <- job.JobID
			if job.JobID != "first" {
				return
			}
			select {
			case <-ctx.Done():
				canceled <- true
			case <-release:
				canceled <- false
			}
		},
	)
	if got := waitValue(t, started, "first callback"); got != "first" {
		t.Fatalf("first callback = %q", got)
	}

	stopped := make(chan struct{})
	go func() {
		queue.Stop()
		close(stopped)
	}()
	waitClosed(t, queue.done, "claim coordinator")
	select {
	case <-stopped:
		t.Fatal("Stop returned before the active callback finished")
	default:
	}
	close(release)
	waitClosed(t, stopped, "durable queue Stop")

	if <-canceled {
		t.Fatal("graceful Stop canceled healthy callback before process grace expired")
	}
	select {
	case jobID := <-started:
		t.Fatalf("callback %q started after Stop", jobID)
	default:
	}
	if got := ledger.claimCalls.Load(); got != 1 {
		t.Fatalf("Claim calls = %d, want one before Stop", got)
	}
}

func TestDurableQueueStopFencesClaimThatIgnoresCancellation(t *testing.T) {
	ledger := newFakeQueueLedger(fakeJob("stop-race"))
	claimCanceled := make(chan struct{})
	releaseClaim := make(chan struct{})
	releaseOnce := sync.Once{}
	defer releaseOnce.Do(func() { close(releaseClaim) })
	ledger.claimFn = func(ctx context.Context, _ state.ClaimRequest) (state.Job, bool, error) {
		<-ctx.Done()
		close(claimCanceled)
		<-releaseClaim
		// A storage driver should honor cancellation, but the queue's Stop boundary must remain
		// safe even if an in-flight Claim returns a row after cancellation.
		return fakeJob("stop-race"), true, nil
	}
	started := make(chan struct{}, 1)
	queue := newTestDurableQueue(t, ledger, 1, func(context.Context, state.Job) {
		started <- struct{}{}
	})
	waitSignal(t, ledger.claimed, "blocked claim")
	assertDurableGauges(t, 1, 0)

	stopped := make(chan struct{})
	go func() {
		queue.Stop()
		close(stopped)
	}()
	waitClosed(t, claimCanceled, "claim cancellation")
	releaseOnce.Do(func() { close(releaseClaim) })
	waitClosed(t, stopped, "Stop after canceled claim returned")
	assertDurableGauges(t, 0, 0)

	select {
	case <-started:
		t.Fatal("callback started from a claim returned after Stop")
	default:
	}
	if got := ledger.heartbeatCalls.Load(); got != 0 {
		t.Fatalf("Heartbeat calls = %d, want none for a Stop-fenced claim", got)
	}
}

func TestDurableQueueProcessCancellationStopsClaimsAndCallbacks(t *testing.T) {
	processCtx, cancelProcess := context.WithCancel(t.Context())
	ledger := newFakeQueueLedger(fakeJob("running"), fakeJob("must-not-start"))
	started := make(chan string, 2)
	canceled := make(chan error, 1)
	queue := mustNewDurableQueue(
		t,
		processCtx,
		ledger,
		testDurableQueueConfig(1),
		func(ctx context.Context, job state.Job) {
			started <- job.JobID
			<-ctx.Done()
			canceled <- ctx.Err()
		},
	)
	if got := waitValue(t, started, "running callback"); got != "running" {
		t.Fatalf("first callback = %q", got)
	}

	cancelProcess()
	if err := waitValue(t, canceled, "process cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback context error = %v, want canceled", err)
	}
	queue.Stop()
	select {
	case jobID := <-started:
		t.Fatalf("callback %q started after process cancellation", jobID)
	default:
	}
}

func TestDurableQueueHeartbeatLossCancelsAndFencesCallback(t *testing.T) {
	ledger := newFakeQueueLedger(fakeJob("lease-loss"))
	const sensitiveDetail = "postgres row contained secret prompt"
	var heartbeatAttempt atomic.Int32
	ledger.heartbeatFn = func(_ context.Context, lease state.LeaseToken, _ time.Time, _ time.Duration) error {
		if heartbeatAttempt.Add(1) == 1 {
			// run renews immediately before invoking external work; lose the lease on the first
			// periodic heartbeat so this test exercises cancellation of an active callback.
			return nil
		}
		ledger.loseLease(lease.JobID)
		return errors.Join(&state.LeaseLostError{JobID: lease.JobID}, errors.New(sensitiveDetail))
	}
	var output lockedBuffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	canceled := make(chan error, 1)
	staleCommit := make(chan error, 1)
	config := testDurableQueueConfig(1)
	config.LeaseDuration = 200 * time.Millisecond
	config.HeartbeatInterval = 20 * time.Millisecond
	queue := mustNewDurableQueue(t, t.Context(), ledger, config, func(ctx context.Context, job state.Job) {
		<-ctx.Done()
		canceled <- ctx.Err()
		staleCommit <- ledger.Transition(context.Background(), state.TransitionRequest{
			Lease: job.LeaseToken(),
			From:  job.State,
			To:    state.StateDead,
			At:    time.Now(),
		})
	}, logger)

	if err := waitValue(t, canceled, "lease-loss cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback context error = %v, want canceled", err)
	}
	if err := waitValue(t, staleCommit, "stale transition"); !errors.Is(err, state.ErrLeaseLost) {
		t.Fatalf("stale transition error = %v, want ErrLeaseLost", err)
	}
	waitDurableQueueSettled(t, 1)
	queue.Stop()

	if got := ledger.heartbeatCalls.Load(); got < 2 {
		t.Fatalf("Heartbeat calls = %d, want immediate renewal plus periodic heartbeat", got)
	}
	if bytes.Contains(output.Bytes(), []byte(sensitiveDetail)) {
		t.Fatalf("heartbeat log leaked storage content: %s", output.Bytes())
	}
}

func TestDurableQueueHeartbeatStallCancelsBeforeLeaseTakeover(t *testing.T) {
	ledger := newFakeQueueLedger(fakeJob("heartbeat-stall"))
	renewedUntil := make(chan time.Time, 1)
	heartbeatBlocked := make(chan struct{})
	var heartbeatAttempt atomic.Int32
	ledger.heartbeatFn = func(
		ctx context.Context,
		_ state.LeaseToken,
		now time.Time,
		duration time.Duration,
	) error {
		switch heartbeatAttempt.Add(1) {
		case 1:
			renewedUntil <- now.Add(duration)
			return nil
		case 2:
			close(heartbeatBlocked)
			<-ctx.Done()
			return ctx.Err()
		default:
			return errors.New("unexpected heartbeat after stalled renewal")
		}
	}

	callbackStarted := make(chan struct{})
	callbackCanceled := make(chan struct{})
	allowStaleCommit := make(chan struct{})
	allowCommitOnce := sync.Once{}
	defer allowCommitOnce.Do(func() { close(allowStaleCommit) })
	staleCommit := make(chan error, 1)
	var callbackActive atomic.Bool
	config := testDurableQueueConfig(1)
	config.LeaseDuration = 240 * time.Millisecond
	config.HeartbeatInterval = 20 * time.Millisecond
	queue := mustNewDurableQueue(t, t.Context(), ledger, config, func(ctx context.Context, job state.Job) {
		callbackActive.Store(true)
		close(callbackStarted)
		<-ctx.Done()
		callbackActive.Store(false)
		close(callbackCanceled)
		<-allowStaleCommit
		staleCommit <- ledger.Transition(context.Background(), state.TransitionRequest{
			Lease: job.LeaseToken(),
			From:  job.State,
			To:    state.StateDead,
			At:    time.Now(),
		})
	})

	waitClosed(t, callbackStarted, "callback after immediate renewal")
	expiresAt := waitValue(t, renewedUntil, "renewed lease expiry")
	waitClosed(t, heartbeatBlocked, "stalled periodic heartbeat")
	waitClosed(t, callbackCanceled, "lease-expiry watchdog cancellation")
	if callbackActive.Load() {
		t.Fatal("callback remained active after its lease-expiry watchdog fired")
	}
	waitUntil(t, expiresAt)
	if _, ok := ledger.reclaimExpired("heartbeat-stall", time.Now(), "takeover-owner"); !ok {
		t.Fatal("expired lease was not available for takeover")
	}
	if callbackActive.Load() {
		t.Fatal("stale callback overlapped the takeover owner")
	}
	allowCommitOnce.Do(func() { close(allowStaleCommit) })
	if err := waitValue(t, staleCommit, "post-takeover stale transition"); !errors.Is(err, state.ErrLeaseLost) {
		t.Fatalf("stale transition error = %v, want ErrLeaseLost", err)
	}
	queue.Stop()
}

func TestDurableQueueCompletionRefillsCapacity(t *testing.T) {
	ledger := newFakeQueueLedger(fakeJob("first"), fakeJob("second"))
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	queue := newTestDurableQueue(t, ledger, 1, func(_ context.Context, job state.Job) {
		switch job.JobID {
		case "first":
			close(firstStarted)
			<-releaseFirst
		case "second":
			close(secondStarted)
		}
	})
	waitClosed(t, firstStarted, "first callback")
	select {
	case <-secondStarted:
		t.Fatal("second callback started before capacity was released")
	default:
	}
	close(releaseFirst)
	waitClosed(t, secondStarted, "completion-triggered refill")
	queue.Stop()
}

func TestDurableQueueUsesOneIdleCoordinatorAndNotifyNeverBlocks(t *testing.T) {
	ledger := newFakeQueueLedger()
	queue := newTestDurableQueue(t, ledger, 32, func(context.Context, state.Job) {})
	waitSignal(t, ledger.claimed, "initial idle claim")
	if got := ledger.claimCalls.Load(); got != 1 {
		t.Fatalf("idle Claim calls = %d, want one coordinator call, not one per worker", got)
	}
	if got := ledger.maxConcurrentClaims.Load(); got != 1 {
		t.Fatalf("concurrent Claim calls = %d, want one", got)
	}

	queue.Notify()
	waitSignal(t, ledger.claimed, "notified idle claim")
	if got := ledger.claimCalls.Load(); got != 2 {
		t.Fatalf("Claim calls after one Notify = %d, want 2", got)
	}
	queue.Stop()

	// A stopped coordinator leaves the one-slot wake full, proving repeated notifications still
	// take the nonblocking default path rather than waiting for a receiver.
	notified := make(chan struct{})
	go func() {
		for range 10_000 {
			queue.Notify()
		}
		close(notified)
	}()
	waitClosed(t, notified, "nonblocking Notify burst")
}

func TestDurableQueueClaimErrorsBackOffAndStayContentFree(t *testing.T) {
	const sensitiveDetail = "postgres://bridge:secret@database/content"
	ledger := newFakeQueueLedger()
	ledger.claimErr = errors.New(sensitiveDetail)
	var output lockedBuffer
	queue := mustNewDurableQueue(
		t,
		t.Context(),
		ledger,
		testDurableQueueConfig(8),
		func(context.Context, state.Job) {},
		slog.New(slog.NewTextHandler(&output, nil)),
	)
	waitSignal(t, ledger.claimed, "failed claim")
	waitCondition(t, "content-free claim warning", func() bool {
		return bytes.Contains(output.Bytes(), []byte("durable delegation claim failed"))
	})

	for range 1_000 {
		queue.Notify()
	}
	if got := ledger.claimCalls.Load(); got != 1 {
		t.Fatalf("Claim calls during error backoff = %d, want one", got)
	}
	queue.Stop()
	if bytes.Contains(output.Bytes(), []byte(sensitiveDetail)) {
		t.Fatalf("claim log leaked storage content: %s", output.Bytes())
	}
}

func newTestDurableQueue(
	t *testing.T,
	ledger state.Ledger,
	concurrency int,
	execute durableJobFunc,
) *durableQueue {
	t.Helper()
	return mustNewDurableQueue(t, t.Context(), ledger, testDurableQueueConfig(concurrency), execute)
}

func mustNewDurableQueue(
	t *testing.T,
	ctx context.Context,
	ledger state.Ledger,
	config durableQueueConfig,
	execute durableJobFunc,
	logs ...*slog.Logger,
) *durableQueue {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if len(logs) > 0 {
		logger = logs[0]
	}
	queue, err := newDurableQueue(ctx, ledger, logger, config, execute)
	if err != nil {
		t.Fatalf("newDurableQueue: %v", err)
	}
	t.Cleanup(func() { queue.Stop() })
	return queue
}

func testDurableQueueConfig(concurrency int) durableQueueConfig {
	return durableQueueConfig{
		Owner:             "test-owner",
		Concurrency:       concurrency,
		LeaseDuration:     2 * time.Second,
		HeartbeatInterval: 250 * time.Millisecond,
		IdleClaimInterval: time.Hour,
	}
}

func fakeJobs(count int) []state.Job {
	jobs := make([]state.Job, 0, count)
	for i := range count {
		jobs = append(jobs, fakeJob("job-"+time.Unix(int64(i), 0).UTC().Format("150405")))
	}
	return jobs
}

func fakeJob(id string) state.Job {
	return state.Job{JobID: id, RoomID: "!" + id + ":test", State: state.StatePending}
}

type fakeQueueLedger struct {
	mu     sync.Mutex
	jobs   []state.Job
	stored map[string]state.Job
	leases map[string]fakeQueueLease

	countErr                 error
	countFn                  func(context.Context) (int, error)
	jobErr                   error
	transitionErrAfterCommit error
	claimErr                 error
	claimFn                  func(context.Context, state.ClaimRequest) (state.Job, bool, error)
	heartbeatFn              func(context.Context, state.LeaseToken, time.Time, time.Duration) error

	claimCalls          atomic.Int32
	heartbeatCalls      atomic.Int32
	claimsInFlight      atomic.Int32
	maxConcurrentClaims atomic.Int32
	claimed             chan struct{}
}

type fakeQueueLease struct {
	token     state.LeaseToken
	expiresAt time.Time
}

func newFakeQueueLedger(jobs ...state.Job) *fakeQueueLedger {
	stored := make(map[string]state.Job, len(jobs))
	for _, job := range jobs {
		stored[job.JobID] = job
	}
	return &fakeQueueLedger{
		jobs:    append([]state.Job(nil), jobs...),
		stored:  stored,
		leases:  make(map[string]fakeQueueLease),
		claimed: make(chan struct{}, 128),
	}
}

func (f *fakeQueueLedger) enqueue(job state.Job) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.stored[job.JobID] = job
	f.mu.Unlock()
}

func (f *fakeQueueLedger) setCountFunc(countFn func(context.Context) (int, error)) {
	f.mu.Lock()
	f.countFn = countFn
	f.mu.Unlock()
}

func (f *fakeQueueLedger) loseLease(jobID string) {
	f.mu.Lock()
	delete(f.leases, jobID)
	f.mu.Unlock()
}

func (f *fakeQueueLedger) reclaimExpired(
	jobID string,
	now time.Time,
	owner string,
) (state.LeaseToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.leases[jobID]
	if !ok || current.expiresAt.After(now) {
		return state.LeaseToken{}, false
	}
	token := state.LeaseToken{
		JobID:      jobID,
		Owner:      owner,
		Generation: current.token.Generation + 1,
	}
	f.leases[jobID] = fakeQueueLease{token: token, expiresAt: now.Add(time.Hour)}
	job := f.stored[jobID]
	job.LeaseOwner = token.Owner
	job.LeaseGeneration = token.Generation
	job.LeaseExpiresAt = now.Add(time.Hour)
	f.stored[jobID] = job
	return token, true
}

func (f *fakeQueueLedger) NonTerminalCount(ctx context.Context) (int, error) {
	f.mu.Lock()
	countFn := f.countFn
	if f.countErr != nil {
		err := f.countErr
		f.mu.Unlock()
		return 0, err
	}
	count := 0
	for _, job := range f.stored {
		if !job.State.Terminal() {
			count++
		}
	}
	f.mu.Unlock()
	if countFn != nil {
		return countFn(ctx)
	}
	return count, nil
}

func (f *fakeQueueLedger) Claim(ctx context.Context, request state.ClaimRequest) (state.Job, bool, error) {
	current := f.claimsInFlight.Add(1)
	updatePeak(&f.maxConcurrentClaims, current)
	defer f.claimsInFlight.Add(-1)
	f.claimCalls.Add(1)
	select {
	case f.claimed <- struct{}{}:
	default:
	}
	if err := ctx.Err(); err != nil {
		return state.Job{}, false, err
	}
	if f.claimFn != nil {
		job, found, err := f.claimFn(ctx, request)
		if err != nil || !found {
			return job, found, err
		}
		return f.recordClaim(job, request), true, nil
	}

	f.mu.Lock()
	if f.claimErr != nil {
		defer f.mu.Unlock()
		return state.Job{}, false, f.claimErr
	}
	if len(f.jobs) == 0 {
		f.mu.Unlock()
		return state.Job{}, false, nil
	}
	job := f.jobs[0]
	f.jobs = f.jobs[1:]
	f.mu.Unlock()
	return f.recordClaim(job, request), true, nil
}

func (f *fakeQueueLedger) recordClaim(job state.Job, request state.ClaimRequest) state.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	job.LeaseOwner = request.Owner
	job.LeaseGeneration = max(job.LeaseGeneration+1, f.leases[job.JobID].token.Generation+1)
	job.LeaseExpiresAt = request.Now.Add(request.LeaseDuration)
	f.leases[job.JobID] = fakeQueueLease{token: job.LeaseToken(), expiresAt: job.LeaseExpiresAt}
	f.stored[job.JobID] = job
	return job
}

func (f *fakeQueueLedger) Heartbeat(
	ctx context.Context,
	lease state.LeaseToken,
	now time.Time,
	duration time.Duration,
) error {
	f.heartbeatCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	current, ok := f.leases[lease.JobID]
	f.mu.Unlock()
	if !ok || current.token != lease || !current.expiresAt.After(now) {
		return &state.LeaseLostError{JobID: lease.JobID}
	}
	if f.heartbeatFn != nil {
		if err := f.heartbeatFn(ctx, lease, now, duration); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok = f.leases[lease.JobID]
	if !ok || current.token != lease || !current.expiresAt.After(now) {
		return &state.LeaseLostError{JobID: lease.JobID}
	}
	current.expiresAt = now.Add(duration)
	f.leases[lease.JobID] = current
	return nil
}

func (f *fakeQueueLedger) Transition(_ context.Context, request state.TransitionRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.leases[request.Lease.JobID]
	if !ok || current.token != request.Lease || !current.expiresAt.After(request.At) {
		return &state.LeaseLostError{JobID: request.Lease.JobID}
	}
	job := f.stored[request.Lease.JobID]
	job.State = request.To
	if request.To.Terminal() {
		job.TerminalAt = request.At
		job.LeaseOwner = ""
		job.LeaseExpiresAt = time.Time{}
		delete(f.leases, request.Lease.JobID)
	}
	f.stored[request.Lease.JobID] = job
	return f.transitionErrAfterCommit
}

func (*fakeQueueLedger) AdmitTransaction(context.Context, state.TransactionAdmission) (state.AdmissionResult, error) {
	return state.AdmissionResult{}, nil
}

func (*fakeQueueLedger) RecordAdmission(context.Context, state.AdmissionRequest) error { return nil }

func (*fakeQueueLedger) RecordMatrixEvent(context.Context, state.MatrixEventRequest) error {
	return nil
}

func (*fakeQueueLedger) RecordDeadMan(context.Context, state.DeadManRequest) error { return nil }

func (f *fakeQueueLedger) ScheduleRetry(_ context.Context, request state.RetryRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.leases[request.Lease.JobID]
	if !ok || current.token != request.Lease || !current.expiresAt.After(request.At) {
		return &state.LeaseLostError{JobID: request.Lease.JobID}
	}
	job := f.stored[request.Lease.JobID]
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
	job.NextAttemptAt = request.NextAttemptAt
	delete(f.leases, request.Lease.JobID)
	f.stored[request.Lease.JobID] = job
	f.jobs = append(f.jobs, job)
	return nil
}

func (f *fakeQueueLedger) Job(_ context.Context, jobID string) (state.Job, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobErr != nil {
		return state.Job{}, false, f.jobErr
	}
	job, found := f.stored[jobID]
	return job, found, nil
}

func (*fakeQueueLedger) CleanupTerminal(context.Context, time.Time) (state.CleanupResult, error) {
	return state.CleanupResult{}, nil
}

func updatePeak(peak *atomic.Int32, value int32) {
	for {
		current := peak.Load()
		if value <= current || peak.CompareAndSwap(current, value) {
			return
		}
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	waitValue(t, ch, description)
}

func waitClosed(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	waitSignal(t, ch, description)
}

func waitValue[T any](t *testing.T, ch <-chan T, description string) T {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}

func waitCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func assertDurableGauges(t *testing.T, queued, inflight float64) {
	t.Helper()
	if got := queueDepthValue(t); got != queued {
		t.Fatalf("fgentic_queue_depth = %v, want %v", got, queued)
	}
	if got := inflightDelegationsValue(t); got != inflight {
		t.Fatalf("fgentic_inflight_delegations = %v, want %v", got, inflight)
	}
}

func waitDurableQueueSettled(t *testing.T, queued float64) {
	t.Helper()
	waitCondition(t, "durable queue gauges", func() bool {
		return queueDepthValue(t) == queued && inflightDelegationsValue(t) == 0
	})
}

func inflightDelegationsValue(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "fgentic_inflight_delegations" {
			continue
		}
		metrics := family.GetMetric()
		if len(metrics) != 1 || metrics[0].Gauge == nil {
			t.Fatalf("fgentic_inflight_delegations metrics = %d, want one gauge", len(metrics))
		}
		return metrics[0].GetGauge().GetValue()
	}
	t.Fatal("fgentic_inflight_delegations metric not found")
	return 0
}

func waitUntil(t *testing.T, deadline time.Time) {
	t.Helper()
	if delay := time.Until(deadline); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatalf("test context ended before %s: %v", deadline, t.Context().Err())
		}
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes())
}
