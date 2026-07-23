package a2aclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
)

// TokenBudgetExtensionURI identifies Fgentic's A2A extension for a caller-supplied,
// partner-enforced maximum token budget on one remote delegation. It is the always-on base of
// every remote delegation's activated extension set (docs/bridge.md §6); operators add more
// through the per-remote `extensions:` config, negotiated against the verified card.
const TokenBudgetExtensionURI = "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1"

const maxExactJSONInteger = 1<<53 - 1

type targetKind uint8

const (
	targetKindLocal targetKind = iota + 1
	targetKindRemote
	targetKindFediverse
)

// CardIdentity is the operator-pinned identity a remote signed AgentCard must match.
// PublicKeyJWK must contain an ES256 public key (EC, P-256); it is public trust material,
// never a private signing key.
type CardIdentity struct {
	Name         string
	Organization string
	KeyID        string
	PublicKeyJWK string
	// AdditionalKeys are further currently-valid signing keys for a rotation overlap window (#352): a
	// card verifies if its kid matches the primary key above OR any of these. RevokedKeyIDs are key IDs
	// explicitly retired — a card offered under one is refused even if cryptographically valid, and no
	// pinned key ID may appear in this list (that fails closed).
	AdditionalKeys []CardKey
	RevokedKeyIDs  []string
}

// CardKey is one additional pinned signing key (kid + its ES256 public JWK) in a rotation overlap.
type CardKey struct {
	KeyID        string
	PublicKeyJWK string
}

// ActivityPubIdentity is the exact operator pin required before an acct mapping may use its
// ActivityPub fallback. A broker verifies the actor's FEP-8b32 proof under this Ed25519 Multikey.
type ActivityPubIdentity struct {
	ActorID            string
	VerificationMethod string
	PublicKeyMultibase string
	ProofMaxAge        time.Duration
}

// Target is an immutable, validated A2A routing target. Local targets are paths relative to
// Client's configured A2A base URL. Remote targets bind one exact endpoint to a pinned card
// identity and a per-request token budget.
type Target struct {
	kind                 targetKind
	address              string
	cardURL              string // optional discovery-provided Signed AgentCard URL (#220)
	expectedName         string
	expectedOrganization string
	expectedKeyID        string      // primary/active key ID, surfaced in the card_key_id audit field
	pins                 []targetPin // all currently-valid pinned keys (primary first); overlap window (#352)
	revokedKeyIDs        []string    // sorted, deduped retired key IDs; refused even if cryptographically valid
	tokenBudget          uint64
	extensions           []string // sorted, deduped operator-configured extras (excludes token-budget)
	identityFingerprint  [sha256.Size]byte
	tls                  *remoteTLS // client-cert material for A2A v1.0 mTLS; nil when unconfigured (#244)
	activityPubIdentity  ActivityPubIdentity
	id                   string
}

// remoteTLS is the transport-layer mutual-authentication material for a remote target (#244): the
// bridge's client certificate presented to the partner, plus optional pinned server roots. It is
// operational config (like extensions and token budget), not card identity, so it stays out of
// identityFingerprint but folds into the opaque target ID through its fingerprint — a cert rotation
// therefore re-verifies the card and re-keys any queued delegation.
type remoteTLS struct {
	certificate tls.Certificate
	roots       *x509.CertPool // pinned server roots; nil defers to the system trust store
	fingerprint string         // stable hash of the client-cert material, folded into the target ID
}

// RemoteOption customises a remote Target beyond its required identity and budget. Existing callers
// pass none; mTLS is opted in through WithClientTLS so the constructor signature stays stable (#244).
type RemoteOption func(*remoteConfig)

type remoteConfig struct {
	tls *remoteTLS
}

// WithClientTLS pins the client certificate the bridge presents to a remote A2A endpoint for mTLS,
// with optional server roots that replace the system trust store. fingerprint is a stable digest of
// the certificate material so rotating the cert re-keys the target and forces card re-verification.
func WithClientTLS(certificate tls.Certificate, roots *x509.CertPool, fingerprint string) RemoteOption {
	return func(c *remoteConfig) {
		c.tls = &remoteTLS{certificate: certificate, roots: roots, fingerprint: fingerprint}
	}
}

