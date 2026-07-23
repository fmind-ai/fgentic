package bridge

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
)

const testRoomBudgetCapacity = 4096

func newTestRoomBudgets(defaultLimit int, period time.Duration, overrides map[string]int, clock *limiterTestClock) *roomBudgets {
	return newRoomBudgetsWithClock(defaultLimit, period, overrides, testRoomBudgetCapacity, clock.Now)
}

func TestRoomBudgetUnlimitedWhenUnconfigured(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(0, time.Hour, nil, clock)

	if rb.enabled() {
		t.Fatal("budgets reported enabled with no default and no overrides")
	}
	rb.record("!room:server", 1_000_000)
	if !rb.allow("!room:server") {
		t.Fatal("an unlimited room was refused")
	}
	if snap := rb.snapshot("!room:server"); snap.limited {
		t.Fatalf("unlimited room reported a finite budget: %+v", snap)
	}
}

func TestRoomBudgetAccumulatesAndRefusesAtCap(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(100, time.Hour, nil, clock)

	if !rb.enabled() {
		t.Fatal("a positive default budget did not enable the feature")
	}
	if !rb.allow("!room:server") {
		t.Fatal("a fresh room under budget was refused")
	}
	rb.record("!room:server", 60)
	if !rb.allow("!room:server") {
		t.Fatal("room under budget (60/100) was refused")
	}
	rb.record("!room:server", 40) // now exactly at the cap
	if rb.allow("!room:server") {
		t.Fatal("room at its cap (100/100) was still admitted; the cap must fail closed")
	}
	snap := rb.snapshot("!room:server")
	if !snap.exhausted || snap.remaining != 0 || snap.used != 100 {
		t.Fatalf("snapshot at cap = %+v, want used=100 remaining=0 exhausted", snap)
	}
	// A different room is unaffected: no cross-room leakage of state.
	if !rb.allow("!other:server") {
		t.Fatal("an unrelated room was refused by another room's exhaustion")
	}
}

func TestRoomBudgetResetsAfterPeriod(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(100, time.Hour, nil, clock)

	rb.record("!room:server", 100)
	if rb.allow("!room:server") {
		t.Fatal("room at cap was admitted before its period elapsed")
	}
	clock.Advance(time.Hour) // window fully elapsed
	if !rb.allow("!room:server") {
		t.Fatal("room budget did not reset after the period elapsed")
	}
	if snap := rb.snapshot("!room:server"); snap.used != 0 {
		t.Fatalf("post-reset used = %d, want 0", snap.used)
	}
	// Consumption resumes cleanly in the new window.
	rb.record("!room:server", 30)
	if snap := rb.snapshot("!room:server"); snap.used != 30 {
		t.Fatalf("new-window used = %d, want 30", snap.used)
	}
}

func TestRoomBudgetPerRoomOverride(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(100, time.Hour, map[string]int{
		"!vip:server":       1_000,
		"!unlimited:server": 0,
	}, clock)

	if got := rb.limitFor("!vip:server"); got != 1_000 {
		t.Fatalf("override limit = %d, want 1000", got)
	}
	if got := rb.limitFor("!plain:server"); got != 100 {
		t.Fatalf("default limit = %d, want 100", got)
	}
	// Zero override means explicitly unlimited even though a default budget exists.
	rb.record("!unlimited:server", 5_000)
	if !rb.allow("!unlimited:server") {
		t.Fatal("a room with a zero (unlimited) override was refused")
	}
	// The default-limited room still caps at 100.
	rb.record("!plain:server", 100)
	if rb.allow("!plain:server") {
		t.Fatal("a default-limited room ignored its cap")
	}
}

func TestRoomBudgetSnapshotDoesNotMutate(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(100, time.Hour, nil, clock)

	// Snapshotting an unseen room must not create a tracked window.
	snap := rb.snapshot("!room:server")
	if snap.used != 0 || snap.remaining != 100 || snap.exhausted {
		t.Fatalf("unseen snapshot = %+v, want used=0 remaining=100", snap)
	}
	if len(rb.windows) != 0 {
		t.Fatalf("snapshot created %d windows, want 0", len(rb.windows))
	}
	if want := clock.now.Add(time.Hour); !snap.resetAt.Equal(want) {
		t.Fatalf("unseen resetAt = %s, want %s", snap.resetAt, want)
	}
}

func TestRoomBudgetIgnoresNonPositiveTokens(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(100, time.Hour, nil, clock)

	rb.record("!room:server", 0)
	rb.record("!room:server", -50)
	if snap := rb.snapshot("!room:server"); snap.used != 0 {
		t.Fatalf("non-positive tokens changed usage: %+v", snap)
	}
}

func TestRoomBudgetOverflowSaturates(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(math.MaxInt, 24*time.Hour, nil, clock)

	rb.record("!room:server", math.MaxInt-10)
	rb.record("!room:server", 1_000) // would overflow without clamping
	snap := rb.snapshot("!room:server")
	if snap.used != math.MaxInt {
		t.Fatalf("used = %d, want saturated MaxInt", snap.used)
	}
	if rb.allow("!room:server") {
		t.Fatal("saturated usage must still fail closed, not wrap negative")
	}
}

