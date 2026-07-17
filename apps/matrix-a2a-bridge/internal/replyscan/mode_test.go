package replyscan

import "testing"

func TestParseModeRoundTrip(t *testing.T) {
	for _, m := range []Mode{ModeOff, ModeAnnotate, ModeRedact, ModeBlock} {
		got, err := ParseMode(m.String())
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", m.String(), err)
		}
		if got != m {
			t.Fatalf("ParseMode(%q) = %v, want %v", m.String(), got, m)
		}
	}
}

func TestParseModeFailsClosed(t *testing.T) {
	got, err := ParseMode("annotatte")
	if err == nil {
		t.Fatal("expected an error on an unknown mode")
	}
	if got != ModeOff {
		t.Fatalf("unknown mode must fail to off, got %v", got)
	}
}

func TestModeStrictnessOrdering(t *testing.T) {
	if !ModeBlock.AtLeastAsStrict(ModeRedact) || !ModeRedact.AtLeastAsStrict(ModeAnnotate) ||
		!ModeAnnotate.AtLeastAsStrict(ModeOff) {
		t.Fatal("expected off < annotate < redact < block strictness ordering")
	}
	if ModeAnnotate.AtLeastAsStrict(ModeBlock) {
		t.Fatal("annotate must not be considered as strict as block")
	}
}
