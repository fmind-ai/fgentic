package a2aclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/fmind/matrix-a2a-bridge/internal/agentcardjws"
)

// TokenBudgetExtensionURI identifies Fgentic's A2A extension for a caller-supplied,
// partner-enforced maximum token budget on one remote delegation.
const TokenBudgetExtensionURI = "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1"

const maxExactJSONInteger = 1<<53 - 1

type targetKind uint8

const (
	targetKindLocal targetKind = iota + 1
	targetKindRemote
)

// CardIdentity is the operator-pinned identity a remote signed AgentCard must match.
// PublicKeyJWK must contain an ES256 public key (EC, P-256); it is public trust material,
// never a private signing key.
type CardIdentity struct {
	Name         string
	Organization string
	KeyID        string
	PublicKeyJWK string
}

// Target is an immutable, validated A2A routing target. Local targets are paths relative to
// Client's configured A2A base URL. Remote targets bind one exact endpoint to a pinned card
// identity and a per-request token budget.
type Target struct {
	kind                 targetKind
	address              string
	expectedName         string
	expectedOrganization string
	expectedKeyID        string
	publicKey            [65]byte
	tokenBudget          uint64
	identityFingerprint  [sha256.Size]byte
	id                   string
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
// tokenBudget is transmitted through TokenBudgetExtensionURI and must be positive.
func NewRemoteTarget(rawURL string, identity CardIdentity, tokenBudget uint64) (Target, error) {
	endpoint, err := NormalizeRemoteURL(rawURL)
	if err != nil {
		return Target{}, err
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
	publicKey, err := agentcardjws.ParsePublicJWK(
		[]byte(identity.PublicKeyJWK),
		identity.KeyID,
		agentcardjws.AllowOptionalJWKMetadata,
	)
	if err != nil {
		return Target{}, fmt.Errorf("remote card identity public key: %w", err)
	}
	publicKeyBytes, err := publicKey.Bytes()
	if err != nil {
		return Target{}, fmt.Errorf("encode remote card identity public key: %w", err)
	}
	var encoded [65]byte
	copy(encoded[:], publicKeyBytes)

	fingerprintInput := strings.Join([]string{
		"remote",
		endpoint,
		identity.Name,
		identity.Organization,
		identity.KeyID,
		base64.RawURLEncoding.EncodeToString(encoded[:]),
	}, "\x00")
	fingerprint := sha256.Sum256([]byte(fingerprintInput))
	idInput := fmt.Sprintf("%x\x00%d", fingerprint, tokenBudget)
	id := sha256.Sum256([]byte(idInput))

	return Target{
		kind:                 targetKindRemote,
		address:              endpoint,
		expectedName:         identity.Name,
		expectedOrganization: identity.Organization,
		expectedKeyID:        identity.KeyID,
		publicKey:            encoded,
		tokenBudget:          tokenBudget,
		identityFingerprint:  fingerprint,
		id:                   "remote:" + hex.EncodeToString(id[:]),
	}, nil
}

// String returns the local agent path or normalized exact remote endpoint. Targets reject URL
// credentials and query strings, so this value is safe for routing diagnostics.
func (t Target) String() string {
	return t.address
}

// IsRemote reports whether this target requires a verified signed AgentCard before use.
func (t Target) IsRemote() bool {
	return t.kind == targetKindRemote
}

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

func (t Target) valid() bool {
	return t.id != "" && (t.kind == targetKindLocal || t.kind == targetKindRemote)
}

func (t Target) es256PublicKey() *ecdsa.PublicKey {
	if !t.IsRemote() {
		return nil
	}
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), t.publicKey[:])
	if err != nil {
		return nil
	}
	return key
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

var kubernetesDNSLabelRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]*[a-z0-9])?$`)
