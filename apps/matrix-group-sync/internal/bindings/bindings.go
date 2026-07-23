// Package bindings parses the git-declared, exact Keycloak-group -> managed-room bindings that are
// the sole source of which rooms this reconciler manages (docs/adr/0009). One binding = one access
// bundle: an exact IdP group path, one managed Matrix room, and the explicit set of agent ghosts
// that room is bound to. A room absent from this file is UNMANAGED and can never be granted into.
package bindings

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Binding maps one exact Keycloak group path to one managed Matrix room and its explicit agent set.
type Binding struct {
	// Group is the exact, unambiguous full Keycloak group path (e.g. /fgentic/agent-access/platform).
	// The leading '/' is the path root and each segment is non-empty; a group name may not contain
	// '/' (the separator), so the path is unambiguous (docs/identity.md).
	Group string `yaml:"group"`
	// RoomAlias is the managed room's full alias `#name:server` — always server-qualified so the
	// design never assumes a single homeserver.
	RoomAlias string `yaml:"roomAlias"`
	// Agents is the explicit set of ghosts bound to this room (recorded for audit + bridge
	// authorization). Membership authorizes invocation of exactly these agents in this room.
	Agents []string `yaml:"agents"`
}

// Set is the immutable, validated collection of bindings, stable-ordered by group path.
type Set struct {
	bindings []Binding
}

type bindingsFile struct {
	SchemaVersion int       `yaml:"schemaVersion"`
	Bindings      []Binding `yaml:"bindings"`
}

// Load parses and validates the projected bindings.yaml, failing fast on any malformed entry,
// duplicate group, or duplicate room. Parsing into a trusted type at the boundary keeps every later
// stage free of ambiguous configuration.
func Load(path, ghostPrefix string) (*Set, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bindings file %s: %w", path, err)
	}
	var parsed bindingsFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject unknown fields
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse bindings file %s: %w", path, err)
	}
	if parsed.SchemaVersion != 1 {
		return nil, fmt.Errorf("bindings file %s: unsupported schemaVersion %d (want 1)", path, parsed.SchemaVersion)
	}
	if len(parsed.Bindings) == 0 {
		return nil, fmt.Errorf("bindings file %s: at least one binding must be declared", path)
	}
	seenGroup := make(map[string]struct{}, len(parsed.Bindings))
	seenRoom := make(map[string]struct{}, len(parsed.Bindings))
	out := make([]Binding, 0, len(parsed.Bindings))
	for i, b := range parsed.Bindings {
		if err := validateGroupPath(b.Group); err != nil {
			return nil, fmt.Errorf("bindings file %s: binding %d: %w", path, i, err)
		}
		if err := validateRoomAlias(b.RoomAlias); err != nil {
			return nil, fmt.Errorf("bindings file %s: binding %d: %w", path, i, err)
		}
		if len(b.Agents) == 0 {
			return nil, fmt.Errorf("bindings file %s: binding %d (%s): at least one agent must be bound", path, i, b.Group)
		}
		for _, a := range b.Agents {
			if !strings.HasPrefix(a, ghostPrefix) || a == ghostPrefix || strings.ContainsAny(a, "/ @:") {
				return nil, fmt.Errorf("bindings file %s: binding %d (%s): agent %q must start with %q and carry a name", path, i, b.Group, a, ghostPrefix)
			}
		}
		if _, dup := seenGroup[b.Group]; dup {
			return nil, fmt.Errorf("bindings file %s: duplicate group %q (one binding per group)", path, b.Group)
		}
		if _, dup := seenRoom[b.RoomAlias]; dup {
			return nil, fmt.Errorf("bindings file %s: duplicate roomAlias %q (one binding per room)", path, b.RoomAlias)
		}
		seenGroup[b.Group] = struct{}{}
		seenRoom[b.RoomAlias] = struct{}{}
		out = append(out, Binding{Group: b.Group, RoomAlias: b.RoomAlias, Agents: append([]string(nil), b.Agents...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return &Set{bindings: out}, nil
}

// All returns the validated bindings in stable order.
func (s *Set) All() []Binding {
	return s.bindings
}

// Groups returns the distinct, stable-ordered group paths the reconciler must read from the IdP.
func (s *Set) Groups() []string {
	groups := make([]string, 0, len(s.bindings))
	for _, b := range s.bindings {
		groups = append(groups, b.Group)
	}
	return groups
}

func validateGroupPath(group string) error {
	if group == "" {
		return fmt.Errorf("group must not be empty")
	}
	if group != strings.TrimSpace(group) {
		return fmt.Errorf("group %q must not have surrounding whitespace", group)
	}
	if !strings.HasPrefix(group, "/") {
		return fmt.Errorf("group %q must be an absolute path starting with '/'", group)
	}
	segments := strings.Split(strings.TrimPrefix(group, "/"), "/")
	for _, seg := range segments {
		if seg == "" {
			return fmt.Errorf("group %q must not contain an empty path segment", group)
		}
		if strings.ContainsAny(seg, " \t") {
			return fmt.Errorf("group %q segment %q must not contain whitespace", group, seg)
		}
	}
	return nil
}

func validateRoomAlias(alias string) error {
	if !strings.HasPrefix(alias, "#") {
		return fmt.Errorf("roomAlias %q must start with '#'", alias)
	}
	name, server, ok := strings.Cut(alias[1:], ":")
	if !ok || name == "" || server == "" {
		return fmt.Errorf("roomAlias %q must be a full alias '#name:server'", alias)
	}
	return nil
}
