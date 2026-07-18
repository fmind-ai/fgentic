// Package activitystate persists ActivityPub inbox work before asynchronous delegation. The
// activity IRI is the idempotency key: one accepted federated activity can create at most one
// processing pass, even when its sender retries or the gateway restarts.
package activitystate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrConflict means an existing activity ID was replayed with different immutable intake data.
	// Treating that as a cached retry would let one actor suppress another activity by ID collision.
	ErrConflict = errors.New("activity state: activity ID conflicts with its original intake")
	// ErrCapacity applies backpressure before another unique activity can occupy the durable queue.
	ErrCapacity = errors.New("activity state: pending queue is full")
	// ErrNotFound hides whether an invalid opaque status token was ever issued.
	ErrNotFound = errors.New("activity state: status not found")
)

const (
	// MaxBodyBytes is the shared HTTP and persistence ceiling for one untrusted activity document.
	MaxBodyBytes = 1 << 20
	// MaxResultBytes bounds one cached agent reply served through the opaque status capability.
	MaxResultBytes   = 1 << 20
	statusTokenBytes = 24
)

// Route identifies which bounded inbox processor owns an activity.
type Route string

const (
	// RouteAgent processes a Create(Note) delivered to one agent inbox.
	RouteAgent Route = "agent"
	// RouteGroup processes a Create(Note) delivered to one collaboration Group inbox.
	RouteGroup Route = "group"
)

// State is the durable, content-free processing outcome.
type State string

const (
	// StatePending is committed work that has not begun external side effects.
	StatePending State = "pending"
	// StateRunning means the one allowed processing pass may have started external calls.
	StateRunning State = "running"
	// StateSucceeded records a completed delegation and optional cached reply.
	StateSucceeded State = "succeeded"
	// StateDenied records a unique activity rejected by the budget gate.
	StateDenied State = "denied"
	// StateFailed records an execution error or an interrupted, outcome-unknown attempt.
	StateFailed State = "failed"
	// StateIgnored records a valid activity that did not address a local agent.
	StateIgnored State = "ignored"
)

// Job is the bounded work record committed before a public inbox returns 202. Body is capped by
// the HTTP boundary before storage and erased as soon as the record becomes terminal.
type Job struct {
	ActivityID string
	Route      Route
	Target     string
	ActorURI   string
	Body       []byte
}

// Record is the cached outcome returned for a duplicate delivery or opaque status lookup. Result
// contains the exact reply Activity bytes only for a successful direct-agent delegation.
type Record struct {
	Job
	State       State
	StatusToken string
	Location    string
	Result      []byte
	BodyHash    []byte
	Updated     time.Time
}

// Completion is one immutable terminal result. Location is the reply Activity IRI while Result is
// its exact serialized representation; group and content-free outcomes leave both empty.
type Completion struct {
	State    State
	Location string
	Result   []byte
}

// Store is the durable activity ledger used by the inbox and its single background processor.
type Store interface {
	Enqueue(ctx context.Context, job Job) (record Record, inserted bool, err error)
	Ignore(ctx context.Context, job Job) (record Record, inserted bool, err error)
	Claim(ctx context.Context) (job Job, claimed bool, err error)
	Complete(ctx context.Context, activityID string, completion Completion) error
	LookupStatus(ctx context.Context, token string) (Record, error)
	LookupResult(ctx context.Context, location string) (Record, error)
	Prune(ctx context.Context) error
	FailRunning(ctx context.Context) error
	Close() error
}

// ValidateJob checks the content-free fields before a store accepts untrusted inbox work.
func ValidateJob(job Job) error {
	if job.ActivityID == "" || len(job.ActivityID) > 4096 {
		return errors.New("activity state: activity ID must contain 1..4096 bytes")
	}
	if job.Route != RouteAgent && job.Route != RouteGroup {
		return fmt.Errorf("activity state: unsupported route %q", job.Route)
	}
	if job.Target == "" || job.ActorURI == "" || len(job.Body) == 0 {
		return errors.New("activity state: target, actor URI, and body are required")
	}
	if len(job.Target) > 256 || len(job.ActorURI) > 4096 || len(job.Body) > MaxBodyBytes {
		return errors.New("activity state: target, actor URI, or body exceeds its storage boundary")
	}
	return nil
}

func validateCompletion(completion Completion) error {
	if !terminal(completion.State) {
		return fmt.Errorf("activity state: %q is not terminal", completion.State)
	}
	if len(completion.Location) > 4096 || len(completion.Result) > MaxResultBytes {
		return errors.New("activity state: completion exceeds its storage boundary")
	}
	if completion.State != StateSucceeded && (completion.Location != "" || len(completion.Result) != 0) {
		return errors.New("activity state: only a successful outcome may carry a result")
	}
	return nil
}

func terminal(state State) bool {
	switch state {
	case StateSucceeded, StateDenied, StateFailed, StateIgnored:
		return true
	default:
		return false
	}
}

// Memory is the development and test implementation. Reusing one Memory across Gateway instances
// models a durable restart without weakening the production Postgres requirement.
type Memory struct {
	mu        sync.Mutex
	records   map[string]Record
	tokens    map[string]string
	order     []string
	retention time.Duration
	capacity  int
	now       func() time.Time
}

// NewMemory returns an in-memory activity ledger with bounded terminal retention and pending work.
func NewMemory(retention time.Duration, capacity int) *Memory {
	return newMemory(retention, capacity, func() time.Time { return time.Now().UTC() })
}

