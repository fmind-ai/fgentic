package bridge

import (
	"context"
	"sync"

	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
)

// inflightTask is a long-running delegation that a room member can cancel by reacting to its
// placeholder message (#98). It is registered when tasks/get polling starts and removed when the
// task reaches any terminal state (completion, failure, timeout, cancel, or shutdown). The poll
// runs on a worker goroutine while cancellation arrives on the event-processor goroutine, so every
// mutable field is guarded by mu.
type inflightTask struct {
	room           id.RoomID
	placeholder    id.EventID
	taskID         string
	originalSender id.UserID
	target         a2aclient.Target
	cancelPoll     context.CancelFunc

	mu         sync.Mutex
	canceledBy id.UserID // set once when an authorized member cancels; empty while running
}

// requestCancel records the canceling member and interrupts the poll, exactly once. It reports
// whether this call was the one that triggered cancellation, so a duplicate ❌ reaction does not
// re-cancel or re-audit an already-canceled task.
func (t *inflightTask) requestCancel(by id.UserID) bool {
	t.mu.Lock()
	if t.canceledBy != "" {
		t.mu.Unlock()
		return false
	}
	t.canceledBy = by
	t.mu.Unlock()
	t.cancelPoll()
	return true
}

// canceler returns the member who canceled the task, or empty while it is still running.
func (t *inflightTask) canceler() id.UserID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.canceledBy
}

// inflightRegistry tracks cancelable long tasks keyed by their placeholder event ID. A placeholder
// is authored by exactly one ghost for exactly one delegation, so the key is unique per task. Its
// size is naturally bounded by the dispatcher's global concurrency cap: only running tasks appear.
type inflightRegistry struct {
	mu    sync.Mutex
	tasks map[id.EventID]*inflightTask
}

func newInflightRegistry() *inflightRegistry {
	return &inflightRegistry{tasks: make(map[id.EventID]*inflightTask)}
}

func (r *inflightRegistry) register(t *inflightTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[t.placeholder] = t
}

func (r *inflightRegistry) unregister(placeholder id.EventID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, placeholder)
}

func (r *inflightRegistry) lookup(placeholder id.EventID) (*inflightTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[placeholder]
	return t, ok
}
