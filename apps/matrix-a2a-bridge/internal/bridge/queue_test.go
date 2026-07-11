package bridge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"maunium.net/go/mautrix/id"
)

// Per-room FIFO ordering must hold even with a large concurrency cap (SPEC §4 F3).
func TestDispatcherPerRoomOrdering(t *testing.T) {
	d := newDispatcher(8, 128, 128)
	ctx := t.Context()

	var mu sync.Mutex
	got := make([]int, 0, 100)
	for i := range 100 {
		d.Enqueue(ctx, "!room:x", func(context.Context) {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		}, nil)
	}
	d.Wait()
	for i, v := range got {
		if v != i {
			t.Fatalf("room jobs ran out of order: got[%d] = %d", i, v)
		}
	}
	if len(got) != 100 {
		t.Fatalf("ran %d jobs, want 100", len(got))
	}
}

// The global concurrency cap bounds in-flight jobs across rooms.
func TestDispatcherConcurrencyCap(t *testing.T) {
	const limit = 3
	d := newDispatcher(limit, 20, 20)
	ctx := t.Context()

	var inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		room := id.RoomID(string(rune('a'+i)) + ":x") // distinct rooms → eligible to run in parallel
		d.Enqueue(ctx, room, func(context.Context) {
			defer wg.Done()
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
		}, nil)
	}
	wg.Wait()
	d.Wait()
	if p := peak.Load(); p > limit {
		t.Fatalf("peak in-flight = %d, want <= %d", p, limit)
	}
}

// Enqueue after shutdown must drop jobs instead of leaking goroutines.
func TestDispatcherDropsAfterCancel(t *testing.T) {
	d := newDispatcher(1, 10, 10)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ran := false
	if got := d.Enqueue(ctx, "!room:x", func(context.Context) { ran = true }, nil); got != enqueueStopped {
		t.Fatalf("Enqueue after cancel = %v, want stopped", got)
	}
	d.Wait()
	if ran {
		t.Fatal("job ran despite cancelled context")
	}
}

func TestDispatcherRejectsPerRoomOverflow(t *testing.T) {
	d := newDispatcher(1, 2, 10)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		d.Wait()
	})
	if got := d.Enqueue(t.Context(), "!room:x", func(context.Context) {
		close(started)
		<-release
	}, nil); got != enqueueAccepted {
		t.Fatalf("first Enqueue = %v, want accepted", got)
	}
	<-started
	secondRan := false
	if got := d.Enqueue(t.Context(), "!room:x", func(context.Context) { secondRan = true }, nil); got != enqueueAccepted {
		t.Fatalf("second Enqueue = %v, want accepted", got)
	}
	if got := d.Enqueue(t.Context(), "!room:x", func(context.Context) {}, nil); got != enqueueRoomFull {
		t.Fatalf("overflow Enqueue = %v, want room full", got)
	}
	unblock()
	d.Wait()
	if !secondRan {
		t.Fatal("accepted per-room job did not run")
	}
}

func TestDispatcherRejectsGlobalOverflowAcrossRooms(t *testing.T) {
	d := newDispatcher(1, 10, 2)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		d.Wait()
	})
	if got := d.Enqueue(t.Context(), "!first:x", func(context.Context) {
		close(started)
		<-release
	}, nil); got != enqueueAccepted {
		t.Fatalf("first Enqueue = %v, want accepted", got)
	}
	<-started
	secondRan := false
	if got := d.Enqueue(t.Context(), "!second:x", func(context.Context) { secondRan = true }, nil); got != enqueueAccepted {
		t.Fatalf("second Enqueue = %v, want accepted", got)
	}
	if got := d.Enqueue(t.Context(), "!third:x", func(context.Context) {}, nil); got != enqueueGlobalFull {
		t.Fatalf("overflow Enqueue = %v, want global full", got)
	}
	unblock()
	d.Wait()
	if !secondRan {
		t.Fatal("accepted cross-room job did not run")
	}
}

func TestDispatcherCancellationDropsQueuedJobsAndReleasesCapacity(t *testing.T) {
	d := newDispatcher(1, 2, 2)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	if got := d.Enqueue(ctx, "!room:x", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}, nil); got != enqueueAccepted {
		t.Fatalf("running Enqueue = %v, want accepted", got)
	}
	<-started
	queuedRan := false
	var dropCount atomic.Int32
	if got := d.Enqueue(
		ctx,
		"!room:x",
		func(context.Context) { queuedRan = true },
		func() { dropCount.Add(1) },
	); got != enqueueAccepted {
		t.Fatalf("queued Enqueue = %v, want accepted", got)
	}

	cancel()
	d.Wait()

	if queuedRan {
		t.Fatal("queued job ran after shutdown cancellation")
	}
	if got := dropCount.Load(); got != 1 {
		t.Fatalf("queued job onDrop calls = %d, want exactly 1", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending != 0 || len(d.roomPending) != 0 || len(d.rooms) != 0 {
		t.Fatalf(
			"dispatcher retained state after cancellation: pending=%d rooms=%d roomPending=%d",
			d.pending,
			len(d.rooms),
			len(d.roomPending),
		)
	}
}

func TestDispatcherQueueDepth(t *testing.T) {
	if got := queueDepthValue(t); got != 0 {
		t.Fatalf("initial queue depth = %v, want 0", got)
	}

	d := newDispatcher(2, 10, 10)
	ctx := t.Context()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		d.Wait()
		queueDepth.Set(0)
	})

	for _, roomID := range []id.RoomID{"!first:x", "!second:x"} {
		d.Enqueue(ctx, roomID, func(context.Context) {
			started <- struct{}{}
			<-release
		}, nil)
	}
	for range 2 {
		<-started
	}

	// Both room drainers are occupied, so these jobs remain queued independently.
	d.Enqueue(ctx, "!first:x", func(context.Context) {}, nil)
	if got := queueDepthValue(t); got != 1 {
		t.Fatalf("queue depth after first enqueue = %v, want 1", got)
	}
	d.Enqueue(ctx, "!second:x", func(context.Context) {}, nil)
	if got := queueDepthValue(t); got != 2 {
		t.Fatalf("aggregate queue depth = %v, want 2", got)
	}

	unblock()
	d.Wait()
	if got := queueDepthValue(t); got != 0 {
		t.Fatalf("queue depth after drain = %v, want 0", got)
	}
}

func queueDepthValue(t *testing.T) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "fgentic_queue_depth" {
			continue
		}
		if got := len(family.GetMetric()); got != 1 {
			t.Fatalf("queue depth metric series = %d, want 1", got)
		}
		metric := family.GetMetric()[0]
		if got := len(metric.GetLabel()); got != 0 {
			t.Fatalf("queue depth metric labels = %d, want 0", got)
		}
		if metric.GetGauge() == nil {
			t.Fatal("queue depth metric is not a gauge")
		}
		return metric.GetGauge().GetValue()
	}

	t.Fatal("fgentic_queue_depth metric not found")
	return 0
}
