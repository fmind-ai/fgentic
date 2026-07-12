package apgateway

import (
	"encoding/json"
	"sync"

	vocab "github.com/go-ap/activitypub"
)

// storedActivity is a published reply as served bytes. Storing the marshaled (and, when signing is
// enabled, FEP-8b32-signed) JSON verbatim means the exact octets that were signed are the octets a
// remote verifier dereferences — re-marshaling could perturb them and break the object proof.
type storedActivity struct {
	id  vocab.IRI
	raw json.RawMessage
}

// outboxStore is the gateway's in-memory, per-ghost record of published reply activities, plus an
// id index for dereferencing a single activity. It is deliberately ephemeral for this scaffold:
// durable, crash-safe delegation state is landed separately by the proactive-agents work (bridge
// Postgres, issue #237). Newest-first ordering follows the ActivityPub outbox convention.
type outboxStore struct {
	mu    sync.Mutex
	seq   uint64
	byID  map[string][]storedActivity
	byIRI map[vocab.IRI]json.RawMessage
}

func newOutboxStore() *outboxStore {
	return &outboxStore{
		byID:  make(map[string][]storedActivity),
		byIRI: make(map[vocab.IRI]json.RawMessage),
	}
}

// next returns a strictly increasing sequence number for minting object and activity IRIs.
func (s *outboxStore) next() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

// append records a published activity for a ghost, indexed by its IRI for dereferencing.
func (s *outboxStore) append(ghost string, id vocab.IRI, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[ghost] = append(s.byID[ghost], storedActivity{id: id, raw: raw})
	s.byIRI[id] = raw
}

// items returns a ghost's published activities newest-first.
func (s *outboxStore) items(ghost string) []storedActivity {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.byID[ghost]
	out := make([]storedActivity, len(stored))
	for i, activity := range stored {
		out[len(stored)-1-i] = activity
	}
	return out
}

// lookup returns the raw bytes of a single published activity by its IRI.
func (s *outboxStore) lookup(id vocab.IRI) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.byIRI[id]
	return raw, ok
}
