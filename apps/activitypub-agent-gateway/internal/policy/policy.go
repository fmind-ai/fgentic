// Package policy is the ActivityPub federation border's strict, fail-closed allowlist. It is the
// AP twin of apps/synapse-federation-policy: a versioned, strictly-parsed policy.json naming the
// signing domains and exact actor URIs permitted to reach an agent, hot-reloadable from git
// without a pod restart (docs/fediverse.md §3, issue #211).
//
// Fail-closed is the whole point: a parse error, an unknown field, an unreadable file, or an
// invalid replacement denies EVERY inbound activity until a valid policy is restored. Deny-by-
// default means an actor is admitted only when its signing origin is explicitly allowlisted, so an
// untrusted remote (prompt injection is threat #1, docs/security.md) never reaches an agent.
package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// SupportedVersion is the only policy schema version this build accepts.
const SupportedVersion = 1

// Policy is an immutable, validated allowlist snapshot.
type Policy struct {
	version        int
	allowedDomains map[string]struct{}
	allowedActors  map[string]struct{}
	// budgets is the optional per-actor/domain token-budget reservation config (nil when absent).
	budgets *budgets
	// digest is a stable, content-free identifier (domain/actor counts + version) for evidence
	// logs; it never contains an actor URI or activity content.
	domainCount int
	actorCount  int
}

// file is the on-disk schema. Unknown fields are rejected at decode time (strict parsing).
type file struct {
	Version        int         `json:"version"`
	AllowedDomains []string    `json:"allowed_domains"`
	AllowedActors  []string    `json:"allowed_actors"`
	Budgets        *budgetFile `json:"budgets"`
}

// Parse validates raw policy bytes into an immutable Policy, failing closed on any defect.
func Parse(raw []byte) (*Policy, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("decode policy: trailing content after JSON document")
	}
	if f.Version != SupportedVersion {
		return nil, fmt.Errorf("unsupported policy version %d (want %d)", f.Version, SupportedVersion)
	}

	domains, err := normalizeSet("allowed_domains", f.AllowedDomains, normalizeDomain)
	if err != nil {
		return nil, err
	}
	actors, err := normalizeSet("allowed_actors", f.AllowedActors, normalizeActor)
	if err != nil {
		return nil, err
	}
	bdg, err := parseBudgets(f.Budgets)
	if err != nil {
		return nil, err
	}
	return &Policy{
		version:        f.Version,
		allowedDomains: domains,
		allowedActors:  actors,
		budgets:        bdg,
		domainCount:    len(domains),
		actorCount:     len(actors),
	}, nil
}

// Load reads and parses a policy file, failing closed on read or parse errors.
func Load(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy %s: %w", path, err)
	}
	p, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse policy %s: %w", path, err)
	}
	return p, nil
}

// Allows reports whether an inbound actor URI is admitted: its host is an allowlisted signing
// domain, or the exact actor URI is explicitly allowlisted. Everything else is denied.
func (p *Policy) Allows(actorURI string) bool {
	parsed, err := url.Parse(actorURI)
	if err != nil || parsed.Host == "" {
		return false
	}
	if _, ok := p.allowedActors[actorURI]; ok {
		return true
	}
	_, ok := p.allowedDomains[strings.ToLower(parsed.Host)]
	return ok
}

// Version is the schema version of the loaded policy.
func (p *Policy) Version() int { return p.version }

// Digest is a stable, content-free summary for evidence logs (never an actor URI).
func (p *Policy) Digest() string {
	return fmt.Sprintf("v%d/d%d/a%d", p.version, p.domainCount, p.actorCount)
}

func normalizeSet(field string, values []string, normalize func(string) (string, error)) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(values))
	for _, raw := range values {
		key, err := normalize(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if _, dup := set[key]; dup {
			return nil, fmt.Errorf("%s: duplicate entry %q", field, key)
		}
		set[key] = struct{}{}
	}
	return set, nil
}

// normalizeDomain accepts a bare host (no scheme, path, port, userinfo), lowercased.
func normalizeDomain(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" || host != raw {
		return "", fmt.Errorf("domain %q must be a trimmed, non-empty host", raw)
	}
	if strings.ContainsAny(host, "/:@ ") || strings.Contains(host, "://") {
		return "", fmt.Errorf("domain %q must be a bare host without scheme, port, or path", raw)
	}
	return strings.ToLower(host), nil
}

// normalizeActor accepts an absolute https actor URI.
func normalizeActor(raw string) (string, error) {
	actor := strings.TrimSpace(raw)
	if actor != raw {
		return "", fmt.Errorf("actor %q must not have surrounding whitespace", raw)
	}
	parsed, err := url.Parse(actor)
	if err != nil {
		return "", fmt.Errorf("actor %q is not a valid URI: %w", raw, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("actor %q must be an absolute https URI", raw)
	}
	return actor, nil
}
