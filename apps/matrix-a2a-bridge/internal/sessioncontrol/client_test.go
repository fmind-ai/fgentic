package sessioncontrol

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPurgeDeletesAndVerifiesEveryOwner(t *testing.T) {
	const contextID = "ctx/with space"
	remaining := map[string]bool{"@alice:example.org": true, "@bob:example.org": true}
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		owner := request.URL.Query().Get("user_id")
		if request.Header.Get("X-User-ID") != owner ||
			request.URL.EscapedPath() != "/api/sessions/ctx%2Fwith%20space" {
			t.Errorf("unexpected request %s %s headers=%v", request.Method, request.URL, request.Header)
		}
		methods = append(methods, request.Method+" "+owner)
		switch request.Method {
		case http.MethodDelete:
			remaining[owner] = false
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if remaining[owner] {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Purge(t.Context(), contextID, []string{"@alice:example.org", "@bob:example.org"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DELETE @alice:example.org", "GET @alice:example.org",
		"DELETE @bob:example.org", "GET @bob:example.org",
	}
	if !slices.Equal(methods, want) {
		t.Fatalf("request sequence = %v, want %v", methods, want)
	}
}

func TestPurgeFailsWhenDeleteDoesNotMakeSessionUnreadable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Purge(t.Context(), "ctx-1", []string{"@alice:example.org"}); err == nil {
		t.Fatal("Purge() error = nil, want verification failure")
	}
}

func TestPurgeRejectsUnknownOwners(t *testing.T) {
	client, err := New("http://kagent.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Purge(t.Context(), "ctx-1", nil); !errors.Is(err, ErrOwnersUnknown) {
		t.Fatalf("Purge() error = %v, want ErrOwnersUnknown", err)
	}
}
