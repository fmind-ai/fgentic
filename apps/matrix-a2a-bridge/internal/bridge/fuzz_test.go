package bridge

import "testing"

// FuzzParsePlaintextCommand fuzzes the plaintext-fallback command parser, which consumes untrusted
// room message bodies (the #1 untrusted input per docs/security.md). It asserts the parser never
// panics and always returns a well-formed command: a valid kind, and — for an accepted /ask — a
// non-empty agent and prompt, so a hostile body can never yield an actionable command with missing
// fields.
func FuzzParsePlaintextCommand(f *testing.F) {
	for _, seed := range []string{
		"", " ", "\t\n", "hello world",
		"/ask", "/ask agent", "/ask agent prompt text",
		"!ask agent prompt", "/agents", "/agents query", "/agents a b",
		"/budget", "!budget x", "/unknown", "/",
		"/ask \u202e prompt", "/ask agent \U0001f600", "/ask  \t  ",
		"/ask\x00agent prompt",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		command := parsePlaintextCommand(body)
		switch command.kind {
		case plaintextCommandNone, plaintextCommandInvalid, plaintextCommandAgents, plaintextCommandBudget:
			// These carry no execution guarantee beyond their kind.
		case plaintextCommandAsk:
			if command.agent == "" || command.prompt == "" {
				t.Fatalf("accepted /ask with empty agent %q or prompt %q for body %q",
					command.agent, command.prompt, body)
			}
		default:
			t.Fatalf("parser returned unknown command kind %d for body %q", command.kind, body)
		}

		// Budget commands never carry operands; the parser must not leak trailing tokens into them.
		if command.kind == plaintextCommandBudget &&
			(command.agent != "" || command.prompt != "" || command.query != "") {
			t.Fatalf("budget command carried operands for body %q", body)
		}
	})
}
