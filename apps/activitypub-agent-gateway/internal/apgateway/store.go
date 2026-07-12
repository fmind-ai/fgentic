package apgateway

import (
	"sync"

	vocab "github.com/go-ap/activitypub"
)

// outboxStore is the gateway's in-memory, per-ghost record of published reply activities. It is
// deliberately ephemeral for this scaffold: durable, crash-safe delegation state is landed
// separately by the proactive-agents work (bridge Postgres, issue #237). Newest-first ordering
// follows the ActivityPub outbox convention.
type outboxStore struct {
	mu   sync.Mutex
	seq  uint64
	byID map[string][]*vocab.Create
}

func newOutboxStore() *outboxStore {
	return &outboxStore{byID: make(map[string][]*vocab.Create)}
}

// next returns a strictly increasing sequence number for minting object and activity IRIs.
func (s *outboxStore) next() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

// append records a published Create activity for a ghost.
func (s *outboxStore) append(ghost string, activity *vocab.Create) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[ghost] = append(s.byID[ghost], activity)
}

// items returns a ghost's published activities newest-first.
func (s *outboxStore) items(ghost string) []*vocab.Create {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.byID[ghost]
	out := make([]*vocab.Create, len(stored))
	for i, activity := range stored {
		out[len(stored)-1-i] = activity
	}
	return out
}