func TestRoomBudgetAllowsUntrackedRoomAtCapacity(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newRoomBudgetsWithClock(100, time.Hour, nil, 1, clock.Now)

	rb.record("!first:server", 10) // fills the single map slot
	// A second, untracked room must be admitted (accounting fails open, not admission).
	if !rb.allow("!second:server") {
		t.Fatal("an untrackable room at map capacity was wrongly refused")
	}
	if _, tracked := rb.windows["!second:server"]; tracked {
		t.Fatal("the untracked room should not have displaced the capacity-bounded map")
	}
}

func TestRoomBudgetConcurrentRecordAndAllow(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	rb := newTestRoomBudgets(1_000_000, time.Hour, nil, clock)

	const goroutines = 50
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				rb.record("!room:server", 1)
				rb.allow("!room:server")
				rb.snapshot("!room:server")
			}
		}()
	}
	wg.Wait()

	if snap := rb.snapshot("!room:server"); snap.used != goroutines*perGoroutine {
		t.Fatalf("concurrent used = %d, want %d", snap.used, goroutines*perGoroutine)
	}
}

func TestRoomBudgetGateRefusesAtCapAndCountsExhaustion(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, _ := pollingHarness(t, client)
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	b.roomBudgets = newRoomBudgetsWithClock(100, time.Hour, nil, testRoomBudgetCapacity, clock.Now)

	before := counterValue(t, roomBudgetExhaustionsTotal)
	if !b.roomBudgetAllows(evt.RoomID) {
		t.Fatal("a fresh room under budget was refused by the gate")
	}
	b.recordRoomBudget(evt.RoomID, a2aclient.Result{TotalTokens: 100}) // reach the cap
	if b.roomBudgetAllows(evt.RoomID) {
		t.Fatal("a room at its cap was admitted; the gate must fail closed")
	}
	if delta := counterValue(t, roomBudgetExhaustionsTotal) - before; delta != 1 {
		t.Fatalf("exhaustion metric delta = %v, want exactly 1", delta)
	}
}

func TestRoomBudgetGateInertWhenUnconfigured(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, _ := pollingHarness(t, client)

	if b.roomBudgets.enabled() {
		t.Fatal("harness config unexpectedly enabled a room budget")
	}
	before := counterValue(t, roomBudgetExhaustionsTotal)
	b.recordRoomBudget(evt.RoomID, a2aclient.Result{TotalTokens: 10_000_000})
	if !b.roomBudgetAllows(evt.RoomID) {
		t.Fatal("an unconfigured budget refused a delegation")
	}
	if delta := counterValue(t, roomBudgetExhaustionsTotal) - before; delta != 0 {
		t.Fatalf("disabled budget touched the exhaustion metric: delta = %v", delta)
	}
}

func TestRoomBudgetIgnoresUnattributableTokens(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, _ := pollingHarness(t, client)
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	b.roomBudgets = newRoomBudgetsWithClock(100, time.Hour, nil, testRoomBudgetCapacity, clock.Now)

	// A bare terminal Message (no task usage) reports 0 tokens: it must not move the ledger, and the
	// invocation rate limits — not this budget — bound that delegation.
	b.recordRoomBudget(evt.RoomID, a2aclient.Result{TotalTokens: 0})
	if snap := b.roomBudgets.snapshot(evt.RoomID.String()); snap.used != 0 {
		t.Fatalf("unattributable tokens changed usage: %+v", snap)
	}
}

func TestBudgetCommandShowsRoomTokenConsumption(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, _ := pollingHarness(t, client)
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	b.roomBudgets = newRoomBudgetsWithClock(1000, time.Hour, nil, testRoomBudgetCapacity, clock.Now)
	b.recordRoomBudget(evt.RoomID, a2aclient.Result{TotalTokens: 250})

	text := b.budgetText(context.Background(), evt.Sender, evt.RoomID)
	if !strings.Contains(text, "Room token budget: 250 of 1000 tokens used this period, 750 remaining") {
		t.Fatalf("budget command missing room token consumption line:\n%s", text)
	}
}

func TestRoomTokenBudgetFailureMessageIsContentSafe(t *testing.T) {
	msg := failureMessage(errorRoomTokenBudget, "agent-k8s", 0)
	if !strings.Contains(msg, "token budget") || !strings.Contains(msg, "reset") {
		t.Fatalf("budget failure notice is not actionable: %q", msg)
	}
	if strings.Contains(msg, "agent-k8s") {
		t.Fatalf("budget notice leaked the ghost name (room budget is not agent-specific): %q", msg)
	}
}

func TestRoomTokenBudgetOverridesFailsClosedOnMalformedConfig(t *testing.T) {
	// A malformed override on an unvalidated Config (config.Load would have rejected it) must fail
	// CLOSED — panic — rather than nil-degrade to the more permissive default budget.
	defer func() {
		if recover() == nil {
			t.Fatal("malformed room budget overrides did not fail closed (no panic)")
		}
	}()
	roomTokenBudgetOverrides(config.Config{RoomTokenBudgetOverrides: []string{"not-a-room"}})
}

func TestRoomTokenBudgetOverridesParsesValidConfig(t *testing.T) {
	got := roomTokenBudgetOverrides(config.Config{
		RoomTokenBudgetOverrides: []string{"!vip:server=1000"},
	})
	if got["!vip:server"] != 1000 {
		t.Fatalf("valid overrides = %v, want !vip:server=1000", got)
	}
}
