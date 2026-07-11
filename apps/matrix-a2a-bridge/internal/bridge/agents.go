// Package bridge wires Matrix room events to A2A agent calls: it resolves which agent an
// @mention addresses, checks the sender is authorized, delegates the task over A2A, and posts
// the reply back as the ghost user.
package bridge

import (
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/id"
)

// AgentRef identifies a kagent agent by its namespace and name (its A2A endpoint path) and
// carries the per-agent sender policy (SPEC §4 F6): which homeservers and which users may
// invoke it. The bridge's own server is always allowed for native Matrix senders; federated and
// configured bridged senders are deny-by-default.
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
}

// Path is the A2A endpoint path for this agent, relative to the A2A base URL
// (e.g. "/api/a2a/kagent/k8s-agent" — kagent's controller shape).
func (a *AgentRef) Path() string {
	return fmt.Sprintf("/api/a2a/%s/%s", a.Namespace, a.Name)
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

// AgentMap is the allowlist mapping a ghost local-part (@agent-k8s -> "agent-k8s") to its agent.
// It doubles as the authorization boundary: only mapped ghosts are invokable, and each mapping
// carries its sender policy.
type AgentMap struct {
	mu             sync.RWMutex
	byGhost        map[string]*AgentRef
	bridgedOrigins []bridgedOriginRule
	fingerprint    [sha256.Size]byte
}

// AgentEntry is one immutable snapshot entry from an AgentMap.
type AgentEntry struct {
	Ghost string
	Ref   *AgentRef
}

// LoadAgents reads the ghost->agent routing map from a YAML file:
//
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
		BridgedOrigins map[string][]string  `yaml:"bridgedOrigins,omitempty"`
		Agents         map[string]*AgentRef `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse agents file %q: %w", path, err)
	}
	if len(doc.Agents) == 0 {
		return nil, fmt.Errorf("agents file %q defines no agents under `agents:`", path)
	}
	origins, err := compileBridgedOrigins(doc.BridgedOrigins)
	if err != nil {
		return nil, fmt.Errorf("parse agents file %q: %w", path, err)
	}
	for ghost, ref := range doc.Agents {
		if ref == nil || ref.Namespace == "" || ref.Name == "" {
			return nil, fmt.Errorf("agent %q: both namespace and name are required", ghost)
		}
		if err := ref.compileSenders(ghost); err != nil {
			return nil, err
		}
	}
	return &AgentMap{
		byGhost:        doc.Agents,
		bridgedOrigins: origins,
		fingerprint:    sha256.Sum256(data),
	}, nil
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
	other.mu.RUnlock()
	am.mu.Lock()
	am.byGhost = byGhost
	am.bridgedOrigins = bridgedOrigins
	am.fingerprint = fingerprint
	am.mu.Unlock()
}
