package bridge

import (
	"context"
	"sync"

	"maunium.net/go/mautrix/id"
)

// dispatcher runs delegation jobs with per-room FIFO ordering and a bounded global concurrency
// cap (SPEC §4 F3): the appservice event handler only enqueues, so the homeserver's transaction
// push is never blocked, goroutines are bounded, and one slow agent cannot starve other rooms.
type dispatcher struct {
	sem chan struct{} // global concurrency permits

	mu    sync.Mutex
	rooms map[id.RoomID][]func(context.Context)
	wg    sync.WaitGroup
}

func newDispatcher(concurrency int) *dispatcher {
	return &dispatcher{
		sem:   make(chan struct{}, concurrency),
		rooms: make(map[id.RoomID][]func(context.Context)),
	}
}

// Enqueue appends a job to the room's queue and starts a drainer for the room if none runs.
// ctx is the process lifetime context: jobs observe its cancellation, and Enqueue drops jobs
// once it is done (shutdown).
func (d *dispatcher) Enqueue(ctx context.Context, roomID id.RoomID, job func(context.Context)) {
	if ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	pending, draining := d.rooms[roomID]
	d.rooms[roomID] = append(pending, job)
	if !draining {
		// The room key existing (even with an empty slice) marks an active drainer.
		d.wg.Add(1)
		go d.drain(ctx, roomID)
	}
	d.mu.Unlock()
}

// drain runs the room's jobs in FIFO order, one at a time, each under a global permit.
func (d *dispatcher) drain(ctx context.Context, roomID id.RoomID) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		pending := d.rooms[roomID]
		if len(pending) == 0 {
			delete(d.rooms, roomID)
			d.mu.Unlock()
			return
		}
		job := pending[0]
		d.rooms[roomID] = pending[1:]
		d.mu.Unlock()

		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		job(ctx)
		<-d.sem
	}
}

// Wait blocks until all queued jobs have finished (used for graceful shutdown).
func (d *dispatcher) Wait() {
	d.wg.Wait()
}
