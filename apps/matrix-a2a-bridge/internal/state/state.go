// Package state persists the bridge's correctness-critical state (SPEC §5): the per-(room, ghost)
// A2A contextId used for conversation threading, and a processed-event set that collapses the
// homeserver's at-least-once transaction delivery into effectively-once agent invocation.
package state

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// retention bounds the legacy processed-event dedup window. New durable jobs use
// TerminalRetention and preserve ambiguous/dead evidence indefinitely.
const retention = TerminalRetention

// MaxConversationOwners bounds per-context identity metadata and deletion fan-out.
const MaxConversationOwners = 256

var (
	// ErrConversationChanged reports that an observed context is no longer current.
	ErrConversationChanged = errors.New("conversation context changed")
	// ErrConversationOwnerLimit prevents unbounded per-context identity accumulation.
	ErrConversationOwnerLimit = errors.New("conversation owner limit reached")
)

// Conversation is the bridge-owned handle for one backend conversation. When OwnersComplete is
// true, Owners is the complete set of Matrix identities under which kagent has persisted this
// context since its last reset. Pre-governance rows remain incomplete and cannot be purged safely.
type Conversation struct {
	RoomID         string
	Ghost          string
	ContextID      string
	Owners         []string
	OwnersComplete bool
	UpdatedAt      time.Time
}

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

// ControlLedger is the crash-safe interactive-control outbox tied to durable delegation leases.
type ControlLedger interface {
	ControlTarget(context.Context, string) (ControlTarget, bool, error)
	ClaimControl(context.Context, LeaseToken, time.Time) (Control, bool, error)
	PlanControl(context.Context, PlanControlRequest) (Control, error)
	TransitionControl(context.Context, ControlTransitionRequest) error
	Controls(context.Context, string) ([]Control, error)
}

// Store persists bridge state. Context loss is benign (a conversation restarts fresh);
// MarkEventProcessed loss risks duplicate agent invocations after a redelivery.
type Store interface {
	Ledger
	ControlLedger
	// Context returns the A2A contextId for a (room, ghost) thread, or "" for a fresh one.
	Context(ctx context.Context, roomID, ghost string) (string, error)
	// Conversation returns the purge metadata for a (room, ghost) thread.
	Conversation(ctx context.Context, roomID, ghost string) (Conversation, bool, error)
	// AddContextOwner records an identity before reusing an existing backend context.
	AddContextOwner(ctx context.Context, roomID, ghost, contextID, owner string) error
	// SetContext records the contextId returned by the agent and the identity that invoked it.
	SetContext(ctx context.Context, roomID, ghost, contextID, owner string) error
	// ConversationsBefore returns a bounded oldest-first retention batch for one agent mapping.
	ConversationsBefore(ctx context.Context, ghost string, cutoff time.Time, limit int) ([]Conversation, error)
	// DeleteConversation removes metadata only if the exact observed record is still current.
	DeleteConversation(ctx context.Context, conversation Conversation) (bool, error)
	// ConversationBusy reports whether durable work can still read or mutate the backend session.
	ConversationBusy(ctx context.Context, roomID, ghost string) (bool, error)
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
	mu              sync.Mutex
	contexts        map[[2]string]Conversation
	processed       map[string]time.Time
	welcomeRooms    map[string]struct{}
	transactions    map[string]memoryTransaction
	jobs            map[string]Job
	jobOrder        []string
	jobByTarget     map[[2]string]string
	controls        map[string]Control
	controlOrder    []string
	controlBySource map[[3]string]string
	nextSequence    int64
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{
		contexts:        make(map[[2]string]Conversation),
		processed:       make(map[string]time.Time),
		welcomeRooms:    make(map[string]struct{}),
		transactions:    make(map[string]memoryTransaction),
		jobs:            make(map[string]Job),
		jobByTarget:     make(map[[2]string]string),
		controls:        make(map[string]Control),
		controlBySource: make(map[[3]string]string),
	}
}

// Context implements Store.
func (m *Memory) Context(_ context.Context, roomID, ghost string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.contexts[[2]string{roomID, ghost}].ContextID, nil
}

// Conversation implements Store.
func (m *Memory) Conversation(_ context.Context, roomID, ghost string) (Conversation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conversation, ok := m.contexts[[2]string{roomID, ghost}]
	conversation.Owners = slices.Clone(conversation.Owners)
	return conversation, ok, nil
}

// AddContextOwner implements Store.
func (m *Memory) AddContextOwner(_ context.Context, roomID, ghost, contextID, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]string{roomID, ghost}
	conversation, ok := m.contexts[key]
	if !ok || conversation.ContextID != contextID {
		return ErrConversationChanged
	}
	if slices.Contains(conversation.Owners, owner) {
		return nil
	}
	if len(conversation.Owners) >= MaxConversationOwners {
		return ErrConversationOwnerLimit
	}
	conversation.Owners = append(conversation.Owners, owner)
	conversation.UpdatedAt = time.Now().UTC()
	m.contexts[key] = conversation
	return nil
}

// SetContext implements Store.
func (m *Memory) SetContext(_ context.Context, roomID, ghost, contextID, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]string{roomID, ghost}
	conversation := m.contexts[key]
	owners := []string{owner}
	if conversation.ContextID == contextID {
		owners = slices.Clone(conversation.Owners)
		if !slices.Contains(owners, owner) {
			if len(owners) >= MaxConversationOwners {
				return ErrConversationOwnerLimit
			}
			owners = append(owners, owner)
		}
	}
	m.contexts[key] = Conversation{
		RoomID: roomID, Ghost: ghost, ContextID: contextID, Owners: owners,
		OwnersComplete: true, UpdatedAt: time.Now().UTC(),
	}
	return nil
}

// ConversationsBefore implements Store.
func (m *Memory) ConversationsBefore(_ context.Context, ghost string, cutoff time.Time, limit int) ([]Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conversations := make([]Conversation, 0, limit)
	for _, conversation := range m.contexts {
		if conversation.Ghost != ghost || !conversation.UpdatedAt.Before(cutoff) {
			continue
		}
		conversation.Owners = slices.Clone(conversation.Owners)
		conversations = append(conversations, conversation)
	}
	slices.SortFunc(conversations, func(a, b Conversation) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	if len(conversations) > limit {
		conversations = conversations[:limit]
	}
	return conversations, nil
}

// DeleteConversation implements Store.
func (m *Memory) DeleteConversation(_ context.Context, observed Conversation) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]string{observed.RoomID, observed.Ghost}
	current, ok := m.contexts[key]
	if !ok || current.ContextID != observed.ContextID || !current.UpdatedAt.Equal(observed.UpdatedAt) {
		return false, nil
	}
	delete(m.contexts, key)
	return true, nil
}

// ConversationBusy implements Store.
func (m *Memory) ConversationBusy(_ context.Context, roomID, ghost string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job.RoomID == roomID && job.GhostLocalpart == ghost && !job.State.Terminal() {
			return true, nil
		}
	}
	return false, nil
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