func newMemory(retention time.Duration, capacity int, now func() time.Time) *Memory {
	return &Memory{
		records:   make(map[string]Record),
		tokens:    make(map[string]string),
		retention: retention,
		capacity:  capacity,
		now:       now,
	}
}

// Enqueue atomically inserts a unique pending activity or returns its cached record.
func (m *Memory) Enqueue(_ context.Context, job Job) (Record, bool, error) {
	return m.insert(job, StatePending)
}

// Ignore atomically inserts a unique activity directly as terminal ignored work. It must never be
// visible to Claim, closing the race between mention validation and asynchronous processing.
func (m *Memory) Ignore(_ context.Context, job Job) (Record, bool, error) {
	return m.insert(job, StateIgnored)
}

func (m *Memory) insert(job Job, initial State) (Record, bool, error) {
	if err := ValidateJob(job); err != nil {
		return Record{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	if record, ok := m.records[job.ActivityID]; ok {
		if !sameRecordJob(record, job) {
			return Record{}, false, ErrConflict
		}
		return cloneRecord(record), false, nil
	}
	if initial == StatePending && m.pendingLocked() >= m.capacity {
		return Record{}, false, ErrCapacity
	}
	token, err := newStatusToken()
	if err != nil {
		return Record{}, false, err
	}
	now := m.now()
	record := Record{
		Job: cloneJob(job), State: initial, StatusToken: token,
		BodyHash: bodyHash(job.Body), Updated: now,
	}
	if terminal(initial) {
		record.Body = nil
	}
	m.records[job.ActivityID] = record
	m.tokens[token] = job.ActivityID
	m.order = append(m.order, job.ActivityID)
	return cloneRecord(record), true, nil
}

// Claim atomically changes the oldest pending activity to running.
func (m *Memory) Claim(context.Context) (Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		record, ok := m.records[id]
		if !ok || record.State != StatePending {
			continue
		}
		record.State = StateRunning
		record.Updated = m.now()
		m.records[id] = record
		return cloneJob(record.Job), true, nil
	}
	return Job{}, false, nil
}

// Complete records one terminal outcome. Running work is never retried after a crash, preserving
// the at-most-one processing-pass invariant.
func (m *Memory) Complete(_ context.Context, activityID string, completion Completion) error {
	if err := validateCompletion(completion); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[activityID]
	if !ok {
		return fmt.Errorf("activity state: unknown activity %q", activityID)
	}
	if record.State != StateRunning && record.State != StatePending {
		return fmt.Errorf("activity state: activity %q is already %s", activityID, record.State)
	}
	record.State = completion.State
	record.Location = completion.Location
	record.Result = append([]byte(nil), completion.Result...)
	record.Body = nil
	record.Updated = m.now()
	m.records[activityID] = record
	return nil
}

// LookupStatus resolves an unguessable status capability without exposing activity IDs or actors.
func (m *Memory) LookupStatus(_ context.Context, token string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.tokens[token]
	if !ok {
		return Record{}, ErrNotFound
	}
	record, ok := m.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return cloneRecord(record), nil
}

// LookupResult resolves a successful reply's canonical public Activity IRI.
func (m *Memory) LookupResult(_ context.Context, location string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, record := range m.records {
		if record.Location == location && len(record.Result) > 0 {
			return cloneRecord(record), nil
		}
	}
	return Record{}, ErrNotFound
}

// Prune removes terminal outcomes whose bounded retention has elapsed.
func (m *Memory) Prune(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	return nil
}

// FailRunning terminalizes work whose process ended at an unknown point. Retrying it could repeat
// an already-started LLM invocation, so cost safety takes precedence over an automatic replay.
func (m *Memory) FailRunning(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, record := range m.records {
		if record.State == StateRunning {
			record.State = StateFailed
			record.Body = nil
			record.Updated = m.now()
			m.records[id] = record
		}
	}
	return nil
}

// Close implements Store.
func (*Memory) Close() error { return nil }

func (m *Memory) pendingLocked() int {
	count := 0
	for _, record := range m.records {
		if record.State == StatePending || record.State == StateRunning {
			count++
		}
	}
	return count
}

func (m *Memory) pruneLocked() {
	if m.retention <= 0 {
		return
	}
	cutoff := m.now().Add(-m.retention)
	kept := m.order[:0]
	for _, id := range m.order {
		record, ok := m.records[id]
		if !ok {
			continue
		}
		if terminal(record.State) && record.Updated.Before(cutoff) {
			delete(m.tokens, record.StatusToken)
			delete(m.records, id)
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
}

func newStatusToken() (string, error) {
	raw := make([]byte, statusTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("activity state: generate status token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func cloneJob(job Job) Job {
	job.Body = append([]byte(nil), job.Body...)
	return job
}

func cloneRecord(record Record) Record {
	record.Job = cloneJob(record.Job)
	record.Result = append([]byte(nil), record.Result...)
	record.BodyHash = append([]byte(nil), record.BodyHash...)
	return record
}

func sameRecordJob(left Record, right Job) bool {
	if left.ActivityID != right.ActivityID || left.Route != right.Route ||
		left.Target != right.Target || left.ActorURI != right.ActorURI {
		return false
	}
	if len(left.Body) > 0 {
		return bytes.Equal(left.Body, right.Body)
	}
	return bytes.Equal(left.BodyHash, bodyHash(right.Body))
}

func bodyHash(body []byte) []byte {
	digest := sha256.Sum256(body)
	return digest[:]
}
