// Package bridge wires Matrix room events to A2A agent calls: it resolves which agent an
// @mention addresses, checks the sender is authorized, delegates the task over A2A, and posts
// the reply back as the ghost user.
package bridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
)

const agentsSchemaVersion = 1

// AgentRef identifies one immutable local or remote A2A target and carries the per-agent sender
// policy (SPEC §4 F6): which homeservers and which users may invoke it. The bridge's own server
// is always allowed for native Matrix senders; federated and configured bridged senders are
// deny-by-default.
type AgentRef struct {
	Namespace   string `yaml:"namespace"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	AvatarURL   string `yaml:"avatarURL,omitempty"`

	// AllowedServers lists ADDITIONAL homeservers whose users may invoke this agent
	// (the bridge's own server is always allowed). Exact server names, no globs.
	AllowedServers []string `yaml:"allowedServers,omitempty"`
	// AllowedSenders restricts invocation to matching user IDs (glob with '*', e.g.
	// "@ops-*:partner.example"). Empty means: any user on an allowed server.
	AllowedSenders []string `yaml:"allowedSenders,omitempty"`

	senderRes []*regexp.Regexp // compiled AllowedSenders
	avatar    id.ContentURI
	target    a2aclient.Target
	timeout   time.Duration
	maxCost   uint64 // per-remote credit-unit ceiling on the verified skill quote (0 = no gate)
	dev       bool   // stage:dev — invocable only in configured staging rooms (#128)
	mappingID string
}

// agentConfig is the on-disk shape. Runtime code only receives an AgentRef containing a
// validated immutable a2aclient.Target, so later mutation of decoded YAML cannot change where
// a queued delegation is sent.
type agentConfig struct {
	Namespace   *string `yaml:"namespace,omitempty"`
	Name        *string `yaml:"name,omitempty"`
	URL         *string `yaml:"url,omitempty"`
	Description string  `yaml:"description,omitempty"`
	AvatarURL   string  `yaml:"avatarURL,omitempty"`
	// Stage gates blast radius (#128): `dev` agents are invocable only in the bridge's configured
	// staging rooms; `prod` (the default) is unrestricted. Valid for both local and remote targets.
	Stage *string `yaml:"stage,omitempty"`

	Timeout      *time.Duration      `yaml:"timeout,omitempty"`
	TokenBudget  *uint64             `yaml:"tokenBudget,omitempty"`
	CardIdentity *cardIdentityConfig `yaml:"cardIdentity,omitempty"`
	// Extensions lists additional A2A extension URIs to activate on top of the always-on
	// token-budget contract, and doubles as the allowlist of `required: true` card extensions the
	// bridge will accept (docs/bridge.md §6). Remote targets only.
	Extensions []string `yaml:"extensions,omitempty"`
	// MaxCost is the highest per-skill credit-unit price the bridge will pay this remote. When set,
	// the verified card must advertise a skill quote within it or the delegation fails closed with
	// quote_over_budget (docs/federation.md). Remote targets only; omitted means no cost gate.
	MaxCost *uint64 `yaml:"maxCost,omitempty"`

	AllowedServers []string `yaml:"allowedServers,omitempty"`
	AllowedSenders []string `yaml:"allowedSenders,omitempty"`
}

type cardIdentityConfig struct {
	Name         string             `yaml:"name"`
	Organization string             `yaml:"organization"`
	KeyID        string             `yaml:"keyID"`
	PublicKey    *ecPublicKeyConfig `yaml:"publicKey"`
}

// ecPublicKeyConfig intentionally accepts only the public RFC 7517 members needed for ES256.
// Algorithm policy is fixed by the bridge instead of being operator-controlled YAML.
type ecPublicKeyConfig struct {
	KeyType string `json:"kty" yaml:"kty"`
	Curve   string `json:"crv" yaml:"crv"`
	X       string `json:"x" yaml:"x"`
	Y       string `json:"y" yaml:"y"`
}

// Path returns the local A2A path or exact remote endpoint. New dispatch code should pass
// Target() to the A2A client; Path remains the profile/logging compatibility view.
func (a *AgentRef) Path() string {
	return a.target.String()
}

// Target returns the immutable, validated routing and trust policy for this mapping.
func (a *AgentRef) Target() a2aclient.Target {
	return a.target
}

// Timeout returns the per-request timeout for a remote mapping. Local mappings return zero and
// continue to use the bridge-wide request/task timeouts.
func (a *AgentRef) Timeout() time.Duration {
	return a.timeout
}

// MaxCost returns the per-remote credit-unit ceiling enforced against the verified skill quote, or
// zero when no cost gate is configured (docs/federation.md).
func (a *AgentRef) MaxCost() uint64 {
	return a.maxCost
}

// IsDev reports whether this agent is staged `dev` and therefore invocable only in the bridge's
// configured staging rooms (#128). The default (unset) stage is `prod`, which is unrestricted.
func (a *AgentRef) IsDev() bool {
	return a.dev
}

// MappingID binds a mapping to its route, pinned signer, token budget, and timeout. Profile
// caches can use it to avoid carrying metadata across a same-URL signer change.
func (a *AgentRef) MappingID() string {
	return a.mappingID
}

// SameTarget protects queued work from configuration reloads. A change to any routing, trust,
// budget, or timeout input makes the queued reference stale and therefore non-dispatchable.
func (a *AgentRef) SameTarget(other *AgentRef) bool {
	return a != nil && other != nil && a.mappingID == other.mappingID &&
		a.target.SameIdentity(other.target)
}

// Avatar returns the validated optional Matrix Content URI configured for this ghost.
func (a *AgentRef) Avatar() id.ContentURI {
	return a.avatar
}

// AllowsSender reports whether a classified sender may invoke this agent. A bridge-derived
// identity always requires an explicit full-MXID allowedSenders match, even on the local server.
func (a *AgentRef) AllowsSender(sender senderIdentity, ownServer string) bool {
	server := sender.mxid.Homeserver()
	if server != ownServer {
		allowed := false
		for _, s := range a.AllowedServers {
			if s == server {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if len(a.senderRes) == 0 {
		return !sender.isBridged()
	}
	for _, re := range a.senderRes {
		if re.MatchString(sender.mxid.String()) {
			return true
		}
	}
	return false
}

// compileSenders turns the '*' globs into anchored regexps, failing fast on bad patterns.
func (a *AgentRef) compileSenders(ghost string) error {
	a.senderRes = make([]*regexp.Regexp, 0, len(a.AllowedSenders))
	for _, pattern := range a.AllowedSenders {
		parts := strings.Split(pattern, "*")
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
		if err != nil {
			return fmt.Errorf("agent %q: bad allowedSenders pattern %q: %w", ghost, pattern, err)
		}
		a.senderRes = append(a.senderRes, re)
	}
	if a.AvatarURL != "" {
		avatar, err := id.ParseContentURI(a.AvatarURL)
		if err != nil {
			return fmt.Errorf("agent %q: avatarURL must be an mxc:// URI: %w", ghost, err)
		}
		a.avatar = avatar
	}
	return nil
}

func compileAgent(ghost string, cfg *agentConfig) (*AgentRef, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent %q: target configuration must not be null", ghost)
	}
	if err := id.ValidateUserLocalpart(ghost); err != nil {
		return nil, fmt.Errorf("agent %q: ghost must be a valid Matrix user localpart: %w", ghost, err)
	}
	if _, _, err := id.NewUserID(ghost, "example.invalid").ParseAndValidateStrict(); err != nil {
		return nil, fmt.Errorf("agent %q: ghost must be a valid Matrix user localpart: %w", ghost, err)
	}

	hasLocal := cfg.Namespace != nil || cfg.Name != nil
	hasRemote := cfg.URL != nil
	if hasLocal == hasRemote {
		return nil, fmt.Errorf("agent %q: exactly one target form is required: namespace+name or url", ghost)
	}

	namespace := configuredString(cfg.Namespace)
	name := configuredString(cfg.Name)
	ref := &AgentRef{
		Namespace:      namespace,
		Name:           name,
		Description:    cfg.Description,
		AvatarURL:      cfg.AvatarURL,
		AllowedServers: append([]string(nil), cfg.AllowedServers...),
		AllowedSenders: append([]string(nil), cfg.AllowedSenders...),
	}

	var err error
	if hasLocal {
		if namespace == "" || name == "" {
			return nil, fmt.Errorf("agent %q: both namespace and name are required for a local target", ghost)
		}
		if cfg.Timeout != nil || cfg.TokenBudget != nil || cfg.CardIdentity != nil || len(cfg.Extensions) > 0 || cfg.MaxCost != nil {
			return nil, fmt.Errorf("agent %q: timeout, tokenBudget, cardIdentity, extensions, and maxCost are only valid for a url target", ghost)
		}
		ref.target, err = a2aclient.NewLocalTarget(fmt.Sprintf("/api/a2a/%s/%s", namespace, name))
	} else {
		ref.target, ref.timeout, err = compileRemoteTarget(cfg)
		if cfg.MaxCost != nil {
			if *cfg.MaxCost == 0 {
				return nil, fmt.Errorf("agent %q: maxCost must be positive when set", ghost)
			}
			ref.maxCost = *cfg.MaxCost
		}
		// The expected signed-card name is the safe profile fallback before discovery succeeds.
		if cfg.CardIdentity != nil {
			ref.Name = cfg.CardIdentity.Name
		}
	}
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", ghost, err)
	}
	if ref.dev, err = compileStage(ghost, cfg); err != nil {
		return nil, err
	}
	if err := ref.compileSenders(ghost); err != nil {
		return nil, err
	}
	ref.mappingID = mappingID(ref.target, ref.timeout, ref.maxCost, ref.dev)
	return ref, nil
}

// compileStage validates the optional per-agent stage flag. The default (unset) is `prod`.
func compileStage(ghost string, cfg *agentConfig) (bool, error) {
	if cfg.Stage == nil {
		return false, nil
	}
	switch *cfg.Stage {
	case "prod":
		return false, nil
	case "dev":
		return true, nil
	default:
		return false, fmt.Errorf("agent %q: stage must be \"dev\" or \"prod\", got %q", ghost, *cfg.Stage)
	}
}

func compileRemoteTarget(cfg *agentConfig) (a2aclient.Target, time.Duration, error) {
	if cfg.Namespace != nil || cfg.Name != nil {
		return a2aclient.Target{}, 0, fmt.Errorf("url target must not define namespace or name")
	}
	rawURL := configuredString(cfg.URL)
	if cfg.Timeout == nil || *cfg.Timeout <= 0 {
		return a2aclient.Target{}, 0, fmt.Errorf("url target timeout must be positive")
	}
	if cfg.TokenBudget == nil || *cfg.TokenBudget == 0 {
		return a2aclient.Target{}, 0, fmt.Errorf("url target tokenBudget must be positive")
	}
	identity, err := compileCardIdentity(cfg.CardIdentity)
	if err != nil {
		return a2aclient.Target{}, 0, err
	}
	target, err := a2aclient.NewRemoteTarget(rawURL, identity, *cfg.TokenBudget, cfg.Extensions)
	if err != nil {
		return a2aclient.Target{}, 0, err
	}
	return target, *cfg.Timeout, nil
}

func configuredString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func compileCardIdentity(cfg *cardIdentityConfig) (a2aclient.CardIdentity, error) {
	if cfg == nil {
		return a2aclient.CardIdentity{}, fmt.Errorf("url target cardIdentity is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "name", value: cfg.Name},
		{name: "organization", value: cfg.Organization},
		{name: "keyID", value: cfg.KeyID},
	} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return a2aclient.CardIdentity{}, fmt.Errorf("url target cardIdentity.%s must be non-empty without surrounding whitespace", field.name)
		}
	}
	if cfg.PublicKey == nil {
		return a2aclient.CardIdentity{}, fmt.Errorf("url target cardIdentity.publicKey is required")
	}
	publicKey, err := json.Marshal(cfg.PublicKey)
	if err != nil {
		return a2aclient.CardIdentity{}, fmt.Errorf("encode url target cardIdentity.publicKey: %w", err)
	}
	return a2aclient.CardIdentity{
		Name:         cfg.Name,
		Organization: cfg.Organization,
		KeyID:        cfg.KeyID,
		PublicKeyJWK: string(publicKey),
	}, nil
}

func mappingID(target a2aclient.Target, timeout time.Duration, maxCost uint64, dev bool) string {
	identity := target.IdentityFingerprint()
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00%x\x00%d\x00%d\x00%d\x00%t", target.ID(), identity, target.TokenBudget(), timeout, maxCost, dev,
	)))
	return hex.EncodeToString(sum[:])
}

// AgentMap is the allowlist mapping a ghost local-part (@agent-k8s -> "agent-k8s") to its agent.
// It doubles as the authorization boundary: only mapped ghosts are invokable, and each mapping
// carries its sender policy.
type AgentMap struct {
	mu                    sync.RWMutex
	byGhost               map[string]*AgentRef
	bridgedOrigins        []bridgedOriginRule
	fingerprint           [sha256.Size]byte
	implicitSchemaVersion bool
}

// AgentEntry is one immutable snapshot entry from an AgentMap.
type AgentEntry struct {
	Ghost string
	Ref   *AgentRef
}

// LoadAgents reads the ghost->agent routing map from a YAML file:
//
//	schemaVersion: 1
//	bridgedOrigins:
//	  slack: ["@slack_*:fgentic.fmind.ai"] # anchored full-MXID bridge namespaces
//	agents:
//	  agent-k8s:
//	    namespace: kagent
//	    name: k8s-agent
//	    description: Startup fallback while the AgentCard is unavailable
//	    avatarURL: mxc://fgentic.fmind.ai/media-id # optional existing Matrix media
//	    allowedServers: [partner.example]        # optional; own server always allowed
//	    allowedSenders: ["@ops-*:partner.example"] # optional; empty = anyone on allowed servers
func LoadAgents(path string) (*AgentMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agents file %q: %w", path, err)
	}
	var doc struct {
		SchemaVersion  yaml.Node               `yaml:"schemaVersion,omitempty"`
		BridgedOrigins map[string][]string     `yaml:"bridgedOrigins,omitempty"`
		Agents         map[string]*agentConfig `yaml:"agents"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse agents file %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("parse agents file %q: %w", path, err)
	}
	implicitSchemaVersion := doc.SchemaVersion.Kind == 0
	var schemaVersion int
	if !implicitSchemaVersion {
		if doc.SchemaVersion.Tag != "!!int" {
			return nil, fmt.Errorf("parse agents file %q: schemaVersion must be an integer", path)
		}
		if err := doc.SchemaVersion.Decode(&schemaVersion); err != nil {
			return nil, fmt.Errorf("parse agents file %q: decode schemaVersion: %w", path, err)
		}
	}
	if !implicitSchemaVersion && schemaVersion != agentsSchemaVersion {
		return nil, fmt.Errorf(
			"parse agents file %q: unsupported schemaVersion %d (supported: %d)",
			path, schemaVersion, agentsSchemaVersion,
		)
	}
	if len(doc.Agents) == 0 {
		return nil, fmt.Errorf("agents file %q defines no agents under `agents:`", path)
	}
	origins, err := compileBridgedOrigins(doc.BridgedOrigins)
	if err != nil {
		return nil, fmt.Errorf("parse agents file %q: %w", path, err)
	}
	refs := make(map[string]*AgentRef, len(doc.Agents))
	for ghost, cfg := range doc.Agents {
		ref, err := compileAgent(ghost, cfg)
		if err != nil {
			return nil, err
		}
		refs[ghost] = ref
	}
	return &AgentMap{
		byGhost:               refs,
		bridgedOrigins:        origins,
		fingerprint:           sha256.Sum256(data),
		implicitSchemaVersion: implicitSchemaVersion,
	}, nil
}

