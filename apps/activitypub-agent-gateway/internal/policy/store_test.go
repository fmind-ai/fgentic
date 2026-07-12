package policy

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePolicy(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func TestStoreAdmitAndReloadFlipsAllowToDeny(t *testing.T) {
	dir := t.TempDir()
	path := writePolicy(t, dir, `{"version":1,"allowed_domains":["mastodon.example"]}`)
	store := NewStore(path, slog.Default())

	if !store.Healthy() {
		t.Fatalf("store should be healthy after a valid load")
	}
	if d := store.Admit("https://mastodon.example/users/bob"); !d.Allowed || d.Reason != "allowlisted" {
		t.Fatalf("initial admit = %+v, want allowed", d)
	}

	// Edit policy.json in place (git-reload equivalent) removing the domain, then reload.
	if err := os.WriteFile(path, []byte(`{"version":1,"allowed_domains":["other.example"]}`), 0o600); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}
	store.Reload()
	if d := store.Admit("https://mastodon.example/users/bob"); d.Allowed {
		t.Fatalf("after reload the previously-allowed actor must be denied: %+v", d)
	}
	if d := store.Admit("https://other.example/users/x"); !d.Allowed {
		t.Fatalf("newly-allowed domain must be admitted after reload: %+v", d)
	}
}

func TestStoreFailsClosedOnInvalidPolicy(t *testing.T) {
	dir := t.TempDir()
	path := writePolicy(t, dir, `{ not valid json`)
	store := NewStore(path, slog.Default())
	if store.Healthy() {
		t.Errorf("store must be unhealthy on invalid policy")
	}
	if d := store.Admit("https://mastodon.example/users/bob"); d.Allowed || d.Reason != "policy_unavailable" {
		t.Errorf("invalid policy must fail closed: %+v", d)
	}

	// Once a valid policy is written, a reload recovers.
	if err := os.WriteFile(path, []byte(`{"version":1,"allowed_domains":["mastodon.example"]}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	store.Reload()
	if !store.Healthy() || !store.Admit("https://mastodon.example/users/bob").Allowed {
		t.Errorf("store must recover after a valid policy is restored")
	}
}

func TestStoreMissingFileFailsClosed(t *testing.T) {
	store := NewStore("/no/such/policy.json", slog.Default())
	if store.Healthy() || store.Admit("https://mastodon.example/users/bob").Allowed {
		t.Errorf("missing policy must fail closed")
	}
}

func TestStoreWatchReloads(t *testing.T) {
	dir := t.TempDir()
	path := writePolicy(t, dir, `{"version":1,"allowed_domains":["mastodon.example"]}`)
	store := NewStore(path, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.Watch(ctx, 10*time.Millisecond)

	if err := os.WriteFile(path, []byte(`{"version":1,"allowed_domains":["other.example"]}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !store.Admit("https://mastodon.example/users/bob").Allowed {
			return // watcher observed the change
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Watch did not hot-reload the policy in time")
}
