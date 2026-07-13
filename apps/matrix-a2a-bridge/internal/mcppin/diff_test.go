package mcppin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCompareReportsRecursiveDescriptionAndSchemaDrift(t *testing.T) {
	t.Parallel()

	before := mustFile(t, mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{json.RawMessage(
			`{"name":"echo","description":"Echo input","inputSchema":{"type":"object","properties":{"value":{"type":"string","description":"old"}}}}`,
		)},
	}))
	after := mustFile(t, mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{json.RawMessage(
			`{"inputSchema":{"properties":{"value":{"description":"Old","type":"string"}},"type":"object"},"description":"Echo inputs","name":"echo"}`,
		)},
	}))

	changes, err := Compare(before, after)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	want := []Change{
		{
			Operation: OperationChanged,
			Path:      `$.servers["fixture"].tools["echo"].description`,
			Before:    json.RawMessage(`"Echo input"`),
			After:     json.RawMessage(`"Echo inputs"`),
		},
		{
			Operation: OperationChanged,
			Path:      `$.servers["fixture"].tools["echo"].inputSchema.properties.value.description`,
			Before:    json.RawMessage(`"old"`),
			After:     json.RawMessage(`"Old"`),
		},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("Compare() = %#v, want %#v", changes, want)
	}
}

func TestCompareReportsInitializeInstructionDrift(t *testing.T) {
	t.Parallel()

	before := mustFile(t, mustServer(t, "fixture", nil, Surface{Initialize: json.RawMessage(
		`{"capabilities":{},"instructions":"Use reviewed tools.","protocolVersion":"2025-06-18","serverInfo":{"name":"fixture","version":"1"}}`,
	)}))
	after := mustFile(t, mustServer(t, "fixture", nil, Surface{Initialize: json.RawMessage(
		`{"serverInfo":{"version":"1","name":"fixture"},"protocolVersion":"2025-06-18","instructions":"Use reviewed toolS.","capabilities":{}}`,
	)}))

	changes, err := Compare(before, after)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	want := []Change{{
		Operation: OperationChanged,
		Path:      `$.servers["fixture"].initialize.instructions`,
		Before:    json.RawMessage(`"Use reviewed tools."`),
		After:     json.RawMessage(`"Use reviewed toolS."`),
	}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("Compare() = %#v, want %#v", changes, want)
	}
}

func TestCompareReportsAddedRemovedAndSupportInStableOrder(t *testing.T) {
	t.Parallel()

	before := mustFile(t, mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"beta","description":"remove"}`),
			json.RawMessage(`{"name":"stable","description":"same"}`),
		},
	}))
	after := mustFile(t, mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"description":"same","name":"stable"}`),
			json.RawMessage(`{"description":"add","name":"alpha"}`),
		},
		PromptsSupported: true,
	}))

	changes, err := Compare(before, after)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if got, want := changeSummaries(changes), []string{
		`added $.servers["fixture"].initialize.capabilities.prompts = {}`,
		`added $.servers["fixture"].tools["alpha"] = {"description":"add","name":"alpha"}`,
		`removed $.servers["fixture"].tools["beta"] = {"description":"remove","name":"beta"}`,
		`changed $.servers["fixture"].prompts.supported: false -> true`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

func TestCompareIgnoresResponseAndObjectOrdering(t *testing.T) {
	t.Parallel()

	first := mustFile(t, mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"beta","inputSchema":{"type":"object","properties":{"z":{"type":"number"},"a":{"type":"string"}}}}`),
			json.RawMessage(`{"name":"alpha","description":"same"}`),
		},
	}))
	second := mustFile(t, mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"description":"same","name":"alpha"}`),
			json.RawMessage(`{"inputSchema":{"properties":{"a":{"type":"string"},"z":{"type":"number"}},"type":"object"},"name":"beta"}`),
		},
	}))

	changes, err := Compare(first, second)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("Compare() = %v, want no ordering-only drift", changeSummaries(changes))
	}
}

