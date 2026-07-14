// Package state persists the bridge's correctness-critical state (SPEC §5): the per-(room, ghost)
// A2A contextId used for conversation threading, and a processed-event set that collapses the
// homeserver's at-least-once transaction delivery into effectively-once agent invocation.
package state

import (
	"context"
	"sync"
	"time"
)

// retention bounds the processed-event dedup window. Synapse retries a transaction until it is
// ACKed; redeliveries land within minutes, so a day is a comfortable margin.
const retention = 24 * time.Hour

// Store persists bridge state. Context loss is benign (a conversation restarts fresh);
// MarkEventProcessed loss risks duplicate agent invocations after a redelivery.
type Store interface {
	// Context returns the A2A contextId for a (room, ghost) thread, or "" for a fresh one.
	Context(ctx context.Context, roomID, ghost string) (string, error)
	// SetContext records the contextId returned by the agent for the next turn of the thread.
	SetContext(ctx context.Context, roomID, ghost, contextID string) error
	// MarkEventProcessed records an event ID and reports whether this was its first sighting.
	MarkEventProcessed(ctx context.Context, eventID string) (first bool, err error)
	// Close releases the underlying resources.
	Close() error
}

// Memory is the in-memory fallback used when no DATABASE_URL is configured (dev only).
type Memory struct {
	mu        sync.Mutex
	contexts  map[[2]string]string
	processed map[string]time.Time
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{
		contexts:  make(map[[2]string]string),
		processed: make(map[string]time.Time),
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

// Close implements Store (no resources to release).
func (m *Memory) Close() error { return nil }
