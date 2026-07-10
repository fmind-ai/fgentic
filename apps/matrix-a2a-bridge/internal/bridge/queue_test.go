package bridge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

// Per-room FIFO ordering must hold even with a large concurrency cap (SPEC §4 F3).
func TestDispatcherPerRoomOrdering(t *testing.T) {
	d := newDispatcher(8)
	ctx := t.Context()

	var mu sync.Mutex
	got := make([]int, 0, 100)
	for i := range 100 {
		d.Enqueue(ctx, "!room:x", func(context.Context) {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
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
	d := newDispatcher(limit)
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
		})
	}
	wg.Wait()
	d.Wait()
	if p := peak.Load(); p > limit {
		t.Fatalf("peak in-flight = %d, want <= %d", p, limit)
	}
}

// Enqueue after shutdown must drop jobs instead of leaking goroutines.
func TestDispatcherDropsAfterCancel(t *testing.T) {
	d := newDispatcher(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ran := false
	d.Enqueue(ctx, "!room:x", func(context.Context) { ran = true })
	d.Wait()
	if ran {
		t.Fatal("job ran despite cancelled context")
	}
}