// NewLocalTarget validates a path served beneath Client's configured A2A base URL.
func NewLocalTarget(agentPath string) (Target, error) {
	normalized, err := normalizeLocalPath(agentPath)
	if err != nil {
		return Target{}, err
	}
	fingerprint := sha256.Sum256([]byte("local\x00" + normalized))
	return Target{
		kind:                targetKindLocal,
		address:             normalized,
		identityFingerprint: fingerprint,
		id:                  "local:" + hex.EncodeToString(fingerprint[:]),
	}, nil
}

// NewRemoteTarget validates an exact remote A2A endpoint and its pinned ES256 card identity.
// tokenBudget is transmitted through TokenBudgetExtensionURI and must be positive. extensions
// lists additional A2A extension URIs to activate on top of the always-on token-budget contract;
// they also form the allowlist of `required: true` card extensions the bridge will accept
// (docs/bridge.md §6). A change to any of these inputs yields a new opaque ID, forcing re-verify.
func NewRemoteTarget(rawURL string, identity CardIdentity, tokenBudget uint64, extensions []string, opts ...RemoteOption) (Target, error) {
	endpoint, err := NormalizeRemoteURL(rawURL)
	if err != nil {
		return Target{}, err
	}
	normalizedExtensions, err := normalizeExtensionURIs(extensions)
	if err != nil {
		return Target{}, err
	}
	var cfg remoteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if identity.Name == "" || identity.Name != strings.TrimSpace(identity.Name) {
		return Target{}, fmt.Errorf("remote card identity name must not be empty")
	}
	if identity.Organization == "" || identity.Organization != strings.TrimSpace(identity.Organization) {
		return Target{}, fmt.Errorf("remote card identity organization must not be empty")
	}
	if identity.KeyID == "" || identity.KeyID != strings.TrimSpace(identity.KeyID) {
		return Target{}, fmt.Errorf("remote card identity keyID must not be empty")
	}
	if tokenBudget == 0 {
		return Target{}, fmt.Errorf("remote token budget must be positive")
	}
	if tokenBudget > maxExactJSONInteger {
		return Target{}, fmt.Errorf("remote token budget must not exceed %d", maxExactJSONInteger)
	}
	// The primary key plus any overlap keys (#352). Parse and encode each; reject duplicate key IDs so a
	// pin set can never be ambiguous about which public key a kid names.
	rawKeys := append([]CardKey{{KeyID: identity.KeyID, PublicKeyJWK: identity.PublicKeyJWK}}, identity.AdditionalKeys...)
	pins := make([]targetPin, 0, len(rawKeys))
	seenKeyIDs := make(map[string]struct{}, len(rawKeys))
	for index, rawKey := range rawKeys {
		if rawKey.KeyID == "" || rawKey.KeyID != strings.TrimSpace(rawKey.KeyID) {
			return Target{}, fmt.Errorf("remote card identity key %d keyID must not be empty", index)
		}
		if _, duplicate := seenKeyIDs[rawKey.KeyID]; duplicate {
			return Target{}, fmt.Errorf("remote card identity has duplicate key ID %q", rawKey.KeyID)
		}
		seenKeyIDs[rawKey.KeyID] = struct{}{}
		publicKey, err := agentcardjws.ParsePublicJWK(
			[]byte(rawKey.PublicKeyJWK),
			rawKey.KeyID,
			agentcardjws.AllowOptionalJWKMetadata,
		)
		if err != nil {
			return Target{}, fmt.Errorf("remote card identity public key %q: %w", rawKey.KeyID, err)
		}
		publicKeyBytes, err := publicKey.Bytes()
		if err != nil {
			return Target{}, fmt.Errorf("encode remote card identity public key %q: %w", rawKey.KeyID, err)
		}
		var encoded [65]byte
		copy(encoded[:], publicKeyBytes)
		pins = append(pins, targetPin{keyID: rawKey.KeyID, publicKey: encoded})
	}

	// Revoked key IDs are sorted+deduped; no pinned key ID may be revoked (fail closed on the contradiction).
	revokedKeyIDs, err := normalizeRevokedKeyIDs(identity.RevokedKeyIDs, seenKeyIDs)
	if err != nil {
		return Target{}, err
	}

	// The fingerprint binds every pinned key (sorted by ID) and the revoked list, so any rotation — adding
	// an overlap key, retiring one, or revoking a kid — yields a new opaque ID and re-verifies the card.
	fingerprintParts := []string{"remote", endpoint, identity.Name, identity.Organization}
	sortedPins := append([]targetPin(nil), pins...)
	sort.Slice(sortedPins, func(i, j int) bool { return sortedPins[i].keyID < sortedPins[j].keyID })
	for _, pin := range sortedPins {
		fingerprintParts = append(fingerprintParts, pin.keyID, base64.RawURLEncoding.EncodeToString(pin.publicKey[:]))
	}
	fingerprintParts = append(fingerprintParts, "revoked")
	fingerprintParts = append(fingerprintParts, revokedKeyIDs...)
	fingerprint := sha256.Sum256([]byte(strings.Join(fingerprintParts, "\x00")))
	// Extensions are operational config, not identity: they stay out of identityFingerprint (like
	// tokenBudget) but fold into the opaque ID so a config change re-verifies the card against the
	// new required-extension allowlist.
	mtlsFingerprint := ""
	if cfg.tls != nil {
		mtlsFingerprint = cfg.tls.fingerprint
	}
	idInput := fmt.Sprintf("%x\x00%d\x00%s\x00%s", fingerprint, tokenBudget, strings.Join(normalizedExtensions, "\x1f"), mtlsFingerprint)
	id := sha256.Sum256([]byte(idInput))

	return Target{
		kind:                 targetKindRemote,
		address:              endpoint,
		expectedName:         identity.Name,
		expectedOrganization: identity.Organization,
		expectedKeyID:        identity.KeyID,
		pins:                 pins,
		revokedKeyIDs:        revokedKeyIDs,
		tokenBudget:          tokenBudget,
		extensions:           normalizedExtensions,
		identityFingerprint:  fingerprint,
		tls:                  cfg.tls,
		id:                   "remote:" + hex.EncodeToString(id[:]),
	}, nil
}

