package replyscan

import "fmt"

// Mode is the enforcement posture applied when the reply scan finds a credential. The zero value is
// ModeOff so an unset or explicitly disabled control restores the prior post-unchanged behavior.
//
// The ordering off < annotate < redact < block is a strictness ordering: a stricter mode reveals
// strictly less. Critically, every non-off mode upholds the same invariant — the raw matched value
// never enters the room or any log — because annotate and redact both mask the span; the modes
// differ only in transparency (annotate appends a value-free notice) and in whether a masked
// remainder is still delivered (block withholds the whole reply). Federation detection therefore
// only ever decides mask-vs-withhold, never whether a secret leaks.
type Mode int

const (
	// ModeOff disables the scan: the reply is delivered exactly as the agent produced it.
	ModeOff Mode = iota
	// ModeAnnotate masks matched spans and appends a bounded, value-free transparency notice.
	ModeAnnotate
	// ModeRedact masks matched spans and delivers the remainder with no added notice.
	ModeRedact
	// ModeBlock withholds the whole reply and posts only a content-free withheld notice.
	ModeBlock
)

// ParseMode maps a configuration string to a Mode, failing closed on any unknown value so a
// typo never silently disables the control.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "off":
		return ModeOff, nil
	case "annotate":
		return ModeAnnotate, nil
	case "redact":
		return ModeRedact, nil
	case "block":
		return ModeBlock, nil
	default:
		return ModeOff, fmt.Errorf("unknown reply-scan mode %q (want off|annotate|redact|block)", s)
	}
}

// String renders the canonical configuration token for a Mode.
func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeAnnotate:
		return "annotate"
	case ModeRedact:
		return "redact"
	case ModeBlock:
		return "block"
	default:
		return "off"
	}
}

// AtLeastAsStrict reports whether m enforces at least as much as other, used to validate that the
// federated posture is never weaker than the same-org base posture.
func (m Mode) AtLeastAsStrict(other Mode) bool { return m >= other }
