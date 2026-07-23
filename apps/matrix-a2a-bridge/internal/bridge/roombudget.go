package bridge

import (
	"fmt"
	"math"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
)

// roomTokenBudgetOverrides resolves the validated per-room override map from config. config.Load has
// already rejected a malformed entry (fail fast), so a parse error here can only mean New was called
// on an unvalidated Config. It fails CLOSED by panicking rather than nil-degrading to the default
// budget: a cost-enforcement control must never silently fall back to a more permissive limit because
// its own configuration could not be parsed. This mirrors New's existing regexp.MustCompile
// contract — an unvalidated Config is a programming error, not a runtime input.
func roomTokenBudgetOverrides(cfg config.Config) map[string]int {
	overrides, err := cfg.RoomTokenBudgetMap()
	if err != nil {
		panic(fmt.Sprintf("bridge constructed with unvalidated room token budget overrides: %v", err))
	}
	return overrides
}

// roomBudgetAuditSchema tags the content-free audit record emitted when a room reaches its budget.
const roomBudgetAuditSchema = "fgentic.room_token_budget.v1"

// roomBudgets enforces per-room cumulative model-token budgets (#99): a hard cap on the exact model
// tokens a room may spend per rolling period, layered ON TOP of the per-sender/per-room invocation
// rate limits and agentgateway token metering — never a replacement (D7/D8). Usage is metered
// bridge-side from each completed delegation's exact kagent token total (docs/audit.md §157) because
// agentgateway's aggregate GenAI metrics deliberately carry no room label and cannot attribute per
// room (docs/observability.md §9.1).
//
// It fails closed on the budget (a room at or over its ceiling is refused until the window resets)
// but never leaks another room's state and never refuses a room that has not demonstrably exceeded
// its own ceiling. State is in-memory, mirroring the rate limiters: a restart resets the windows,
// and the period reset bounds long-run drift. The bounded map with idle eviction (shared with the
// limiters via idleEviction/limiterSweepInterval) keeps high room cardinality from growing without
// limit; a room that cannot be tracked at capacity is allowed rather than refused, so accounting —
// not admission — is what fails open under extreme churn, and the invocation rate limits still bound
// the blast radius.
type roomBudgets struct {
	defaultLimit int
	period       time.Duration
	overrides    map[string]int
	capacity     int
	now          func() time.Time
	nextSweep    time.Time

	mu      sync.Mutex
	windows map[string]*roomBudgetWindow
}

// roomBudgetWindow is one room's accumulated usage within the current period.
type roomBudgetWindow struct {
	used        int
	windowStart time.Time
	lastUsed    time.Time
}

// roomBudgetSnapshot is the read-only view the budget command and audit record consume. It never
// mutates state and never carries another room's data.
type roomBudgetSnapshot struct {
	// limited reports whether a finite budget applies (limit > 0). remaining/resetAt/exhausted are
	// meaningful only when limited is true.
	limited   bool
	limit     int
	used      int
	remaining int
	resetAt   time.Time
	exhausted bool
}

func newRoomBudgets(defaultLimit int, period time.Duration, overrides map[string]int, capacity int) *roomBudgets {
	return newRoomBudgetsWithClock(defaultLimit, period, overrides, capacity, time.Now)
}

func newRoomBudgetsWithClock(
	defaultLimit int,
	period time.Duration,
	overrides map[string]int,
	capacity int,
	now func() time.Time,
) *roomBudgets {
	return &roomBudgets{
		defaultLimit: defaultLimit,
		period:       period,
		overrides:    overrides,
		capacity:     capacity,
		now:          now,
		windows:      make(map[string]*roomBudgetWindow),
	}
}

// enabled reports whether any finite budget is configured. When false the whole feature is inert and
// callers skip it entirely, preserving the prior unlimited behavior with zero overhead.
func (rb *roomBudgets) enabled() bool {
	if rb == nil {
		return false
	}
	if rb.defaultLimit > 0 {
		return true
	}
	for _, limit := range rb.overrides {
		if limit > 0 {
			return true
		}
	}
	return false
}

// limitFor resolves the token ceiling for a room: its explicit override, else the default. 0 means
// unlimited (either configured explicitly for that room or the global default).
func (rb *roomBudgets) limitFor(room string) int {
	if limit, ok := rb.overrides[room]; ok {
		return limit
	}
	return rb.defaultLimit
}