// NewFediverseTarget validates an acct handle plus independent A2A and ActivityPub trust pins. The
// private broker resolves WebFinger and chooses the transport; no network discovery occurs here.
func NewFediverseTarget(handle string, card CardIdentity, activityPub ActivityPubIdentity, tokenBudget uint64, extensions []string) (Target, error) {
	canonical, err := normalizeAcctHandle(handle)
	if err != nil {
		return Target{}, err
	}
	if _, err := NormalizeRemoteURL(activityPub.ActorID); err != nil || !strings.HasPrefix(activityPub.ActorID, "https://") {
		return Target{}, fmt.Errorf("ActivityPub actor ID must be a canonical https URL")
	}
	if !strings.HasPrefix(activityPub.VerificationMethod, activityPub.ActorID+"#") {
		return Target{}, fmt.Errorf("ActivityPub verificationMethod must be a fragment of actorID")
	}
	if !multikeyRE.MatchString(activityPub.PublicKeyMultibase) {
		return Target{}, fmt.Errorf("ActivityPub publicKeyMultibase must be a base58btc Multikey")
	}
	if activityPub.ProofMaxAge < time.Second || activityPub.ProofMaxAge > 30*24*time.Hour {
		return Target{}, fmt.Errorf("ActivityPub proofMaxAge must be between 1s and 720h")
	}
	// Reuse the remote constructor as the single validator for the Signed AgentCard identity,
	// token budget, and extension contract. The placeholder URL is never dialed.
	validated, err := NewRemoteTarget(activityPub.ActorID, card, tokenBudget, extensions)
	if err != nil {
		return Target{}, err
	}
	fingerprintInput := strings.Join([]string{
		"fediverse", canonical, activityPub.ActorID, activityPub.VerificationMethod,
		activityPub.PublicKeyMultibase, activityPub.ProofMaxAge.String(), hex.EncodeToString(validated.identityFingerprint[:]),
	}, "\x00")
	fingerprint := sha256.Sum256([]byte(fingerprintInput))
	idInput := fmt.Sprintf("%x\x00%d\x00%s", fingerprint, tokenBudget, strings.Join(validated.extensions, "\x1f"))
	id := sha256.Sum256([]byte(idInput))
	validated.kind = targetKindFediverse
	validated.address = canonical
	validated.cardURL = ""
	validated.activityPubIdentity = activityPub
	validated.identityFingerprint = fingerprint
	validated.id = "fediverse:" + hex.EncodeToString(id[:])
	return validated, nil
}