// LogSchemaVersionWarning reports the temporary v1 compatibility path. New files must declare
// schemaVersion explicitly so future major versions can remain fail-closed.
func (am *AgentMap) LogSchemaVersionWarning(log *slog.Logger, path string) {
	if am == nil || !am.implicitSchemaVersion {
		return
	}
	log.Warn(
		"agents config omits schemaVersion; defaulting to v1 is deprecated",
		"path", path,
		"schema_version", agentsSchemaVersion,
	)
}

// IdentifySender classifies one validated event sender against configured, anchored full-MXID
// namespaces. Display names and bare localparts never cross this boundary.
func (am *AgentMap) IdentifySender(mxid id.UserID) senderIdentity {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return identifySender(mxid, am.bridgedOrigins)
}

// SnapshotSenderTarget returns identity classification and routing from one atomic policy
// snapshot, preventing a reload from interleaving the dispatch-time authorization inputs.
func (am *AgentMap) SnapshotSenderTarget(mxid id.UserID, ghostLocalpart string) (senderIdentity, *AgentRef, bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	ref, ok := am.byGhost[ghostLocalpart]
	return identifySender(mxid, am.bridgedOrigins), ref, ok
}

func identifySender(mxid id.UserID, origins []bridgedOriginRule) senderIdentity {
	sender := matrixSender(mxid)
	localpart, _, err := mxid.ParseAndValidateRelaxed()
	if err != nil || !originLocalpartRE.MatchString(localpart) {
		return sender
	}
	for _, rule := range origins {
		if rule.re.MatchString(mxid.String()) {
			sender.origin = rule.origin
			return sender
		}
	}
	return sender
}

