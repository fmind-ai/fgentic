package agentschema

import (
	"bytes"
	"testing"
)

// FuzzValidateAgents fuzzes the agents.yaml schema validator. The agent mapping is operator-authored
// rather than remote-attacker input, but a validator panic would still turn a malformed config into
// a crash instead of a clean rejection, so it must always return an error or nil, never panic.
func FuzzValidateAgents(f *testing.F) {
	for _, seed := range []string{
		"",
		"schemaVersion: 1\nagents: []\n",
		"schemaVersion: 1\nagents:\n  - id: docs\n",
		"not: [valid",
		"schemaVersion: true\n",
		"- - - -\n",
		"\x00\x01\x02",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(_ *testing.T, input []byte) {
		// Any input must produce a clean error or nil, never a panic.
		_ = Validate(testSchemaPath, bytes.NewReader(input))
	})
}