// normalizeExtensionURIs validates operator-configured extra extension URIs: each must be an
// absolute https URI, listed at most once, and must not restate the always-on token-budget base
// contract. The result is sorted so the target ID stays stable regardless of config ordering.
func normalizeExtensionURIs(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	normalized := make([]string, 0, len(raw))
	for _, uri := range raw {
		if uri == "" || strings.TrimSpace(uri) != uri {
			return nil, fmt.Errorf("extension URI %q must be non-empty without surrounding whitespace", uri)
		}
		if uri == TokenBudgetExtensionURI {
			return nil, fmt.Errorf("extension %q is always active and must not be listed explicitly", uri)
		}
		parsed, err := url.Parse(uri)
		if err != nil {
			return nil, fmt.Errorf("parse extension URI %q: %w", uri, err)
		}
		if parsed.Scheme != "https" || parsed.Host == "" {
			return nil, fmt.Errorf("extension URI %q must be an absolute https URI", uri)
		}
		if _, dup := seen[uri]; dup {
			return nil, fmt.Errorf("extension URI %q is listed more than once", uri)
		}
		seen[uri] = struct{}{}
		normalized = append(normalized, uri)
	}
	slices.Sort(normalized)
	return normalized, nil
}

// String returns the local agent path or normalized exact remote endpoint. Targets reject URL
// credentials and query strings, so this value is safe for routing diagnostics.
func (t Target) String() string {
	return t.address
}

// IsRemote reports whether this target requires a verified signed AgentCard before use.
func (t Target) IsRemote() bool {
	return t.kind == targetKindRemote || t.kind == targetKindFediverse
}

// IsFediverse reports whether this target discovers A2A or ActivityPub through the private broker.
func (t Target) IsFediverse() bool { return t.kind == targetKindFediverse }

// ID returns a stable, opaque cache and mapping identity. Remote token-budget changes produce a
// new ID, while IdentityFingerprint remains stable.
func (t Target) ID() string {
	return t.id
}

// IdentityFingerprint returns the stable endpoint-and-card-identity fingerprint.
func (t Target) IdentityFingerprint() [sha256.Size]byte {
	return t.identityFingerprint
}

// SameIdentity reports whether two targets address the same endpoint under the same pinned
// identity. Operational knobs such as the remote token budget are deliberately excluded.
func (t Target) SameIdentity(other Target) bool {
	return t.valid() && other.valid() && t.kind == other.kind && t.identityFingerprint == other.identityFingerprint
}

// TokenBudget returns the partner-enforced maximum token budget configured for a remote target.
// Local targets return zero because their model budgets are governed by the local gateway.
func (t Target) TokenBudget() uint64 {
	return t.tokenBudget
}

// ActivatedExtensions returns the ordered A2A extension URIs the bridge requests activation for on
// a remote delegation: the always-on token-budget base contract followed by the operator-configured
// extras. The remote server ultimately activates only those it also advertises. Local targets
// activate none — their model budget and capabilities are governed by the local gateway.
func (t Target) ActivatedExtensions() []string {
	if !t.IsRemote() {
		return nil
	}
	activated := make([]string, 0, len(t.extensions)+1)
	activated = append(activated, TokenBudgetExtensionURI)
	activated = append(activated, t.extensions...)
	return activated
}

