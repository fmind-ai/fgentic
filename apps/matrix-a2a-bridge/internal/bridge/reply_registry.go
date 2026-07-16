package bridge

import (
	"sync"

	"maunium.net/go/mautrix/id"
)

// qualityReplyRegistryCapacity bounds best-effort reaction attribution without retaining reply
// content. Older replies age out in insertion order; Matrix remains the source of record.
const qualityReplyRegistryCapacity = 4096

type agentReplyRef struct {
	room  id.RoomID
	event id.EventID
	ghost string
}

// agentReplyRegistry records successfully projected terminal m.notice replies. It deliberately
// stores only operational identifiers and is process-local: quality reactions are a metric/span
// signal, not a correctness or audit ledger.
type agentReplyRegistry struct {
	mu       sync.Mutex
	capacity int
	byEvent  map[id.EventID]agentReplyRef
	order    []id.EventID
	next     int
}

func newAgentReplyRegistry(capacity int) *agentReplyRegistry {
	return &agentReplyRegistry{
		capacity: capacity,
		byEvent:  make(map[id.EventID]agentReplyRef, capacity),
		order:    make([]id.EventID, 0, capacity),
	}
}

func (r *agentReplyRegistry) record(reply agentReplyRef) {
	if reply.event == "" || reply.room == "" || reply.ghost == "" || r.capacity <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byEvent[reply.event]; exists {
		r.byEvent[reply.event] = reply
		return
	}
	if len(r.order) < r.capacity {
		r.order = append(r.order, reply.event)
	} else {
		delete(r.byEvent, r.order[r.next])
		r.order[r.next] = reply.event
		r.next = (r.next + 1) % r.capacity
	}
	r.byEvent[reply.event] = reply
}

func (r *agentReplyRegistry) lookup(eventID id.EventID, roomID id.RoomID) (agentReplyRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reply, ok := r.byEvent[eventID]
	return reply, ok && reply.room == roomID
}
