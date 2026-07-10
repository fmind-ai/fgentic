package bridge

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// idleEviction bounds the limiter map: entries untouched this long are dropped on the next
// sweep, so a churn of rooms/senders cannot grow memory unbounded.
const idleEviction = time.Hour

// limiters is a keyed set of token buckets (SPEC §4 F7): one per (sender, agent) and one per
// room, guarding LLM spend against chatty rooms, misbehaving bots, and agent reply loops.
type limiters struct {
	perMinute float64
	burst     int

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	lim      *rate.Limiter
	lastUsed time.Time
}

func newLimiters(perMinute float64, burst int) *limiters {
	return &limiters{
		perMinute: perMinute,
		burst:     burst,
		buckets:   make(map[string]*bucket),
	}
}

// Allow reports whether one more invocation is within budget for key, consuming a token if so.
func (l *limiters) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Limit(l.perMinute/60), l.burst)}
		l.buckets[key] = b
	}
	b.lastUsed = now
	allowed := b.lim.Allow()
	l.sweep(now)
	return allowed
}

// sweep drops idle buckets; called under mu with chat-scale volumes, so a full scan is fine.
func (l *limiters) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.lastUsed) > idleEviction {
			delete(l.buckets, key)
		}
	}
}