// Lookup returns the agent mapped to a ghost local-part, if any.
func (am *AgentMap) Lookup(ghostLocalpart string) (*AgentRef, bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()
	ref, ok := am.byGhost[ghostLocalpart]
	return ref, ok
}

// Names returns the sorted list of mapped ghost local-parts (for logging).
func (am *AgentMap) Names() []string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	names := make([]string, 0, len(am.byGhost))
	for k := range am.byGhost {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Entries returns a stable, ghost-sorted snapshot suitable for profile sync and directory
// rendering while a config reload atomically replaces the live map.
func (am *AgentMap) Entries() []AgentEntry {
	am.mu.RLock()
	defer am.mu.RUnlock()
	entries := make([]AgentEntry, 0, len(am.byGhost))
	for ghost, ref := range am.byGhost {
		entries = append(entries, AgentEntry{Ghost: ghost, Ref: ref})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Ghost < entries[j].Ghost })
	return entries
}

// SameConfig reports whether two maps came from identical file content.
func (am *AgentMap) SameConfig(other *AgentMap) bool {
	am.mu.RLock()
	fingerprint := am.fingerprint
	am.mu.RUnlock()
	other.mu.RLock()
	otherFingerprint := other.fingerprint
	other.mu.RUnlock()
	return fingerprint == otherFingerprint
}

// Replace atomically swaps routing policy while keeping the AgentMap pointer stable for all
// event handlers. AgentRef values are immutable after loading, so snapshots remain safe.
func (am *AgentMap) Replace(other *AgentMap) {
	other.mu.RLock()
	byGhost := other.byGhost
	bridgedOrigins := other.bridgedOrigins
	fingerprint := other.fingerprint
	implicitSchemaVersion := other.implicitSchemaVersion
	other.mu.RUnlock()
	am.mu.Lock()
	am.byGhost = byGhost
	am.bridgedOrigins = bridgedOrigins
	am.fingerprint = fingerprint
	am.implicitSchemaVersion = implicitSchemaVersion
	am.mu.Unlock()
}