// allow reports whether a NEW invocation is within the room's budget for the current period. An
// unlimited room is always allowed; a room whose window cannot be tracked at map capacity is allowed
// (accounting, not admission, fails open); otherwise admission holds only while used < limit.
func (rb *roomBudgets) allow(room string) bool {
	limit := rb.limitFor(room)
	if limit <= 0 {
		return true
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	w := rb.window(room, rb.now())
	if w == nil {
		return true
	}
	return w.used < limit
}

// record adds a completed delegation's exact token total to the room's current window. It is a no-op
// for a non-positive count (not attributable) or an unlimited room, so only rooms with a real budget
// occupy the map.
func (rb *roomBudgets) record(room string, tokens int) {
	if tokens <= 0 || rb.limitFor(room) <= 0 {
		return
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	w := rb.window(room, rb.now())
	if w == nil {
		return
	}
	w.used = addClampInt(w.used, tokens)
}

// snapshot returns the room's current budget view without creating or mutating a window. A due reset
// is reflected as zero usage against a fresh window so the reader never sees stale spend.
func (rb *roomBudgets) snapshot(room string) roomBudgetSnapshot {
	limit := rb.limitFor(room)
	snap := roomBudgetSnapshot{limited: limit > 0, limit: limit}
	if limit <= 0 {
		return snap
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	now := rb.now()
	switch w, ok := rb.windows[room]; {
	case ok && !rb.expired(w, now):
		snap.used = w.used
		snap.resetAt = w.windowStart.Add(rb.period)
	default:
		// Unseen or expired: the next invocation opens a fresh window starting now.
		snap.resetAt = now.Add(rb.period)
	}
	snap.remaining = max(0, limit-snap.used)
	snap.exhausted = snap.used >= limit
	return snap
}

// window returns the room's live window, resetting it when the period has elapsed and creating it on
// first use. The caller holds mu. It returns nil only when the room is unseen and the bounded map is
// full after a due sweep, so an untracked room is never refused.
func (rb *roomBudgets) window(room string, now time.Time) *roomBudgetWindow {
	if w, ok := rb.windows[room]; ok {
		if rb.expired(w, now) {
			w.used = 0
			w.windowStart = now
		}
		w.lastUsed = now
		return w
	}
	if rb.nextSweep.IsZero() || !now.Before(rb.nextSweep) {
		rb.sweep(now)
		rb.nextSweep = now.Add(limiterSweepInterval)
	}
	// LOAD-BEARING INVARIANT: at map capacity an unseen room returns nil, and allow() then admits it
	// (the budget fails OPEN here). That is only safe because the sibling invocation rate limiter
	// (Bridge.roomLimits) fails CLOSED at the SAME capacity, is keyed by the SAME room ID string, and
	// is sized by the SAME RateLimitBucketCapacity — and the set of budgeted rooms is a strict subset
	// of rate-limited rooms (every invocation reserves a roomLimits token first). So any room this map
	// cannot track is already refused by roomLimits before an untracked budget can be exceeded, and
	// the spend blast radius stays bounded. If a future change diverges either map's capacity, keying,
	// or admission order, this fail-open silently unmasks the budget cap — preserve the equal-capacity,
	// same-room-keyed, fail-closed roomLimits pairing or replace this with an explicit fail-closed path.
	if len(rb.windows) >= rb.capacity {
		return nil
	}
	w := &roomBudgetWindow{windowStart: now, lastUsed: now}
	rb.windows[room] = w
	return w
}

// expired reports whether a window's period has fully elapsed. windowStart of zero (never happens for
// a live window) is treated as expired, so any degenerate state resets rather than sticking.
func (rb *roomBudgets) expired(w *roomBudgetWindow, now time.Time) bool {
	return now.Sub(w.windowStart) >= rb.period
}

// sweep drops windows idle beyond idleEviction. The caller holds mu and capacity bounds the scan.
func (rb *roomBudgets) sweep(now time.Time) {
	for room, w := range rb.windows {
		if now.Sub(w.lastUsed) > idleEviction {
			delete(rb.windows, room)
		}
	}
}

// addClampInt adds two non-negative token counts, saturating at MaxInt so an extreme provider total
// can never overflow the accumulator into a negative value that would silently reopen a budget.
func addClampInt(a, b int) int {
	if a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// roomBudgetAllows is the bridge-level admission gate: it reports whether a room may invoke an agent
// under its token budget and, on refusal, records the aggregate exhaustion metric and one content-free
// audit event. It never exposes another room's usage and is inert when no budget is configured.
func (b *Bridge) roomBudgetAllows(roomID id.RoomID) bool {
	if !b.roomBudgets.enabled() {
		return true
	}
	room := roomID.String()
	if b.roomBudgets.allow(room) {
		return true
	}
	roomBudgetExhaustionsTotal.Inc()
	snap := b.roomBudgets.snapshot(room)
	b.auditLog.Info("room token budget exhausted",
		"audit_schema", roomBudgetAuditSchema,
		"room_id", room,
		"tokens_used", snap.used,
		"token_budget", snap.limit,
		"reset_at", snap.resetAt.UTC().Format(time.RFC3339),
	)
	return false
}

// recordRoomBudget meters a completed delegation's exact token total against its room budget. It is a
// no-op when the feature is disabled or the result carried no attributable usage (a bare terminal
// Message or a server that does not report kagent usage), so unattributed spend is never invented.
func (b *Bridge) recordRoomBudget(roomID id.RoomID, result a2aclient.Result) {
	if !b.roomBudgets.enabled() || result.TotalTokens <= 0 {
		return
	}
	b.roomBudgets.record(roomID.String(), result.TotalTokens)
}
