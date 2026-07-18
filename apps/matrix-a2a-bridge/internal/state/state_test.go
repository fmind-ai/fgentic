package state

import (
	"slices"
	"testing"
	"time"
)

func TestMemoryContextsKeyedByRoomAndGhost(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()

	// Two agents in the same room must not share a thread (SPEC §4 F5).
	if err := s.SetContext(ctx, "!room", "agent-a", "ctx-a", "@alice:example.org"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContext(ctx, "!room", "agent-b", "ctx-b", "@alice:example.org"); err != nil {
		t.Fatal(err)
	}
	for ghost, want := range map[string]string{"agent-a": "ctx-a", "agent-b": "ctx-b"} {
		got, err := s.Context(ctx, "!room", ghost)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Context(!room, %s) = %q, want %q", ghost, got, want)
		}
	}
	if got, _ := s.Context(ctx, "!other", "agent-a"); got != "" {
		t.Errorf("unknown room returned %q, want empty", got)
	}
}

func TestMemoryConversationTracksOwnersAndRejectsStaleDelete(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()
	if err := s.SetContext(ctx, "!room", "agent-a", "ctx-a", "@alice:example.org"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddContextOwner(ctx, "!room", "agent-a", "ctx-a", "@bob:example.org"); err != nil {
		t.Fatal(err)
	}
	observed, found, err := s.Conversation(ctx, "!room", "agent-a")
	if err != nil || !found {
		t.Fatalf("Conversation() = %#v, %v, %v", observed, found, err)
	}
	if want := []string{"@alice:example.org", "@bob:example.org"}; !slices.Equal(observed.Owners, want) {
		t.Fatalf("owners = %v, want %v", observed.Owners, want)
	}
	if err := s.SetContext(ctx, "!room", "agent-a", "ctx-new", "@carol:example.org"); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.DeleteConversation(ctx, observed); err != nil || deleted {
		t.Fatalf("stale DeleteConversation() = %v, %v, want false, nil", deleted, err)
	}
	current, _, _ := s.Conversation(ctx, "!room", "agent-a")
	if current.ContextID != "ctx-new" || !slices.Equal(current.Owners, []string{"@carol:example.org"}) {
		t.Fatalf("current conversation = %#v", current)
	}
}

func TestMemoryConversationRetentionSkipsIncompleteAndBusyRows(t *testing.T) {
	s := NewMemory()
	old := time.Now().UTC().Add(-time.Hour)
	s.contexts[[2]string{"!ready", "agent-a"}] = Conversation{
		RoomID: "!ready", Ghost: "agent-a", ContextID: "ctx-ready", Owners: []string{"@alice:example.org"},
		OwnersComplete: true, UpdatedAt: old,
	}
	s.contexts[[2]string{"!legacy", "agent-a"}] = Conversation{
		RoomID: "!legacy", Ghost: "agent-a", ContextID: "ctx-legacy", UpdatedAt: old.Add(-time.Minute),
	}
	s.contexts[[2]string{"!busy", "agent-a"}] = Conversation{
		RoomID: "!busy", Ghost: "agent-a", ContextID: "ctx-busy", Owners: []string{"@alice:example.org"},
		OwnersComplete: true, UpdatedAt: old.Add(-2 * time.Minute),
	}
	s.jobs["busy"] = Job{RoomID: "!busy", GhostLocalpart: "agent-a", State: StatePending}

	conversations, err := s.ConversationsBefore(t.Context(), "agent-a", time.Now().UTC(), 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ContextID != "ctx-ready" {
		t.Fatalf("retention candidates = %#v, want only ctx-ready", conversations)
	}
}

func TestMemoryMarkEventProcessed(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()

	first, err := s.MarkEventProcessed(ctx, "$evt1")
	if err != nil || !first {
		t.Fatalf("first sighting = (%v, %v), want (true, nil)", first, err)
	}
	again, err := s.MarkEventProcessed(ctx, "$evt1")
	if err != nil || again {
		t.Fatalf("second sighting = (%v, %v), want (false, nil)", again, err)
	}
}

func TestMemoryMarkRoomWelcomed(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()

	first, err := s.MarkRoomWelcomed(ctx, "!room:example.org")
	if err != nil || !first {
		t.Fatalf("first welcome = (%v, %v), want (true, nil)", first, err)
	}
	again, err := s.MarkRoomWelcomed(ctx, "!room:example.org")
	if err != nil || again {
		t.Fatalf("second welcome = (%v, %v), want (false, nil)", again, err)
	}
	other, err := s.MarkRoomWelcomed(ctx, "!other:example.org")
	if err != nil || !other {
		t.Fatalf("other-room welcome = (%v, %v), want (true, nil)", other, err)
	}
}
