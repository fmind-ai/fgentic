package bridge

import (
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// openTask is a delegation paused in TASK_STATE_INPUT_REQUIRED, waiting for the original sender to
// answer the agent's question in the placeholder thread (#116). The bridge releases the dispatcher
// worker while waiting — so a paused task never freezes a room's FIFO — and resumes it when the
// answer arrives, so this state must outlive the worker. It is keyed by the placeholder event ID,
// which is the thread root the human replies under and is authored by exactly one ghost.
type openTask struct {
	origin      *event.Event   // the original delegating event (audit identity, room, placeholder root)
	placeholder id.EventID     // thread root; the answering reply must relate to it
	localpart   string         // ghost that owns the paused task
	ref         *AgentRef      // bound mapping (immutable): its target and path
	taskID      string         // A2A task to resume by calling SendMessage with this task ID
	contextID   string         // A2A context threaded across the pause
	sender      senderIdentity // classified original sender; only sender.mxid may answer
	expiry      *time.Timer    // fires the waiting-budget drop when no reply arrives
}

// openTaskRegistry tracks input-required delegations keyed by their placeholder event ID. Its size
// is bounded by the number of concurrently paused tasks; each entry is removed the moment it is
// resumed, expired, or superseded, so it cannot grow without limit.
type openTaskRegistry struct {
	mu    sync.Mutex
	tasks map[id.EventID]*openTask
}

func newOpenTaskRegistry() *openTaskRegistry {
	return &openTaskRegistry{tasks: make(map[id.EventID]*openTask)}
}

// register stores a paused task and arms its expiry timer atomically under the lock, so the timer
// can never fire against an entry that is not yet stored (a near-zero timeout is then correctly an
// immediate expiry). A pre-existing entry for the same placeholder is superseded and its timer
// stopped, so a re-pause after a continuation round cannot leave a stale timer firing against the
// reused placeholder. onExpire runs on the timer goroutine and must itself take the lock via claim.
func (r *openTaskRegistry) register(t *openTask, timeout time.Duration, onExpire func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.tasks[t.placeholder]; ok && existing.expiry != nil {
		existing.expiry.Stop()
	}
	t.expiry = time.AfterFunc(timeout, onExpire)
	r.tasks[t.placeholder] = t
}

// claim atomically removes and returns the paused task for a placeholder, stopping its expiry timer.
// Exactly one caller — the answering reply or the expiry callback — wins the claim, so a resume and
// an expiry can never both act on the same task.
func (r *openTaskRegistry) claim(placeholder id.EventID) (*openTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[placeholder]
	if !ok {
		return nil, false
	}
	delete(r.tasks, placeholder)
	if t.expiry != nil {
		t.expiry.Stop()
	}
	return t, true
}

// owner reports the room and sender allowed to answer the paused task for a placeholder, without
// consuming it, so a wrong-room or wrong-sender reply is rejected while the pending answer slot
// stays open for the owner.
func (r *openTaskRegistry) owner(placeholder id.EventID) (id.RoomID, id.UserID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[placeholder]
	if !ok {
		return "", "", false
	}
	return t.origin.RoomID, t.sender.mxid, true
}
