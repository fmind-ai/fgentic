package state

import "testing"

func TestMemoryContextsKeyedByRoomAndGhost(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()

	// Two agents in the same room must not share a thread (SPEC §4 F5).
	if err := s.SetContext(ctx, "!room", "agent-a", "ctx-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContext(ctx, "!room", "agent-b", "ctx-b"); err != nil {
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
