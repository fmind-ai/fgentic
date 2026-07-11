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
	sem            chan struct{} // global concurrency permits
	roomCapacity   int
	globalCapacity int

	mu          sync.Mutex
	rooms       map[id.RoomID][]queuedJob
	roomPending map[id.RoomID]int
	pending     int
	wg          sync.WaitGroup
}

type queuedJob struct {
	run    func(context.Context)
	onDrop func()
}

type enqueueResult uint8

const (
	enqueueAccepted enqueueResult = iota
	enqueueStopped
	enqueueRoomFull
	enqueueGlobalFull
)

func (r enqueueResult) terminalReason() string {
	switch r {
	case enqueueRoomFull:
		return "queue_room_capacity_rejected"
	case enqueueGlobalFull:
		return "queue_global_capacity_rejected"
	default:
		return ""
	}
}

func newDispatcher(concurrency, roomCapacity, globalCapacity int) *dispatcher {
	return &dispatcher{
		sem:            make(chan struct{}, concurrency),
		roomCapacity:   roomCapacity,
		globalCapacity: globalCapacity,
		rooms:          make(map[id.RoomID][]queuedJob),
		roomPending:    make(map[id.RoomID]int),
	}
}

// Enqueue appends a job to the room's queue and starts a drainer for the room if none runs.
// ctx is the process lifetime context: jobs observe its cancellation, and Enqueue drops jobs
// once it is done (shutdown).
func (d *dispatcher) Enqueue(
	ctx context.Context,
	roomID id.RoomID,
	run func(context.Context),
	onDrop func(),
) enqueueResult {
	if ctx.Err() != nil {
		return enqueueStopped
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return enqueueStopped
	}
	if d.roomPending[roomID] >= d.roomCapacity {
		return enqueueRoomFull
	}
	if d.pending >= d.globalCapacity {
		return enqueueGlobalFull
	}
	pending, draining := d.rooms[roomID]
	job := queuedJob{run: run, onDrop: onDrop}
	d.rooms[roomID] = append(pending, job)
	d.roomPending[roomID]++
	d.pending++
	queueDepth.Inc()
	if !draining {
		// The room key existing (even with an empty slice) marks an active drainer.
		d.wg.Add(1)
		go d.drain(ctx, roomID)
	}
	return enqueueAccepted
}

// drain runs the room's jobs in FIFO order, one at a time, each under a global permit.
func (d *dispatcher) drain(ctx context.Context, roomID id.RoomID) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		if ctx.Err() != nil {
			onDrop := d.dropRoomLocked(roomID, nil)
			d.mu.Unlock()
			runDropCallbacks(onDrop)
			return
		}
		pending := d.rooms[roomID]
		if len(pending) == 0 {
			delete(d.rooms, roomID)
			d.mu.Unlock()
			return
		}
		job := pending[0]
		d.rooms[roomID] = pending[1:]
		queueDepth.Dec()
		d.mu.Unlock()

		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			d.mu.Lock()
			onDrop := d.dropRoomLocked(roomID, &job)
			d.mu.Unlock()
			runDropCallbacks(onDrop)
			return
		}
		job.run(ctx)
		<-d.sem
		d.complete(roomID)
	}
}

func (d *dispatcher) complete(roomID id.RoomID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending--
	d.roomPending[roomID]--
	if d.roomPending[roomID] == 0 {
		delete(d.roomPending, roomID)
	}
}

// dropRoomLocked removes queued jobs plus an optional job already popped by the drainer and
// returns their callbacks. The caller holds mu; callbacks run only after unlocking.
func (d *dispatcher) dropRoomLocked(roomID id.RoomID, extra *queuedJob) []func() {
	queuedJobs := d.rooms[roomID]
	queued := len(queuedJobs)
	dropped := queued
	callbacks := make([]func(), 0, queued+1)
	if extra != nil {
		dropped++
		if extra.onDrop != nil {
			callbacks = append(callbacks, extra.onDrop)
		}
	}
	for _, job := range queuedJobs {
		if job.onDrop != nil {
			callbacks = append(callbacks, job.onDrop)
		}
	}
	delete(d.rooms, roomID)
	delete(d.roomPending, roomID)
	d.pending -= dropped
	queueDepth.Sub(float64(queued))
	return callbacks
}

func runDropCallbacks(callbacks []func()) {
	for _, callback := range callbacks {
		callback()
	}
}

// Wait blocks until all queued jobs have finished (used for graceful shutdown).
func (d *dispatcher) Wait() {
	d.wg.Wait()
}
