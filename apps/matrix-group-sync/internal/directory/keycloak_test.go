package directory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeKeycloak is an httptest fixture standing in for the Keycloak admin REST API: it issues a
// client-credentials token, resolves a group by path, and serves paginated members carrying the
// matrix_localpart attribute. failAfterPage lets a test simulate a mid-pagination transport fault.
type fakeKeycloak struct {
	groupID       string
	pages         [][]userRepresentation
	failAfterPage int // -1 = never fail
	tokenCalls    int
}

func (f *fakeKeycloak) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/fgentic/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		f.tokenCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 300})
	})
	mux.HandleFunc("/admin/realms/fgentic/group-by-path/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": f.groupID})
	})
	mux.HandleFunc("/admin/realms/fgentic/groups/"+f.groupID+"/members", func(w http.ResponseWriter, r *http.Request) {
		first, _ := strconv.Atoi(r.URL.Query().Get("first"))
		max, _ := strconv.Atoi(r.URL.Query().Get("max"))
		page := first / max
		if f.failAfterPage >= 0 && page > f.failAfterPage {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var out []userRepresentation
		if page < len(f.pages) {
			out = f.pages[page]
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

func user(id, localpart string) userRepresentation {
	attrs := map[string][]string{}
	if localpart != "" {
		attrs["matrix_localpart"] = []string{localpart}
	}
	return userRepresentation{ID: id, Attributes: attrs}
}

func newClient(t *testing.T, f *fakeKeycloak, pageSize int) *Keycloak {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return NewKeycloak(srv.URL, "fgentic", "matrix-group-sync", "secret", pageSize, &http.Client{Timeout: 5 * time.Second})
}

func TestSnapshotPaginatesAndReadsLocalpart(t *testing.T) {
	f := &fakeKeycloak{
		groupID:       "gid",
		failAfterPage: -1,
		pages: [][]userRepresentation{
			{user("s1", "alice"), user("s2", "bob")}, // full page => keep paging
			{user("s3", "carol")},                    // short page => stop
		},
	}
	k := newClient(t, f, 2)
	snap, err := k.Snapshot(context.Background(), []string{"/fgentic/agent-access/platform"})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Complete {
		t.Fatal("a fully-paginated read must be Complete")
	}
	members := snap.Groups["/fgentic/agent-access/platform"]
	if len(members) != 3 {
		t.Fatalf("expected 3 members across 2 pages, got %d", len(members))
	}
	if members[0].Sub != "s1" || members[0].Localpart != "alice" {
		t.Fatalf("unexpected first member: %+v", members[0])
	}
	if members[2].Localpart != "carol" {
		t.Fatalf("pagination dropped a member: %+v", members)
	}
}

func TestSnapshotMissingLocalpartIsEmpty(t *testing.T) {
	f := &fakeKeycloak{
		groupID:       "gid",
		failAfterPage: -1,
		pages:         [][]userRepresentation{{user("s1", "")}},
	}
	k := newClient(t, f, 10)
	snap, err := k.Snapshot(context.Background(), []string{"/fgentic/agent-access/platform"})
	if err != nil {
		t.Fatal(err)
	}
	m := snap.Groups["/fgentic/agent-access/platform"][0]
	if m.Localpart != "" {
		t.Fatalf("a member without matrix_localpart must yield an empty localpart (fail-closed downstream), got %q", m.Localpart)
	}
}

func TestSnapshotPartialReadIsNotComplete(t *testing.T) {
	f := &fakeKeycloak{
		groupID:       "gid",
		failAfterPage: 0, // page 0 ok, page 1 → 500
		pages: [][]userRepresentation{
			{user("s1", "alice"), user("s2", "bob")}, // full page forces a second request
		},
	}
	k := newClient(t, f, 2)
	snap, err := k.Snapshot(context.Background(), []string{"/fgentic/agent-access/platform"})
	if err == nil {
		t.Fatal("a mid-pagination fault must surface an error")
	}
	if snap.Complete {
		t.Fatal("a partial read must never report Complete=true")
	}
}

func TestSnapshotTokenReused(t *testing.T) {
	f := &fakeKeycloak{groupID: "gid", failAfterPage: -1, pages: [][]userRepresentation{{user("s1", "alice")}}}
	k := newClient(t, f, 10)
	for i := 0; i < 3; i++ {
		if _, err := k.Snapshot(context.Background(), []string{"/g"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenCalls != 1 {
		t.Fatalf("expected the bearer token to be cached and reused, got %d token calls", f.tokenCalls)
	}
}

func TestSnapshotUnescapesGroupPath(t *testing.T) {
	f := &fakeKeycloak{groupID: "gid", failAfterPage: -1, pages: [][]userRepresentation{{}}}
	var gotPath string
	base := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "group-by-path") {
			gotPath = r.URL.Path
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	k := NewKeycloak(srv.URL, "fgentic", "c", "s", 10, &http.Client{Timeout: 5 * time.Second})
	if _, err := k.Snapshot(context.Background(), []string{"/fgentic/agent-access/platform"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/group-by-path/fgentic/agent-access/platform") {
		t.Fatalf("group-by-path must carry the full path without a leading slash, got %q", gotPath)
	}
}
