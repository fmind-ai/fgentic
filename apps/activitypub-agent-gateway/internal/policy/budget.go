package policy

import (
	"fmt"
	"net/url"
	"strings"
)

// budgetFile is the optional on-disk budget section of policy.json. It declares a fixed per-
// delegation token RESERVATION and the per-window token pool each signing domain (and, optionally,
// each exact actor) may draw from. It is the ActivityPub twin of the federation lab's per-`azp`
// maxTokens reservation (docs/design-decisions.md D7/D8): a reservation gates admission and is never
// consumption. Unknown fields are rejected by the strict decoder in Parse.
type budgetFile struct {
	// ReservationTokens is the token ceiling reserved for one delegation (the maxTokens estimate).
	ReservationTokens uint64 `json:"reservation_tokens"`
	// DefaultTokensPerMinute is the per-window pool for an allowlisted domain with no explicit
	// entry. 0 (the default) means deny-by-default: an allowlisted domain without a budget is denied.
	DefaultTokensPerMinute uint64 `json:"default_tokens_per_minute"`
	// Domains maps a bare signing host to its per-window token pool.
	Domains map[string]uint64 `json:"domains"`
	// Actors maps an exact https actor URI to a per-window token pool overriding its domain's.
	Actors map[string]uint64 `json:"actors"`
}

// budgets is the immutable, validated budget configuration.
type budgets struct {
	reservation uint64
	defaultPool uint64
	domains     map[string]uint64
	actors      map[string]uint64
}

// Budget is the resolved reservation and per-window pools for one verified actor.
type Budget struct {
	// Reservation is the token amount reserved for this delegation.
	Reservation uint64
	// ActorPool bounds the individual actor over one window; DomainPool bounds the whole domain.
	ActorPool  uint64
	DomainPool uint64
}

// parseBudgets validates the optional budget section into an immutable budgets, failing closed on
// any defect. A pool smaller than the reservation could never admit, so it is rejected as a misconfig.
func parseBudgets(f *budgetFile) (*budgets, error) {
	if f == nil {
		return nil, nil
	}
	if f.ReservationTokens == 0 {
		return nil, fmt.Errorf("budgets: reservation_tokens must be positive")
	}
	if f.DefaultTokensPerMinute != 0 && f.DefaultTokensPerMinute < f.ReservationTokens {
		return nil, fmt.Errorf("budgets: default_tokens_per_minute %d is below reservation_tokens %d", f.DefaultTokensPerMinute, f.ReservationTokens)
	}
	domains, err := normalizeBudgetSet("budgets.domains", f.Domains, f.ReservationTokens, normalizeDomain)
	if err != nil {
		return nil, err
	}
	actors, err := normalizeBudgetSet("budgets.actors", f.Actors, f.ReservationTokens, normalizeActor)
	if err != nil {
		return nil, err
	}
	return &budgets{
		reservation: f.ReservationTokens,
		defaultPool: f.DefaultTokensPerMinute,
		domains:     domains,
		actors:      actors,
	}, nil
}

func normalizeBudgetSet(field string, raw map[string]uint64, reservation uint64, normalize func(string) (string, error)) (map[string]uint64, error) {
	set := make(map[string]uint64, len(raw))
	for key, pool := range raw {
		norm, err := normalize(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if _, dup := set[norm]; dup {
			return nil, fmt.Errorf("%s: duplicate entry %q", field, norm)
		}
		if pool < reservation {
			return nil, fmt.Errorf("%s: pool %d for %q is below reservation_tokens %d", field, pool, norm, reservation)
		}
		set[norm] = pool
	}
	return set, nil
}

// Budget resolves the reservation and pools for a verified actor URI. It returns false — deny — when
// no budget is configured at all, or when an allowlisted domain has neither an explicit pool nor a
// default (deny-by-default). An unlisted actor inherits its domain's pool as its individual cap.
func (p *Policy) Budget(actorURI string) (Budget, bool) {
	if p.budgets == nil {
		return Budget{}, false
	}
	parsed, err := url.Parse(actorURI)
	if err != nil || parsed.Host == "" {
		return Budget{}, false
	}
	host := strings.ToLower(parsed.Host)
	domainPool, ok := p.budgets.domains[host]
	if !ok {
		domainPool = p.budgets.defaultPool
	}
	if domainPool == 0 {
		return Budget{}, false // allowlisted but unbudgeted domain: deny-by-default
	}
	actorPool, ok := p.budgets.actors[actorURI]
	if !ok {
		actorPool = domainPool
	}
	return Budget{Reservation: p.budgets.reservation, ActorPool: actorPool, DomainPool: domainPool}, true
}
