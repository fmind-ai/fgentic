package bridge

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// idleEviction makes capacity reusable after inactive identities and rooms disappear.
	idleEviction = time.Hour
	// limiterSweepInterval amortizes expiry work. Together with the hard capacity, this keeps
	// high-cardinality churn from turning every Allow call into a full-map scan.
	limiterSweepInterval = time.Minute
)

// limiters is a keyed set of token buckets (SPEC §4 F7): one per (sender, agent) and one per
// room, guarding LLM spend against chatty rooms, misbehaving bots, and agent reply loops.
type limiters struct {
	perMinute float64
	burst     int
	capacity  int
	now       func() time.Time
	nextSweep time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	lim      *rate.Limiter
	lastUsed time.Time
}

// limiterReservation is one immediately available token that can be returned while a refusal is
// still known to precede the durable write. Once a write begins, an unavailable database response
// is outcome-unknown and the token must remain consumed so recovery cannot escape the spend bound.
type limiterReservation struct {
	reservation *rate.Reservation
	at          time.Time
}

func (r limiterReservation) cancel() {
	if r.reservation != nil {
		r.reservation.CancelAt(r.at)
	}
}

func newLimiters(perMinute float64, burst, capacity int) *limiters {
	return newLimitersWithClock(perMinute, burst, capacity, time.Now)
}

func newLimitersWithClock(perMinute float64, burst, capacity int, now func() time.Time) *limiters {
	return &limiters{
		perMinute: perMinute,
		burst:     burst,
		capacity:  capacity,
		now:       now,
		buckets:   make(map[string]*bucket),
	}
}

// Allow reports whether one more invocation is within budget for key, consuming a token if so.
// Existing keys are always O(1). A new key triggers at most one bounded expiry scan per minute;
// if the map remains full, admission fails closed without evicting an active bucket or resetting
// its burst budget.
func (l *limiters) Allow(key string) bool {
	_, ok := l.reserve(key)
	return ok
}

// reserve consumes one immediately available token and returns a reversible reservation. It never
// reserves future capacity: chat admission remains fail-fast when the bucket has no token now.
func (l *limiters) reserve(key string) (limiterReservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if ok {
		b.lastUsed = now
		return reserveNow(b.lim, now)
	}
	if l.nextSweep.IsZero() || !now.Before(l.nextSweep) {
		l.sweep(now)
		l.nextSweep = now.Add(limiterSweepInterval)
	}
	if len(l.buckets) >= l.capacity {
		return limiterReservation{}, false
	}
	b = &bucket{lim: rate.NewLimiter(rate.Limit(l.perMinute/60), l.burst)}
	b.lastUsed = now
	l.buckets[key] = b
	return reserveNow(b.lim, now)
}

func reserveNow(limiter *rate.Limiter, now time.Time) (limiterReservation, bool) {
	reservation := limiter.ReserveN(now, 1)
	if !reservation.OK() || reservation.DelayFrom(now) > 0 {
		reservation.CancelAt(now)
		return limiterReservation{}, false
	}
	return limiterReservation{reservation: reservation, at: now}, true
}

// sweep drops idle buckets. The caller holds mu, and capacity strictly bounds the scan.
func (l *limiters) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.lastUsed) > idleEviction {
			delete(l.buckets, key)
		}
	}
}
