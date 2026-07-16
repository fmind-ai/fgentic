// Package state persists the bridge's correctness-critical state (SPEC §5): the per-(room, ghost)
// A2A contextId used for conversation threading, and a processed-event set that collapses the
// homeserver's at-least-once transaction delivery into effectively-once agent invocation.
package state

import (
	"context"
	"sync"
	"time"
)

// retention bounds the legacy processed-event dedup window. New durable jobs use
// TerminalRetention and preserve ambiguous/dead evidence indefinitely.
const retention = TerminalRetention

// Ledger is the crash-safe delegation contract used by the appservice intake and lease workers.
type Ledger interface {
	AdmitTransaction(context.Context, TransactionAdmission) (AdmissionResult, error)
	NonTerminalCount(context.Context) (int, error)
	Claim(context.Context, ClaimRequest) (Job, bool, error)
	Heartbeat(context.Context, LeaseToken, time.Time, time.Duration) error
	RecordAdmission(context.Context, AdmissionRequest) error
	RecordMatrixEvent(context.Context, MatrixEventRequest) error
	RecordDeadMan(context.Context, DeadManRequest) error
	Transition(context.Context, TransitionRequest) error
	ScheduleRetry(context.Context, RetryRequest) error
	Job(context.Context, string) (Job, bool, error)
	CleanupTerminal(context.Context, time.Time) (CleanupResult, error)
}

// Store persists bridge state. Context loss is benign (a conversation restarts fresh);
// MarkEventProcessed loss risks duplicate agent invocations after a redelivery.
type Store interface {
	Ledger
	// Context returns the A2A contextId for a (room, ghost) thread, or "" for a fresh one.
	Context(ctx context.Context, roomID, ghost string) (string, error)
	// SetContext records the contextId returned by the agent for the next turn of the thread.
	SetContext(ctx context.Context, roomID, ghost, contextID string) error
	// MarkEventProcessed records an event ID and reports whether this was its first sighting.
	MarkEventProcessed(ctx context.Context, eventID string) (first bool, err error)
	// MarkRoomWelcomed records the bridge's one welcome attempt for a room and reports whether
	// this caller owns that attempt. Unlike processed events, room markers are never pruned.
	MarkRoomWelcomed(ctx context.Context, roomID string) (first bool, err error)
	// Close releases the underlying resources.
	Close() error
}

// Memory is the in-memory fallback used when no DATABASE_URL is configured (dev only).
type Memory struct {
	mu           sync.Mutex
	contexts     map[[2]string]string
	processed    map[string]time.Time
	welcomeRooms map[string]struct{}
	transactions map[string]memoryTransaction
	jobs         map[string]Job
	jobOrder     []string
	jobByTarget  map[[2]string]string
	nextSequence int64
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{
		contexts:     make(map[[2]string]string),
		processed:    make(map[string]time.Time),
		welcomeRooms: make(map[string]struct{}),
		transactions: make(map[string]memoryTransaction),
		jobs:         make(map[string]Job),
		jobByTarget:  make(map[[2]string]string),
	}
}

// Context implements Store.
func (m *Memory) Context(_ context.Context, roomID, ghost string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.contexts[[2]string{roomID, ghost}], nil
}

// SetContext implements Store.
func (m *Memory) SetContext(_ context.Context, roomID, ghost, contextID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contexts[[2]string{roomID, ghost}] = contextID
	return nil
}

// MarkEventProcessed implements Store.
func (m *Memory) MarkEventProcessed(_ context.Context, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, at := range m.processed {
		if now.Sub(at) > retention {
			delete(m.processed, id)
		}
	}
	if _, seen := m.processed[eventID]; seen {
		return false, nil
	}
	m.processed[eventID] = now
	return true, nil
}

// MarkRoomWelcomed implements Store.
func (m *Memory) MarkRoomWelcomed(_ context.Context, roomID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, seen := m.welcomeRooms[roomID]; seen {
		return false, nil
	}
	m.welcomeRooms[roomID] = struct{}{}
	return true, nil
}

// Close implements Store (no resources to release).
func (m *Memory) Close() error { return nil }