func TestComparePreservesOrderedProvenanceArrays(t *testing.T) {
	t.Parallel()

	beforeProvenance := testProvenance()
	beforeProvenance.Command = []string{"launcher", "server"}
	beforeProvenance.Arguments = []string{"--first", "--second"}
	afterProvenance := testProvenance()
	afterProvenance.Command = []string{"server", "launcher"}
	afterProvenance.Arguments = []string{"--second", "--first"}
	afterProvenance.Backend.Path = "/mcp/v2"

	beforeServer, err := NewServer("fixture", beforeProvenance, Surface{Initialize: testInitialize()})
	if err != nil {
		t.Fatalf("NewServer(before) error = %v", err)
	}
	afterServer, err := NewServer("fixture", afterProvenance, Surface{Initialize: testInitialize()})
	if err != nil {
		t.Fatalf("NewServer(after) error = %v", err)
	}
	changes, err := Compare(mustFile(t, beforeServer), mustFile(t, afterServer))
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if got, want := changePaths(changes), []string{
		`$.servers["fixture"].provenance.arguments[0]`,
		`$.servers["fixture"].provenance.arguments[1]`,
		`$.servers["fixture"].provenance.backend.path`,
		`$.servers["fixture"].provenance.command[0]`,
		`$.servers["fixture"].provenance.command[1]`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if beforeServer.SurfaceSHA256 != afterServer.SurfaceSHA256 {
		t.Fatal("provenance-only drift must not alter the surface-only hash")
	}
}

func TestCompareReportsServersInNameOrder(t *testing.T) {
	t.Parallel()

	before := mustFile(t, mustServer(t, "alpha", nil, Surface{}))
	after := mustFile(t, mustServer(t, "beta", nil, Surface{}))
	changes, err := Compare(before, after)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if got, want := changePaths(changes), []string{`$.servers["alpha"]`, `$.servers["beta"]`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if changes[0].Operation != OperationRemoved || changes[1].Operation != OperationAdded {
		t.Fatalf("operations = %s, %s; want removed, added", changes[0].Operation, changes[1].Operation)
	}
}

func TestCompareCanonicalizesAddedAndRemovedObjects(t *testing.T) {
	t.Parallel()

	beforeServer := mustServer(t, "fixture", nil, Surface{ToolsSupported: true})
	afterServer := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"echo","description":"added"}`),
		},
	})
	// A parsed pin may retain whitespace around an otherwise valid raw object. Drift output must
	// still be canonical and independent of that presentation detail.
	afterServer.Tools.Entries[0].Object = json.RawMessage("{\n  \"name\": \"echo\",\n  \"description\": \"added\"\n}")

	changes, err := Compare(mustFile(t, beforeServer), File{
		SchemaVersion: CurrentSchemaVersion,
		Servers:       []Server{afterServer},
	})
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if got, want := string(changes[0].After), `{"description":"added","name":"echo"}`; got != want {
		t.Fatalf("added object = %s, want canonical %s", got, want)
	}
}

func TestCompareRejectsInvalidPin(t *testing.T) {
	t.Parallel()

	valid := mustFile(t, mustServer(t, "fixture", nil, Surface{}))
	invalid := cloneFile(valid)
	invalid.Servers[0].SurfaceSHA256 = strings.Repeat("0", sha256HexLength)
	if _, err := Compare(invalid, valid); err == nil || !strings.Contains(err.Error(), "validate before pin") {
		t.Fatalf("Compare() error = %v, want invalid-before context", err)
	}
}

func TestChangeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		change Change
		want   string
	}{
		{
			change: Change{Operation: OperationAdded, Path: "$.value", After: json.RawMessage(`true`)},
			want:   "added $.value = true",
		},
		{
			change: Change{Operation: OperationRemoved, Path: "$.value", Before: json.RawMessage(`1`)},
			want:   "removed $.value = 1",
		},
		{
			change: Change{Operation: OperationChanged, Path: "$.value", Before: json.RawMessage(`1`), After: json.RawMessage(`2`)},
			want:   "changed $.value: 1 -> 2",
		},
	}
	for _, test := range tests {
		if got := test.change.String(); got != test.want {
			t.Errorf("String() = %q, want %q", got, test.want)
		}
	}
}

func mustFile(t *testing.T, servers ...Server) File {
	t.Helper()
	file, err := NewFile(servers)
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}
	return file
}

func changePaths(changes []Change) []string {
	paths := make([]string, len(changes))
	for index, change := range changes {
		paths[index] = change.Path
	}
	return paths
}

func changeSummaries(changes []Change) []string {
	summaries := make([]string, len(changes))
	for index, change := range changes {
		summaries[index] = change.String()
	}
	return summaries
}
