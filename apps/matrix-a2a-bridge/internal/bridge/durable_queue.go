package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

// durableQueueConfig bounds the database-backed worker set. Owner is the unique process identity
// persisted in lease fences; callers must supply it rather than letting the queue invent one so
// process identity remains explicit and tests stay deterministic.
type durableQueueConfig struct {
	Owner             string
	Concurrency       int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	IdleClaimInterval time.Duration
}

type durableJobFunc func(context.Context, state.Job)

const durableMetricReadTimeout = 2 * time.Second

// durableQueueMetrics tracks the process-local view needed by the existing aggregate gauges.
// nonTerminal includes every durable job accepted by the ledger, including delayed and leased
// recovery work. active is the subset whose callback is executing in this process.
type durableQueueMetrics struct {
	mu          sync.Mutex
	nonTerminal int
	active      int
}

func newDurableQueueMetrics(nonTerminal int) *durableQueueMetrics {
	metrics := &durableQueueMetrics{nonTerminal: nonTerminal}
	metrics.publishLocked()
	return metrics
}

func (m *durableQueueMetrics) observeAdmission(inserted int) {
	if inserted <= 0 {
		return
	}
	m.mu.Lock()
	m.nonTerminal += inserted
	m.publishLocked()
	m.mu.Unlock()
}

func (m *durableQueueMetrics) ensureNonTerminalMinimum(persisted int) {
	m.mu.Lock()
	if persisted > m.nonTerminal {
		m.nonTerminal = persisted
		m.publishLocked()
	}
	m.mu.Unlock()
}

func (m *durableQueueMetrics) callbackStarted() {
	m.mu.Lock()
	m.active++
	m.publishLocked()
	m.mu.Unlock()
}

func (m *durableQueueMetrics) callbackFinished(terminal bool) {
	m.mu.Lock()
	if m.active > 0 {
		m.active--
	}
	if terminal && m.nonTerminal > 0 {
		m.nonTerminal--
	}
	m.publishLocked()
	m.mu.Unlock()
}

func (m *durableQueueMetrics) reset() {
	m.mu.Lock()
	m.nonTerminal = 0
	m.active = 0
	m.publishLocked()
	m.mu.Unlock()
}

func (m *durableQueueMetrics) publishLocked() {
	queued := max(m.nonTerminal-m.active, 0)
	queueDepth.Set(float64(queued))
	inflightDelegations.Set(float64(m.active))
}

// durableQueue uses one coordinator to claim globally ordered jobs. Only claimed jobs create
// goroutines (one callback plus one lease heartbeat), so an idle bridge never maintains a worker
// fleet that repeatedly polls the same database.
type durableQueue struct {
	ledger  state.Ledger
	log     *slog.Logger
	config  durableQueueConfig
	execute durableJobFunc

	processCtx context.Context
	claimCtx   context.Context
	stopClaims context.CancelFunc

	wake  chan struct{}
	slots chan struct{}
	done  chan struct{}

	active   sync.WaitGroup
	stopOnce sync.Once
	startMu  sync.RWMutex
	stopping bool
	metrics  *durableQueueMetrics
}

var (
	errLeaseExpired      = errors.New("durable delegation lease expired")
	errLeaseWindowUnsafe = errors.New("durable delegation lease renewal left an unsafe heartbeat window")
)

