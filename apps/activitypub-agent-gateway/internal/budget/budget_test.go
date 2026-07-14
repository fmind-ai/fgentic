package budget

import (
	"testing"
	"time"
)

// fixedClock is a mutable clock for deterministic window tests.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func newTestReserver(capacity int) (*Reserver, *fixedClock) {
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	return NewWithClock(time.Minute, capacity, clk.now), clk
}

func TestReserveWithinAndOverBudget(t *testing.T) {
	r, _ := newTestReserver(64)
	const actor, domain = "https://m.example/users/bob", "m.example"

	// Pool of 3000, reservation 1000 → three fit, the fourth is over budget.
	for i := 0; i < 3; i++ {
		if !r.Reserve(actor, domain, 1000, 3000, 3000) {
			t.Fatalf("reservation %d should fit", i)
		}
	}
	if r.Reserve(actor, domain, 1000, 3000, 3000) {
		t.Errorf("fourth reservation must exceed the 3000 pool")
	}
}

func TestReserveIsAtomicAcrossActorAndDomain(t *testing.T) {
	r, _ := newTestReserver(64)
	const domain = "m.example"

	// The domain pool (1000) is exhausted by actor A; actor B has its own pool but the domain is
	// full, so B is denied AND B's own pool is not debited (all-or-nothing).
	if !r.Reserve("https://m.example/users/a", domain, 1000, 5000, 1000) {
		t.Fatalf("first actor should fit the domain pool")
	}
	if r.Reserve("https://m.example/users/b", domain, 1000, 5000, 1000) {
		t.Fatalf("domain pool is exhausted; second actor must be denied")
	}
	// Prove B's actor pool was not debited: once the domain window rolls, B fits immediately.
	r2, clk := newTestReserver(64)
	if !r2.Reserve("https://m.example/users/a", domain, 1000, 5000, 1000) {
		t.Fatalf("setup reserve failed")
	}
	if r2.Reserve("https://m.example/users/b", domain, 1000, 5000, 1000) {
		t.Fatalf("domain full; B denied")
	}
	clk.t = clk.t.Add(time.Minute) // roll the window
	if !r2.Reserve("https://m.example/users/b", domain, 1000, 5000, 1000) {
		t.Errorf("after the window rolls, B must fit with a full domain pool (its own pool was untouched)")
	}
}

func TestReserveWindowResets(t *testing.T) {
	r, clk := newTestReserver(64)
	const actor, domain = "https://m.example/users/bob", "m.example"
	if !r.Reserve(actor, domain, 3000, 3000, 3000) {
		t.Fatalf("full-pool reservation should fit")
	}
	if r.Reserve(actor, domain, 1, 3000, 3000) {
		t.Fatalf("pool is exhausted this window")
	}
	clk.t = clk.t.Add(61 * time.Second)
	if !r.Reserve(actor, domain, 3000, 3000, 3000) {
		t.Errorf("a new window must restore the full pool")
	}
}

func TestReserveRejectsReservationLargerThanPool(t *testing.T) {
	r, _ := newTestReserver(64)
	if r.Reserve("https://m.example/users/bob", "m.example", 4000, 3000, 3000) {
		t.Errorf("a reservation larger than the actor pool can never fit")
	}
	if r.Reserve("https://m.example/users/bob", "m.example", 4000, 5000, 3000) {
		t.Errorf("a reservation larger than the domain pool can never fit")
	}
}

func TestReserveFailsClosedAtCapacity(t *testing.T) {
	// Capacity 2 holds exactly one actor + one domain key.
	r, _ := newTestReserver(2)
	if !r.Reserve("https://m.example/users/a", "m.example", 1, 10, 10) {
		t.Fatalf("first key pair should fit within capacity")
	}
	// A different actor needs a new actor key; the map is full, so it fails closed.
	if r.Reserve("https://m.example/users/b", "m.example", 1, 10, 10) {
		t.Errorf("a new actor beyond capacity must fail closed")
	}
	// The already-tracked actor+domain still works (no new key needed).
	if !r.Reserve("https://m.example/users/a", "m.example", 1, 10, 10) {
		t.Errorf("an existing key pair must keep working at capacity")
	}
}

func TestReserveEvictsIdleKeys(t *testing.T) {
	r, clk := newTestReserver(2)
	if !r.Reserve("https://m.example/users/a", "m.example", 1, 10, 10) {
		t.Fatalf("first reserve failed")
	}
	// After idle eviction + a sweep, the stale keys are gone and a new actor fits.
	clk.t = clk.t.Add(idleEviction + 2*sweepInterval)
	if !r.Reserve("https://n.example/users/z", "n.example", 1, 10, 10) {
		t.Errorf("idle keys must be evicted so capacity is reusable")
	}
}
