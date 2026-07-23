// Package directory reads authoritative IdP group membership. The reconciler keys every member by
// the stable upstream `sub` and reads the administrator-managed `matrix_localpart` attribute; it
// never derives identity from a mutable username (docs/adr/0009). The Snapshot's Complete flag is a
// load-bearing safety signal: a partial or failed paginated read yields Complete=false, which the
// reconciler treats as "make no grants and no bulk removals" (D6 fail-closed).
package directory

import "context"

// Member is one IdP group member keyed by its immutable upstream subject. Localpart is the
// single-valued administrator-managed `matrix_localpart` attribute (may be empty/invalid, which the
// reconciler fails closed on rather than guessing an MXID).
type Member struct {
	Sub       string
	Localpart string
}

// Snapshot is the authoritative membership of the requested groups at one point in time. Groups is
// keyed by exact group path. Complete is true only when EVERY requested group was fully and
// successfully paginated; any transport error, timeout, or truncated page makes it false.
type Snapshot struct {
	Groups   map[string][]Member
	Complete bool
}

// Directory is the read-only IdP membership source. Implementations must set Complete=false (never
// silently drop members) on any partial read so the reconciler can retain last-known Matrix state.
type Directory interface {
	// Snapshot reads the current membership of exactly the requested group paths.
	Snapshot(ctx context.Context, groups []string) (Snapshot, error)
}