// newDurableQueue starts the single claim coordinator. Stop must be called during graceful
// shutdown; canceling processCtx is the bounded-grace fallback that also cancels active callbacks.
func newDurableQueue(
	processCtx context.Context,
	ledger state.Ledger,
	log *slog.Logger,
	config durableQueueConfig,
	execute durableJobFunc,
) (*durableQueue, error) {
	if processCtx == nil {
		return nil, fmt.Errorf("durable queue process context must not be nil")
	}
	if ledger == nil {
		return nil, fmt.Errorf("durable queue ledger must not be nil")
	}
	if execute == nil {
		return nil, fmt.Errorf("durable queue callback must not be nil")
	}
	if config.Owner == "" {
		return nil, fmt.Errorf("durable queue owner must not be empty")
	}
	if config.Concurrency <= 0 {
		return nil, fmt.Errorf("durable queue concurrency must be positive")
	}
	if config.LeaseDuration <= 0 {
		return nil, fmt.Errorf("durable queue lease duration must be positive")
	}
	halfLeaseCeiling := config.LeaseDuration/2 + config.LeaseDuration%2
	if config.HeartbeatInterval <= 0 || config.HeartbeatInterval >= halfLeaseCeiling {
		return nil, fmt.Errorf("durable queue heartbeat interval must be positive and less than half the lease duration")
	}
	if config.IdleClaimInterval <= 0 {
		return nil, fmt.Errorf("durable queue idle claim interval must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	nonTerminal, err := ledger.NonTerminalCount(processCtx)
	if err != nil {
		return nil, fmt.Errorf("count durable delegation backlog: %w", err)
	}
	if nonTerminal < 0 {
		return nil, fmt.Errorf("durable delegation backlog count must not be negative")
	}

	claimCtx, stopClaims := context.WithCancel(processCtx)
	queue := &durableQueue{
		ledger:     ledger,
		log:        log,
		config:     config,
		execute:    execute,
		processCtx: processCtx,
		claimCtx:   claimCtx,
		stopClaims: stopClaims,
		wake:       make(chan struct{}, 1),
		slots:      make(chan struct{}, config.Concurrency),
		done:       make(chan struct{}),
		metrics:    newDurableQueueMetrics(nonTerminal),
	}
	go queue.coordinate()
	return queue, nil
}

// ObserveAdmission adds newly accepted non-terminal jobs after their atomic ledger commit. Exact
// replays, legacy tombstones, and capacity-denied terminal rows are deliberately excluded.
func (q *durableQueue) ObserveAdmission(inserted int) {
	q.metrics.observeAdmission(inserted)
}

// ReconcileAdmissionReplay heals a transaction whose database commit succeeded but whose response
// was lost before ObserveAdmission ran. It only raises the tracked aggregate: lowering it could
// double-count a terminal callback that has committed its transition but has not returned yet.
func (q *durableQueue) ReconcileAdmissionReplay(ctx context.Context) {
	metricCtx, cancel := context.WithTimeout(ctx, durableMetricReadTimeout)
	defer cancel()
	persisted, err := q.ledger.NonTerminalCount(metricCtx)
	if err != nil {
		q.log.Warn("durable delegation admission metric reconciliation failed", "reason", "storage_error")
		return
	}
	q.metrics.ensureNonTerminalMinimum(persisted)
}

// Notify asks the coordinator to claim immediately after intake or an external state change. The
// edge-triggered single-slot channel deliberately coalesces bursts and never blocks transaction
// intake. Job completion calls Notify internally after releasing its concurrency slot.
func (q *durableQueue) Notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Stop prevents further claims, waits until the coordinator can no longer add callbacks, and then
// waits for active callbacks to finish. It does not cancel healthy work during the graceful window;
// the caller cancels processCtx when that window expires, which cancels callbacks and heartbeats.
func (q *durableQueue) Stop() {
	q.stopOnce.Do(func() {
		// Cancel a possibly blocked Claim before taking the writer side of the start gate. New
		// callbacks either observe the canceled claim context or wait for stopping to become true;
		// callbacks already holding the reader side are allowed to finish gracefully.
		q.stopClaims()
		q.startMu.Lock()
		q.stopping = true
		q.startMu.Unlock()
	})
	<-q.done
	q.active.Wait()
	q.metrics.reset()
}

func (q *durableQueue) coordinate() {
	defer close(q.done)
	for {
		if q.claimCtx.Err() != nil {
			return
		}
		if !q.acquireSlot() {
			if !q.waitForClaim(true) {
				return
			}
			continue
		}

		job, found, err := q.ledger.Claim(q.claimCtx, state.ClaimRequest{
			Owner:         q.config.Owner,
			Now:           time.Now(),
			LeaseDuration: q.config.LeaseDuration,
		})
		if err != nil {
			q.releaseSlot()
			if q.claimCtx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			// Never copy a database/driver error into logs: it may contain credentials, SQL values, or
			// content-bearing job evidence. The bounded interval below prevents outage spin.
			q.log.Warn("durable delegation claim failed; backing off", "reason", "storage_error")
			if !q.waitForClaim(false) {
				return
			}
			q.discardWake()
			continue
		}
		if !found {
			q.releaseSlot()
			if !q.waitForClaim(true) {
				return
			}
			continue
		}
		lease := job.LeaseToken()
		if lease.JobID != job.JobID || lease.Owner != q.config.Owner || !job.LeaseExpiresAt.After(time.Now()) {
			q.releaseSlot()
			q.log.Error("durable delegation claim returned an invalid lease", "reason", "invalid_lease")
			if !q.waitForClaim(false) {
				return
			}
			q.discardWake()
			continue
		}

		if !q.dispatch(job) {
			// A claim that raced graceful shutdown is recoverable after its lease expires. The start
			// gate linearizes callback dispatch with Stop, so no new work can cross that boundary.
			q.releaseSlot()
			return
		}
	}
}

func (q *durableQueue) dispatch(job state.Job) bool {
	q.startMu.RLock()
	defer q.startMu.RUnlock()
	if q.stopping || q.claimCtx.Err() != nil {
		return false
	}
	q.active.Add(1)
	go q.run(job)
	return true
}

func (q *durableQueue) acquireSlot() bool {
	select {
	case q.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (q *durableQueue) releaseSlot() {
	<-q.slots
}

// waitForClaim uses a fresh stopped timer rather than a process-lifetime ticker. An idle wake may
// bypass the interval; a storage error may not, so repeated Notify calls cannot turn an outage into
// a tight database loop.
func (q *durableQueue) waitForClaim(allowNotify bool) bool {
	timer := time.NewTimer(q.config.IdleClaimInterval)
	defer timer.Stop()
	if !allowNotify {
		select {
		case <-q.claimCtx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	select {
	case <-q.claimCtx.Done():
		return false
	case <-q.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (q *durableQueue) discardWake() {
	select {
	case <-q.wake:
	default:
	}
}

func (q *durableQueue) run(job state.Job) {
	defer q.active.Done()
	defer func() {
		q.releaseSlot()
		q.Notify()
	}()

	workCtx, cancelWork := context.WithCancel(q.processCtx)
	defer cancelWork()
	if q.claimCtx.Err() != nil {
		return
	}

	lease := job.LeaseToken()
	expiresAt, err := q.renewLease(q.claimCtx, lease, job.LeaseExpiresAt)
	if err != nil {
		if workCtx.Err() == nil && q.claimCtx.Err() == nil {
			q.logLeaseFailure(job.JobID, err)
		}
		return
	}

	finished := &atomic.Bool{}
	watchdog := q.armLeaseWatchdog(workCtx, cancelWork, job.JobID, expiresAt, finished)
	heartbeatDone := make(chan struct{})
	go q.heartbeat(workCtx, cancelWork, job.JobID, lease, expiresAt, watchdog, finished, heartbeatDone)

	executed := false
	q.startMu.RLock()
	if !q.stopping && q.claimCtx.Err() == nil && workCtx.Err() == nil {
		q.metrics.callbackStarted()
		q.execute(workCtx, job)
		executed = true
	}
	// Holding the read side through execute lets Stop establish a precise boundary without
	// serializing callbacks: existing readers finish, while new readers wait and then skip work.
	finished.Store(true)
	cancelWork()
	q.startMu.RUnlock()
	<-heartbeatDone
	if executed {
		q.finishCallbackMetrics(job.JobID)
	}
}

func (q *durableQueue) finishCallbackMetrics(jobID string) {
	metricCtx, cancel := context.WithTimeout(context.WithoutCancel(q.processCtx), durableMetricReadTimeout)
	defer cancel()
	persisted, found, err := q.ledger.Job(metricCtx, jobID)
	if err != nil {
		// The job remains conservatively counted as queued when its persisted outcome cannot be read.
		// Do not copy the storage error into logs because it may contain content-bearing row evidence.
		q.log.Warn("durable delegation metric reconciliation failed", "reason", "storage_error")
		q.metrics.callbackFinished(false)
		return
	}
	// A non-terminal job can never be removed by cleanup. A missing row therefore represents a
	// terminal tombstone that was already pruned after its retention window.
	q.metrics.callbackFinished(!found || persisted.State.Terminal())
}

func (q *durableQueue) heartbeat(
	ctx context.Context,
	cancelWork context.CancelFunc,
	jobID string,
	lease state.LeaseToken,
	expiresAt time.Time,
	watchdog *time.Timer,
	finished *atomic.Bool,
	done chan<- struct{},
) {
	defer close(done)
	defer func() { watchdog.Stop() }()
	ticker := time.NewTicker(q.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewedUntil, err := q.renewLease(ctx, lease, expiresAt)
			if err != nil {
				if ctx.Err() != nil || finished.Load() {
					return
				}
				if !watchdog.Stop() {
					// The expiry callback won the race and supplies the log. Cancellation here is
					// idempotent and ensures the callback fence is closed before returning.
					cancelWork()
					return
				}
				q.logLeaseFailure(jobID, err)
				cancelWork()
				return
			}
			if !watchdog.Stop() {
				// A renewal response received after the prior expiry cannot make already-overlapping
				// external work safe, even if the database happened to accept the heartbeat.
				cancelWork()
				return
			}
			expiresAt = renewedUntil
			watchdog = q.armLeaseWatchdog(ctx, cancelWork, jobID, expiresAt, finished)
		}
	}
}

func (q *durableQueue) renewLease(
	ctx context.Context,
	lease state.LeaseToken,
	currentExpiry time.Time,
) (time.Time, error) {
	heartbeatAt := time.Now()
	if !currentExpiry.After(heartbeatAt) {
		return time.Time{}, errLeaseExpired
	}
	heartbeatCtx, cancelHeartbeat := context.WithDeadline(ctx, currentExpiry)
	defer cancelHeartbeat()
	if err := q.ledger.Heartbeat(heartbeatCtx, lease, heartbeatAt, q.config.LeaseDuration); err != nil {
		return time.Time{}, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}

	renewedUntil := heartbeatAt.Add(q.config.LeaseDuration)
	if !renewedUntil.After(time.Now().Add(q.config.HeartbeatInterval)) {
		return time.Time{}, errLeaseWindowUnsafe
	}
	return renewedUntil, nil
}

func (q *durableQueue) armLeaseWatchdog(
	ctx context.Context,
	cancelWork context.CancelFunc,
	jobID string,
	expiresAt time.Time,
	finished *atomic.Bool,
) *time.Timer {
	return time.AfterFunc(time.Until(expiresAt), func() {
		if ctx.Err() != nil || finished.Load() {
			return
		}
		q.log.Warn(
			"durable delegation lease expired; canceling work",
			"job_id", jobID,
			"reason", "lease_expired",
		)
		cancelWork()
	})
}

func (q *durableQueue) logLeaseFailure(jobID string, err error) {
	reason := "storage_error"
	switch {
	case errors.Is(err, state.ErrLeaseLost):
		reason = "lease_lost"
	case errors.Is(err, errLeaseExpired), errors.Is(err, context.DeadlineExceeded):
		reason = "lease_expired"
	case errors.Is(err, errLeaseWindowUnsafe):
		reason = "lease_window_unsafe"
	}
	// Never include err: database/driver errors can contain credentials, SQL values, or job
	// evidence. The fixed reason code is enough to operate the queue safely.
	q.log.Warn(
		"durable delegation heartbeat failed; canceling work",
		"job_id", jobID,
		"reason", reason,
	)
}
