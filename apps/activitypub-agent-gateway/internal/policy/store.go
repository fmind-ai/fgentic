package policy

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// state is the atomically-swapped current policy snapshot. err != nil means the current file is
// unreadable or invalid, and the border denies EVERY inbound activity until a valid policy returns.
type state struct {
	policy *Policy
	err    error
}

// Store holds the current policy and hot-reloads it from a mounted file without a pod restart,
// mirroring the Synapse policy-reload path (docs/federation.md §8.2). Kubernetes swaps a projected
// ConfigMap's atomic symlink; the store polls that file and validates the whole document before
// swapping — an invalid replacement fails closed rather than silently widening the allowlist.
type Store struct {
	path    string
	log     *slog.Logger
	current atomic.Pointer[state]
}

// NewStore loads the initial policy and returns a Store. A load error is retained as the current
// (deny-all) state rather than returned, so the border starts fail-closed instead of not starting.
func NewStore(path string, log *slog.Logger) *Store {
	s := &Store{path: path, log: log}
	s.reload("initial")
	return s
}

// Watch polls the policy file every interval and reloads on change until ctx is done. It is a
// deliberately simple mtime/content poll (no fsnotify dependency), matching the bridge's projected-
// config reload approach.
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reload("poll")
		}
	}
}

// Reload forces an immediate reload (used by tests and the reload proof).
func (s *Store) Reload() { s.reload("manual") }

func (s *Store) reload(reason string) {
	loaded, err := Load(s.path)
	prev := s.current.Load()
	next := &state{policy: loaded, err: err}
	s.current.Store(next)

	if err != nil {
		// Fail closed and say so, but never log file content.
		s.log.Error("federation policy invalid; denying all inbound", "reason", reason, "error", err)
		return
	}
	if prev == nil || prev.err != nil || prev.policy.Digest() != loaded.Digest() {
		s.log.Info("federation policy loaded", "reason", reason, "digest", loaded.Digest())
	}
}

// Decision is the outcome of an admission check, carrying content-free evidence for logs.
type Decision struct {
	Allowed bool
	Reason  string
	Digest  string
}

// Admit evaluates an actor URI against the current policy, failing closed when the policy is
// invalid or unreadable. The returned Decision carries only a reason and the policy digest — never
// the actor URI or any activity content.
func (s *Store) Admit(actorURI string) Decision {
	st := s.current.Load()
	if st == nil || st.err != nil {
		return Decision{Allowed: false, Reason: "policy_unavailable", Digest: "none"}
	}
	if st.policy.Allows(actorURI) {
		return Decision{Allowed: true, Reason: "allowlisted", Digest: st.policy.Digest()}
	}
	return Decision{Allowed: false, Reason: "off_allowlist", Digest: st.policy.Digest()}
}

// Budget resolves the current policy's token budget for a verified actor, failing closed (deny) when
// the policy is invalid or unreadable. It reads the live snapshot, so a git budget change applies on
// the next admission without a pod restart.
func (s *Store) Budget(actorURI string) (Budget, bool) {
	st := s.current.Load()
	if st == nil || st.err != nil {
		return Budget{}, false
	}
	return st.policy.Budget(actorURI)
}

// Healthy reports whether the current policy is valid (used by readiness).
func (s *Store) Healthy() bool {
	st := s.current.Load()
	return st != nil && st.err == nil
}
