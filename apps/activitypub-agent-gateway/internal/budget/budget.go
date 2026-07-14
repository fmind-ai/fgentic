// Package budget is the ActivityPub gateway's per-actor/per-domain token-budget admission reserver.
// Every AP mention that reaches an agent is an LLM invocation, so cost is a correctness constraint
// (docs/design-decisions.md D7/D8): a remote instance must not drive unbounded model spend. This is
// the twin of the federation lab's per-`azp` maxTokens reservation, keyed on the F3/F4-VERIFIED
// inbound actor URI and its domain — never a spoofable claimed handle.
//
// A reservation gates admission; it is NOT consumption and must never be reported as spend (D8).
// Actual model-token metering stays aggregate at agentgateway. The reserver keeps only fixed-window
// counters keyed by actor and domain — the same keyed-bucket idea as the bridge's rate limiters
// (apps/matrix-a2a-bridge internal/bridge/ratelimit.go), applied to tokens instead of requests, with
// the pool re-read from the git-reloadable policy on every admission so a budget change applies at once.
package budget

import (
	"sync"
	"time"
)

const (
	// idleEviction makes capacity reusable after inactive actors and domains disappear.
	idleEviction = time.Hour
	// sweepInterval amortizes expiry work so high-cardinality churn cannot turn every reservation
	// into a full-map scan.
	sweepInterval = time.Minute
)

// window is a fixed-window token counter for one key.
type window struct {
	start    time.Time
	used     uint64
	lastUsed time.Time
}

// Reserver holds fixed-window token counters keyed by "a\x00<actor>" and "d\x00<domain>". It is
// safe for concurrent use. Capacity bounds the map so a flood of distinct actors cannot exhaust
// memory; when full, a new key fails closed rather than evicting an active one.
type Reserver struct {
	windowLen time.Duration
	capacity  int
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*window
	nextSweep time.Time
}

// New builds a Reserver with a window length (e.g. one minute) and a hard key capacity.
func New(windowLen time.Duration, capacity int) *Reserver {
	return NewWithClock(windowLen, capacity, time.Now)
}

// NewWithClock is New with an injectable clock for deterministic tests.
func NewWithClock(windowLen time.Duration, capacity int, now func() time.Time) *Reserver {
	return &Reserver{
		windowLen: windowLen,
		capacity:  capacity,
		now:       now,
		buckets:   make(map[string]*window),
	}
}

// Reserve atomically debits reservation tokens from both the actor pool and the domain pool for the
// current window, returning true only if BOTH have room. It is all-or-nothing: a request that would
// exhaust either pool reserves from neither, so an over-budget actor cannot partially spend a domain's
// budget. A reservation larger than a pool never fits (a misconfig the policy parser also rejects).
func (r *Reserver) Reserve(actor, domain string, reservation, actorPool, domainPool uint64) bool {
	actorKey := "a\x00" + actor
	domainKey := "d\x00" + domain

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.maybeSweep(now)

	if reservation > actorPool || reservation > domainPool {
		return false
	}
	// Fail closed if admitting these keys would exceed capacity and none already exist.
	newKeys := 0
	if _, ok := r.buckets[actorKey]; !ok {
		newKeys++
	}
	if _, ok := r.buckets[domainKey]; !ok {
		newKeys++
	}
	if len(r.buckets)+newKeys > r.capacity {
		return false
	}
	if !r.fits(actorKey, reservation, actorPool, now) || !r.fits(domainKey, reservation, domainPool, now) {
		return false
	}
	r.commit(actorKey, reservation, now)
	r.commit(domainKey, reservation, now)
	return true
}

// fits reports whether reservation tokens fit in key's current window without committing them. A
// missing or elapsed window is treated as fresh (full pool available).
func (r *Reserver) fits(key string, reservation, pool uint64, now time.Time) bool {
	w, ok := r.buckets[key]
	if !ok || now.Sub(w.start) >= r.windowLen {
		return reservation <= pool
	}
	return w.used+reservation <= pool
}

// commit debits reservation tokens from key, rolling the window when missing or elapsed.
func (r *Reserver) commit(key string, reservation uint64, now time.Time) {
	w, ok := r.buckets[key]
	if !ok || now.Sub(w.start) >= r.windowLen {
		w = &window{start: now}
		r.buckets[key] = w
	}
	w.used += reservation
	w.lastUsed = now
}

// maybeSweep drops idle windows at most once per sweepInterval. The caller holds mu.
func (r *Reserver) maybeSweep(now time.Time) {
	if !r.nextSweep.IsZero() && now.Before(r.nextSweep) {
		return
	}
	for key, w := range r.buckets {
		if now.Sub(w.lastUsed) > idleEviction {
			delete(r.buckets, key)
		}
	}
	r.nextSweep = now.Add(sweepInterval)
}
