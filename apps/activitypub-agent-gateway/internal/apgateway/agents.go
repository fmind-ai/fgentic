package apgateway

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentRef maps one exposed ghost (agent-<name>) to its local kagent target and a fallback
// description. It is the AP twin of the bridge's agents.yaml entry, minus the Matrix-only sender
// policy: inbound AP sender authorization is the signed-border/allowlist control landed by later
// federation work, so this file only declares WHICH agents are exposed and WHERE they route.
type AgentRef struct {
	Namespace   string `yaml:"namespace"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Registry is the immutable set of ghosts this gateway serves, keyed by full ghost (agent-<name>).
type Registry struct {
	agents map[string]AgentRef
}

type agentsFile struct {
	SchemaVersion int                 `yaml:"schemaVersion"`
	Agents        map[string]AgentRef `yaml:"agents"`
}

// LoadRegistry parses the projected agents.yaml and validates every entry, failing fast. Each key
// must carry ghostPrefix so an AP actor is unambiguously a platform agent and never collides with
// a human localpart (docs/fediverse.md §2).
func LoadRegistry(path, ghostPrefix string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agents file %s: %w", path, err)
	}
	var parsed agentsFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject unknown fields: parse external config into a trusted type
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse agents file %s: %w", path, err)
	}
	if parsed.SchemaVersion != 1 {
		return nil, fmt.Errorf("agents file %s: unsupported schemaVersion %d (want 1)", path, parsed.SchemaVersion)
	}
	if len(parsed.Agents) == 0 {
		return nil, fmt.Errorf("agents file %s: at least one agent must be declared", path)
	}
	agents := make(map[string]AgentRef, len(parsed.Agents))
	for ghost, ref := range parsed.Agents {
		if !strings.HasPrefix(ghost, ghostPrefix) || ghost == ghostPrefix {
			return nil, fmt.Errorf("agents file %s: ghost %q must start with %q and carry a name", path, ghost, ghostPrefix)
		}
		if ghost != strings.TrimSpace(ghost) || strings.ContainsAny(ghost, "/ @:") {
			return nil, fmt.Errorf("agents file %s: ghost %q has invalid characters", path, ghost)
		}
		if ref.Namespace == "" || ref.Name == "" {
			return nil, fmt.Errorf("agents file %s: ghost %q must set namespace and name", path, ghost)
		}
		if ref.Description == "" {
			return nil, fmt.Errorf("agents file %s: ghost %q must set a description", path, ghost)
		}
		agents[ghost] = ref
	}
	return &Registry{agents: agents}, nil
}

// Lookup returns the target for a ghost and whether it is served.
func (r *Registry) Lookup(ghost string) (AgentRef, bool) {
	ref, ok := r.agents[ghost]
	return ref, ok
}

// Ghosts lists the served ghosts in stable, sorted order.
func (r *Registry) Ghosts() []string {
	ghosts := make([]string, 0, len(r.agents))
	for ghost := range r.agents {
		ghosts = append(ghosts, ghost)
	}
	sort.Strings(ghosts)
	return ghosts
}
