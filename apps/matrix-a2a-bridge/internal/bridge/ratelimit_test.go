package bridge

import (
	"testing"
	"time"
)

const testRateLimitBucketCapacity = 4096

type limiterTestClock struct {
	now time.Time
}

func (c *limiterTestClock) Now() time.Time {
	return c.now
}

func (c *limiterTestClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestLimitersFailClosedAtCapacityWithoutResettingBuckets(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	limits := newLimitersWithClock(1, 1, 2, clock.Now)

	if !limits.Allow("sender-a") || !limits.Allow("sender-b") {
		t.Fatal("initial buckets did not receive their configured burst")
	}
	if limits.Allow("sender-c") {
		t.Fatal("unknown key was admitted after limiter capacity was exhausted")
	}
	if got := len(limits.buckets); got != 2 {
		t.Fatalf("bucket count = %d, want hard cap 2", got)
	}
	if _, exists := limits.buckets["sender-c"]; exists {
		t.Fatal("rejected key was retained in the limiter map")
	}
	if limits.Allow("sender-a") {
		t.Fatal("capacity churn reset an existing sender's exhausted burst")
	}
}

func TestLimiterReservationCanRollBackBeforeDurableWrite(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	limits := newLimitersWithClock(1, 1, 1, clock.Now)

	reservation, ok := limits.reserve("sender")
	if !ok {
		t.Fatal("initial reservation was rejected")
	}
	if limits.Allow("sender") {
		t.Fatal("reserved token remained available before rollback")
	}
	reservation.cancel()
	if !limits.Allow("sender") {
		t.Fatal("known pre-write refusal did not return its reserved token")
	}
}

func TestLimiterSnapshotDoesNotCreateRefreshOrConsumeBucket(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	limits := newLimitersWithClock(60, 3, 2, clock.Now)

	if got := limits.snapshot("unseen"); got.available != 3 || got.burst != 3 || got.perMinute != 60 {
		t.Fatalf("unseen snapshot = %+v", got)
	}
	if len(limits.buckets) != 0 || !limits.nextSweep.IsZero() {
		t.Fatal("unseen snapshot mutated limiter state")
	}
	if !limits.Allow("sender") {
		t.Fatal("initial sender token was rejected")
	}
	bucket := limits.buckets["sender"]
	lastUsed := bucket.lastUsed
	if got := limits.snapshot("sender").available; got != 2 {
		t.Fatalf("sender snapshot availability = %d, want 2", got)
	}
	if bucket.lastUsed != lastUsed {
		t.Fatal("snapshot refreshed bucket activity")
	}
	for range 2 {
		if !limits.Allow("sender") {
			t.Fatal("snapshot consumed a remaining burst token")
		}
	}
	if limits.Allow("sender") {
		t.Fatal("snapshot increased the remaining burst")
	}
}

func TestLimiterSnapshotReflectsUnseenKeyCapacityWithoutSweeping(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	limits := newLimitersWithClock(60, 3, 1, clock.Now)
	if !limits.Allow("occupant") {
		t.Fatal("initial bucket was rejected")
	}

	if got := limits.snapshot("unseen").available; got != 0 {
		t.Fatalf("unseen snapshot at map capacity = %d, want 0", got)
	}
	if len(limits.buckets) != 1 {
		t.Fatal("capacity snapshot mutated limiter map")
	}

	clock.Advance(idleEviction + limiterSweepInterval)
	if got := limits.snapshot("unseen").available; got != 3 {
		t.Fatalf("unseen snapshot with a sweepable idle bucket = %d, want 3", got)
	}
	if _, exists := limits.buckets["occupant"]; !exists {
		t.Fatal("snapshot swept the reusable idle bucket")
	}
	if !limits.Allow("unseen") {
		t.Fatal("reserve disagreed with the sweepable snapshot")
	}
}

func TestBridgeLimiterMapsUseConfiguredCapacity(t *testing.T) {
	b := testBridge(t)
	for name, limits := range map[string]*limiters{
		"invocation sender": b.senderLimits,
		"invocation room":   b.roomLimits,
		"notice sender":     b.noticeSenderLimits,
		"notice room":       b.noticeRoomLimits,
	} {
		if limits.capacity != testRateLimitBucketCapacity {
			t.Errorf("%s capacity = %d, want %d", name, limits.capacity, testRateLimitBucketCapacity)
		}
	}
}

func TestLimitersExistingKeysStayConstantTimeUntilNewKeySweep(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	limits := newLimitersWithClock(60, 1, 2, clock.Now)

	if !limits.Allow("active") || !limits.Allow("stale") {
		t.Fatal("initial buckets were not admitted")
	}
	firstSweepDeadline := limits.nextSweep
	clock.Advance(idleEviction + limiterSweepInterval)
	if !limits.Allow("active") {
		t.Fatal("existing active key did not refill")
	}
	if got := len(limits.buckets); got != 2 {
		t.Fatalf("existing-key lookup swept map: bucket count = %d, want 2", got)
	}
	if !limits.nextSweep.Equal(firstSweepDeadline) {
		t.Fatal("existing-key lookup changed the scheduled sweep deadline")
	}

	if !limits.Allow("replacement") {
		t.Fatal("new key did not recover capacity from the idle bucket")
	}
	if _, exists := limits.buckets["stale"]; exists {
		t.Fatal("idle bucket survived the scheduled new-key sweep")
	}
	if _, exists := limits.buckets["active"]; !exists {
		t.Fatal("recently used bucket was evicted")
	}
}

func TestLimitersSweepAtMostOncePerMinuteAndEvictOnlyAfterIdleWindow(t *testing.T) {
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	limits := newLimitersWithClock(60, 1, 1, clock.Now)

	if !limits.Allow("stale") {
		t.Fatal("initial bucket was not admitted")
	}
	clock.Advance(idleEviction)
	if limits.Allow("replacement") {
		t.Fatal("bucket was evicted at exactly the idle boundary")
	}
	deferredUntil := limits.nextSweep
	if want := clock.Now().Add(limiterSweepInterval); !deferredUntil.Equal(want) {
		t.Fatalf("next sweep = %s, want %s", deferredUntil, want)
	}

	clock.Advance(limiterSweepInterval - time.Nanosecond)
	if limits.Allow("replacement") {
		t.Fatal("capacity recovered before the next sweep interval")
	}
	if !limits.nextSweep.Equal(deferredUntil) {
		t.Fatal("rejected churn scheduled more than one sweep per minute")
	}

	clock.Advance(time.Nanosecond)
	if !limits.Allow("replacement") {
		t.Fatal("capacity did not recover once idle expiry and sweep cadence elapsed")
	}
	if _, exists := limits.buckets["stale"]; exists {
		t.Fatal("expired bucket was not evicted")
	}
}
