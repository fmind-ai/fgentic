package apgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestLoadGroupRegistry(t *testing.T) {
	reg, err := LoadGroupRegistry(writeGroups(t, validGroups))
	if err != nil {
		t.Fatalf("LoadGroupRegistry: %v", err)
	}
	if got := reg.Groups(); len(got) != 1 || got[0] != "collab" {
		t.Errorf("Groups() = %v", got)
	}
	if _, ok := reg.Lookup("collab"); !ok {
		t.Errorf("collab must resolve")
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Errorf("unknown group must not resolve")
	}
}

func TestLoadGroupRegistryRejects(t *testing.T) {
	cases := map[string]string{
		"bad version":   `schemaVersion: 2`,
		"empty":         "schemaVersion: 1",
		"bad id":        "schemaVersion: 1\ngroups:\n  \"a/b\":\n    name: x\n    description: y\n",
		"missing name":  "schemaVersion: 1\ngroups:\n  collab:\n    description: y\n",
		"unknown field": "schemaVersion: 1\ngroups:\n  collab:\n    name: x\n    description: y\n    bogus: z\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadGroupRegistry(writeGroups(t, body)); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
	if _, err := LoadGroupRegistry("/no/such/groups.yaml"); err == nil {
		t.Errorf("missing file must error")
	}
}

func TestGroupOutboxAndFollowers(t *testing.T) {
	g := newGroupGateway(t, &fakeDelegator{}, http.DefaultClient, nil)

	out := do(t, g, http.MethodGet, "/ap/groups/collab/outbox", "")
	if out.Code != http.StatusOK {
		t.Fatalf("outbox code = %d", out.Code)
	}
	var oc map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &oc); err != nil {
		t.Fatalf("unmarshal outbox: %v", err)
	}
	if oc["type"] != "OrderedCollection" {
		t.Errorf("outbox type = %v", oc["type"])
	}

	// The followers collection is content-free: it exposes a count, never member URIs.
	g.followers.add("collab", "https://m.example/users/x", "https://m.example/users/x/inbox")
	fol := do(t, g, http.MethodGet, "/ap/groups/collab/followers", "")
	var fc map[string]any
	if err := json.Unmarshal(fol.Body.Bytes(), &fc); err != nil {
		t.Fatalf("unmarshal followers: %v", err)
	}
	if fc["totalItems"] != float64(1) {
		t.Errorf("followers totalItems = %v, want 1", fc["totalItems"])
	}
	if items := fc["orderedItems"].([]any); len(items) != 0 {
		t.Errorf("followers must not leak member URIs, got %v", items)
	}
}

func TestGroupUndoRemovesFollower(t *testing.T) {
	g := newGroupGateway(t, &fakeDelegator{}, http.DefaultClient, nil)
	g.followers.add("collab", "https://m.example/users/x", "https://m.example/users/x/inbox")

	undo := fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","type":"Undo","actor":%q,"object":{"type":"Follow","actor":%q,"object":"https://fgentic.localhost/ap/groups/collab"}}`,
		"https://m.example/users/x", "https://m.example/users/x")
	if rec := do(t, g, http.MethodPost, "/ap/groups/collab/inbox", undo); rec.Code != http.StatusAccepted {
		t.Fatalf("undo code = %d", rec.Code)
	}
	if g.followers.count("collab") != 0 {
		t.Errorf("Undo(Follow) must remove the follower")
	}
}

func TestGroupUnknownAnd404WhenDisabled(t *testing.T) {
	// Unknown group on a groups-enabled gateway.
	g := newGroupGateway(t, &fakeDelegator{}, http.DefaultClient, nil)
	if rec := do(t, g, http.MethodGet, "/ap/groups/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown group code = %d, want 404", rec.Code)
	}

	// Groups disabled: every group route 404s.
	plain := newTestGateway(t, &fakeDelegator{})
	for _, path := range []string{"/ap/groups/collab", "/ap/groups/collab/outbox", "/ap/groups/collab/followers"} {
		if rec := do(t, plain, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s on groups-disabled gateway = %d, want 404", path, rec.Code)
		}
	}
	if rec := do(t, plain, http.MethodPost, "/ap/groups/collab/inbox", "{}"); rec.Code != http.StatusNotFound {
		t.Errorf("group inbox on groups-disabled gateway = %d, want 404", rec.Code)
	}
}
