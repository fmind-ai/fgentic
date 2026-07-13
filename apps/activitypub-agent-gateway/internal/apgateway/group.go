package apgateway

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// GroupRef designates one Matrix collaboration room exposed as an ActivityPub Group actor (issue
// #217). Remote Fediverse actors follow and post to the Group; an @agent mention inside it routes
// through the governed A2A path — cross-org agent collaboration without requiring the remote side to
// run Matrix federation at all. Membership and origin checks key on FULL actor URIs, never localparts,
// so the design never assumes a single homeserver (docs/fediverse.md standing rule).
type GroupRef struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// GroupRegistry is the immutable set of exposed collaboration groups, keyed by group id.
type GroupRegistry struct {
	groups map[string]GroupRef
}

type groupsFile struct {
	SchemaVersion int                 `yaml:"schemaVersion"`
	Groups        map[string]GroupRef `yaml:"groups"`
}

// LoadGroupRegistry parses the projected groups.yaml, failing fast. A group id is a bare localpart
// (no scheme, path, or prefix): it becomes the actor at /ap/groups/<id> and the handle
// acct:<id>@<server>.
func LoadGroupRegistry(path string) (*GroupRegistry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read groups file %s: %w", path, err)
	}
	var parsed groupsFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse groups file %s: %w", path, err)
	}
	if parsed.SchemaVersion != 1 {
		return nil, fmt.Errorf("groups file %s: unsupported schemaVersion %d (want 1)", path, parsed.SchemaVersion)
	}
	if len(parsed.Groups) == 0 {
		return nil, fmt.Errorf("groups file %s: at least one group must be declared", path)
	}
	groups := make(map[string]GroupRef, len(parsed.Groups))
	for id, ref := range parsed.Groups {
		if id == "" || id != strings.TrimSpace(id) || strings.ContainsAny(id, "/ @:") {
			return nil, fmt.Errorf("groups file %s: group id %q has invalid characters", path, id)
		}
		if ref.Name == "" || ref.Description == "" {
			return nil, fmt.Errorf("groups file %s: group %q must set name and description", path, id)
		}
		groups[id] = ref
	}
	return &GroupRegistry{groups: groups}, nil
}

// Lookup returns a group's metadata and whether it is served.
func (r *GroupRegistry) Lookup(id string) (GroupRef, bool) {
	ref, ok := r.groups[id]
	return ref, ok
}

// Groups lists the served group ids in stable, sorted order.
func (r *GroupRegistry) Groups() []string {
	ids := make([]string, 0, len(r.groups))
	for id := range r.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// followerStore records, per group, each follower's actor URI and its resolved inbox. It is
// deliberately in-memory for this milestone (durable membership lands with the proactive-agents
// Postgres work, issue #237). Keying by actor URI means a follower is identified by its full sovereign
// address, never a localpart.
type followerStore struct {
	mu      sync.Mutex
	byGroup map[string]map[string]string // group -> actorURI -> inbox
}

func newFollowerStore() *followerStore {
	return &followerStore{byGroup: make(map[string]map[string]string)}
}

// add records (or refreshes) a follower's inbox for a group.
func (s *followerStore) add(group, actorURI, inbox string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.byGroup[group]
	if set == nil {
		set = make(map[string]string)
		s.byGroup[group] = set
	}
	set[actorURI] = inbox
}

// remove drops a follower from a group (Undo Follow).
func (s *followerStore) remove(group, actorURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if set := s.byGroup[group]; set != nil {
		delete(set, actorURI)
	}
}

// inboxes returns every follower inbox for a group, minus the excluded actor (never echo a post back
// to its own author). Order is stable for deterministic fan-out.
func (s *followerStore) inboxes(group, exclude string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.byGroup[group]
	inboxes := make([]string, 0, len(set))
	for actor, inbox := range set {
		if actor == exclude {
			continue
		}
		inboxes = append(inboxes, inbox)
	}
	sort.Strings(inboxes)
	return inboxes
}

// count returns the number of followers of a group.
func (s *followerStore) count(group string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byGroup[group])
}