// activatesExtension reports whether uri is in this target's activated set. It is the allowlist a
// verified card's `required: true` extensions are checked against before the target is trusted.
func (t Target) activatesExtension(uri string) bool {
	return uri == TokenBudgetExtensionURI || slices.Contains(t.extensions, uri)
}

// requiresClientCert reports whether this remote mapping configured mTLS client-cert material (#244).
func (t Target) requiresClientCert() bool {
	return t.tls != nil
}

// clientTLSConfig returns the *tls.Config the bridge dials this remote target with, or nil when no
// client certificate is configured (the caller then uses the default transport). It presents the
// pinned client certificate and, when configured, restricts server trust to the pinned roots. TLS 1.2
// is the floor (#244).
func (t Target) clientTLSConfig() *tls.Config {
	if t.tls == nil {
		return nil
	}
	return &tls.Config{
		Certificates: []tls.Certificate{t.tls.certificate},
		RootCAs:      t.tls.roots,
		MinVersion:   tls.VersionTLS12,
	}
}

func (t Target) valid() bool {
	return t.id != "" && (t.kind == targetKindLocal || t.kind == targetKindRemote || t.kind == targetKindFediverse)
}

func (t Target) resolvedRemote(endpoint, cardURL string) (Target, error) {
	if !t.IsFediverse() {
		return Target{}, fmt.Errorf("target is not a fediverse mapping")
	}
	endpoint, err := NormalizeRemoteURL(endpoint)
	if err != nil {
		return Target{}, err
	}
	cardURL, err = NormalizeRemoteURL(cardURL)
	if err != nil || !strings.HasPrefix(cardURL, "https://") {
		return Target{}, fmt.Errorf("discovered AgentCard URL must be canonical https")
	}
	resolved := t
	resolved.kind = targetKindRemote
	resolved.address = endpoint
	resolved.cardURL = cardURL
	resolved.activityPubIdentity = ActivityPubIdentity{}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%x\x00%s\x00%s", t.identityFingerprint, endpoint, cardURL)))
	resolved.identityFingerprint = fingerprint
	id := sha256.Sum256([]byte(fmt.Sprintf("%x\x00%d\x00%s", fingerprint, t.tokenBudget, strings.Join(t.extensions, "\x1f"))))
	resolved.id = "remote:" + hex.EncodeToString(id[:])
	return resolved, nil
}

func normalizeAcctHandle(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || !strings.HasPrefix(raw, "acct:") {
		return "", fmt.Errorf("fediverse handle must use acct:<local>@<domain>")
	}
	rest := strings.TrimPrefix(raw, "acct:")
	local, domain, ok := strings.Cut(rest, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("fediverse handle must use acct:<local>@<domain>")
	}
	if local != url.PathEscape(local) || strings.ContainsAny(local, "/:#? ") {
		return "", fmt.Errorf("fediverse handle localpart is invalid")
	}
	if domain != strings.ToLower(domain) || !validAcctDomain(domain) {
		return "", fmt.Errorf("fediverse handle domain must be a lowercase canonical hostname")
	}
	return raw, nil
}

func validAcctDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for label := range strings.SplitSeq(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

// pinnedKeys reconstructs the currently-valid ES256 public keys (primary + overlap) for card
// verification (#352). A pin whose stored point fails to parse is skipped defensively; NewRemoteTarget
// already validated every pin, and VerifySet fails closed if the resulting set is empty.
func (t Target) pinnedKeys() []agentcardjws.PinnedKey {
	keys := make([]agentcardjws.PinnedKey, 0, len(t.pins))
	for _, pin := range t.pins {
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pin.publicKey[:])
		if err != nil {
			continue
		}
		keys = append(keys, agentcardjws.PinnedKey{KeyID: pin.keyID, Key: key})
	}
	return keys
}

