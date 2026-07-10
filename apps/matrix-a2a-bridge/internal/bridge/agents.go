// Package bridge wires Matrix room events to A2A agent calls: it resolves which agent an
// @mention addresses, checks the sender is authorized, delegates the task over A2A, and posts
// the reply back as the ghost user.
package bridge

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/id"
)

// AgentRef identifies a kagent agent by its namespace and name (its A2A endpoint path) and
// carries the per-agent sender policy (SPEC §4 F6): which homeservers and which users may
// invoke it. The bridge's own server is always allowed; federated senders are deny-by-default.
type AgentRef struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`

	// AllowedServers lists ADDITIONAL homeservers whose users may invoke this agent
	// (the bridge's own server is always allowed). Exact server names, no globs.
	AllowedServers []string `yaml:"allowedServers,omitempty"`
	// AllowedSenders restricts invocation to matching user IDs (glob with '*', e.g.
	// "@ops-*:partner.example"). Empty means: any user on an allowed server.
	AllowedSenders []string `yaml:"allowedSenders,omitempty"`

	senderRes []*regexp.Regexp // compiled AllowedSenders
}

// Path is the A2A endpoint path for this agent, relative to the A2A base URL
// (e.g. "/api/a2a/kagent/k8s-agent" — kagent's controller shape).
func (a *AgentRef) Path() string {
	return fmt.Sprintf("/api/a2a/%s/%s", a.Namespace, a.Name)
}

// AllowsSender reports whether sender may invoke this agent, given the bridge's own server name.
func (a *AgentRef) AllowsSender(sender id.UserID, ownServer string) bool {
	server := sender.Homeserver()
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
		return true
	}
	for _, re := range a.senderRes {
		if re.MatchString(sender.String()) {
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
	return nil
}

// AgentMap is the allowlist mapping a ghost local-part (@agent-k8s -> "agent-k8s") to its agent.
// It doubles as the authorization boundary: only mapped ghosts are invokable, and each mapping
// carries its sender policy.
type AgentMap struct {
	byGhost map[string]*AgentRef
}

// LoadAgents reads the ghost->agent routing map from a YAML file:
//
//	agents:
//	  agent-k8s:
//	    namespace: kagent
//	    name: k8s-agent
//	    allowedServers: [partner.example]        # optional; own server always allowed
//	    allowedSenders: ["@ops-*:partner.example"] # optional; empty = anyone on allowed servers
func LoadAgents(path string) (*AgentMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agents file %q: %w", path, err)
	}
	var doc struct {
		Agents map[string]*AgentRef `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse agents file %q: %w", path, err)
	}
	if len(doc.Agents) == 0 {
		return nil, fmt.Errorf("agents file %q defines no agents under `agents:`", path)
	}
	for ghost, ref := range doc.Agents {
		if ref == nil || ref.Namespace == "" || ref.Name == "" {
			return nil, fmt.Errorf("agent %q: both namespace and name are required", ghost)
		}
		if err := ref.compileSenders(ghost); err != nil {
			return nil, err
		}
	}
	return &AgentMap{byGhost: doc.Agents}, nil
}

// Lookup returns the agent mapped to a ghost local-part, if any.
func (am *AgentMap) Lookup(ghostLocalpart string) (*AgentRef, bool) {
	ref, ok := am.byGhost[ghostLocalpart]
	return ref, ok
}

// Names returns the sorted list of mapped ghost local-parts (for logging).
func (am *AgentMap) Names() []string {
	names := make([]string, 0, len(am.byGhost))
	for k := range am.byGhost {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