// revoked returns the set of retired key IDs a card must not be trusted under (#352), or nil if none.
func (t Target) revoked() map[string]bool {
	if len(t.revokedKeyIDs) == 0 {
		return nil
	}
	set := make(map[string]bool, len(t.revokedKeyIDs))
	for _, keyID := range t.revokedKeyIDs {
		set[keyID] = true
	}
	return set
}

// targetPin is one pinned signing key: its protected key ID and the SEC1-uncompressed P-256 point.
type targetPin struct {
	keyID     string
	publicKey [65]byte
}

// normalizeRevokedKeyIDs validates, dedupes, and sorts the retired key IDs, failing closed if any is
// also a currently-pinned key (a key id cannot be simultaneously trusted and revoked).
func normalizeRevokedKeyIDs(revoked []string, pinnedKeyIDs map[string]struct{}) ([]string, error) {
	if len(revoked) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(revoked))
	normalized := make([]string, 0, len(revoked))
	for _, keyID := range revoked {
		if keyID == "" || keyID != strings.TrimSpace(keyID) {
			return nil, fmt.Errorf("remote card identity revoked key ID must not be empty")
		}
		if _, pinned := pinnedKeyIDs[keyID]; pinned {
			return nil, fmt.Errorf("remote card identity key ID %q is both pinned and revoked", keyID)
		}
		if _, dup := seen[keyID]; dup {
			continue
		}
		seen[keyID] = struct{}{}
		normalized = append(normalized, keyID)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func normalizeLocalPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("local agent path must not be empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse local agent path %q: %w", raw, err)
	}
	if parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("local agent path %q must be an absolute path without authority, query, or fragment", raw)
	}
	if !strings.HasPrefix(parsed.Path, "/") || parsed.Path == "/" {
		return "", fmt.Errorf("local agent path %q must start with / and identify an agent", raw)
	}
	cleaned := path.Clean(parsed.Path)
	if cleaned != parsed.Path {
		return "", fmt.Errorf("local agent path %q is not canonical", raw)
	}
	return cleaned, nil
}

// NormalizeRemoteURL validates one exact remote A2A endpoint. Cleartext HTTP is restricted to
// loopback development hosts and DNS-valid Kubernetes service names.
func NormalizeRemoteURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("remote agent URL must be non-empty without surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse remote agent URL %q: %w", raw, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("remote agent URL %q must use http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("remote agent URL %q must be absolute", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("remote agent URL %q must not contain credentials, a query, or a fragment", raw)
	}
	if parsed.Opaque != "" || parsed.RawPath != "" {
		return "", fmt.Errorf("remote agent URL %q must use a canonical hierarchical path", raw)
	}
	if parsed.Scheme == "http" && !isAllowedCleartextHost(parsed.Hostname()) {
		return "", fmt.Errorf(
			"remote agent URL %q may use http only for loopback, .localhost, or DNS-valid Kubernetes service hosts",
			raw,
		)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if path.Clean(parsed.Path) != parsed.Path {
		return "", fmt.Errorf("remote agent URL %q path is not canonical", raw)
	}
	normalized := strings.TrimRight(parsed.String(), "/")
	if normalized == parsed.Scheme+":/" {
		return "", fmt.Errorf("remote agent URL %q must identify a host", raw)
	}
	if normalized != raw {
		return "", fmt.Errorf("remote agent URL %q must be canonical without a trailing slash", raw)
	}
	return normalized, nil
}

func isAllowedCleartextHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	if strings.Contains(host, ":") {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	prefix := host
	switch {
	case strings.HasSuffix(host, ".svc.cluster.local"):
		prefix = strings.TrimSuffix(host, ".svc.cluster.local")
	case strings.HasSuffix(host, ".svc"):
		prefix = strings.TrimSuffix(host, ".svc")
	case strings.Contains(host, "."):
		return false
	}
	if prefix == "" {
		return false
	}
	for _, label := range strings.Split(prefix, ".") {
		if len(label) > 63 || !kubernetesDNSLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

var (
	kubernetesDNSLabelRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]*[a-z0-9])?$`)
	multikeyRE           = regexp.MustCompile(`^z[1-9A-HJ-NP-Za-km-z]{40,60}$`)
)
