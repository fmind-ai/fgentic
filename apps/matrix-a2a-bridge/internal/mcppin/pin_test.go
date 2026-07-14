package mcppin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const testImage = "registry.example.test/mcp@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNewServerCanonicalizesCompleteSurface(t *testing.T) {
	t.Parallel()

	provenance := testProvenance()
	provenance.Arguments = []string{"serve", "--read-only"}
	server, err := NewServer("fixture", provenance, Surface{
		Initialize: json.RawMessage(
			`{"capabilities":{"prompts":{},"resources":{},"tools":{}},"protocolVersion":"2025-06-18","serverInfo":{"name":"fixture","version":"1"}}`,
		),
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"zeta","description":"last","inputSchema":{"type":"object"}}`),
			json.RawMessage(`{"inputSchema":{"properties":{"value":{"type":"string"}},"type":"object"},"_meta":{"owner":"security"},"description":"first","name":"alpha"}`),
		},
		PromptsSupported: true,
		Prompts: []json.RawMessage{
			json.RawMessage(`{"name":"review","description":"Review a change","arguments":[{"name":"diff","required":true}]}`),
		},
		ResourcesSupported: true,
		Resources: []json.RawMessage{
			json.RawMessage(`{"uri":"file:///runbook","name":"runbook","mimeType":"text/markdown","annotations":{"audience":["assistant"]}}`),
		},
		ResourceTemplatesSupported: true,
		ResourceTemplates: []json.RawMessage{
			json.RawMessage(`{"uriTemplate":"file:///runbooks/{name}","name":"runbooks","description":"Named runbook"}`),
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	if got, want := server.Provenance.Arguments, []string{"serve", "--read-only"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %v, want ordered %v", got, want)
	}
	provenance.Command[0] = "mutated"
	provenance.Arguments[0] = "mutated"
	if server.Provenance.Command[0] != "/app/mcp-server" || server.Provenance.Arguments[0] != "serve" {
		t.Fatal("server retained caller-owned provenance array storage")
	}
	if got, want := identities(server.Tools.Entries), []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tool identities = %v, want %v", got, want)
	}
	if got, want := string(server.Tools.Entries[0].Object), `{"_meta":{"owner":"security"},"description":"first","inputSchema":{"properties":{"value":{"type":"string"}},"type":"object"},"name":"alpha"}`; got != want {
		t.Fatalf("canonical complete tool = %s, want %s", got, want)
	}
	if server.Tools.Entries[0].SHA256 == "" || server.SurfaceSHA256 == "" {
		t.Fatal("entry and surface hashes must be populated")
	}
	if err := server.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestNewFileSortsAndClonesServers(t *testing.T) {
	t.Parallel()

	zeta := mustServer(t, "zeta", nil, Surface{})
	alpha := mustServer(t, "alpha", []string{"first", "second"}, Surface{})
	file, err := NewFile([]Server{zeta, alpha})
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}
	if got, want := []string{file.Servers[0].Name, file.Servers[1].Name}, []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("server order = %v, want %v", got, want)
	}

	alpha.Provenance.Arguments[0] = "mutated"
	if got := file.Servers[0].Provenance.Arguments[0]; got != "first" {
		t.Fatalf("file retained caller-owned argument storage: %q", got)
	}
	if err := file.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestSurfaceHashDistinguishesSupportFromEmpty(t *testing.T) {
	t.Parallel()

	unsupported := mustServer(t, "fixture", nil, Surface{})
	supported := mustServer(t, "fixture", nil, Surface{ToolsSupported: true})
	if unsupported.SurfaceSHA256 == supported.SurfaceSHA256 {
		t.Fatalf("unsupported and supported-empty tools share hash %q", unsupported.SurfaceSHA256)
	}
	if supported.Tools.Entries == nil {
		t.Fatal("supported-empty collection must serialize as an empty array")
	}
	if unsupported.Tools.Supported || !supported.Tools.Supported {
		t.Fatal("tool support bit was not preserved")
	}
}

func TestCanonicalHashesIgnoreObjectAndCollectionOrder(t *testing.T) {
	t.Parallel()

	first := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"beta","description":"B","inputSchema":{"type":"object","properties":{"z":{"type":"number"},"a":{"type":"string"}}}}`),
			json.RawMessage(`{"description":"A","name":"alpha","inputSchema":{"type":"object"}}`),
		},
	})
	second := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"inputSchema":{"type":"object"},"name":"alpha","description":"A"}`),
			json.RawMessage(`{"inputSchema":{"properties":{"a":{"type":"string"},"z":{"type":"number"}},"type":"object"},"description":"B","name":"beta"}`),
		},
	})
	if !reflect.DeepEqual(first.Tools, second.Tools) {
		t.Fatalf("canonical collections differ:\nfirst:  %+v\nsecond: %+v", first.Tools, second.Tools)
	}
	if first.SurfaceSHA256 != second.SurfaceSHA256 {
		t.Fatalf("surface hashes differ: %s != %s", first.SurfaceSHA256, second.SurfaceSHA256)
	}
}

func TestGenerateResourceSchemaDescriptionNormalization(t *testing.T) {
	t.Parallel()

	firstDescription := resourceTypeDescription([]string{
		"istio_auth_policy",
		"istio_virtual_service",
		"gateway_api_reference_grant",
		"gateway_api_gateway_class",
		"gateway_api_grpc_route",
		"argo_analysis_template",
		"gateway_api_gateway",
		"gateway_api_http_route",
		"argo_rollout",
	})
	secondDescription := resourceTypeDescription([]string{
		"argo_rollout",
		"gateway_api_http_route",
		"gateway_api_gateway",
		"argo_analysis_template",
		"gateway_api_grpc_route",
		"gateway_api_gateway_class",
		"gateway_api_reference_grant",
		"istio_virtual_service",
		"istio_auth_policy",
	})
	wantDescription := resourceTypeDescription([]string{
		"argo_analysis_template",
		"argo_rollout",
		"gateway_api_gateway",
		"gateway_api_gateway_class",
		"gateway_api_grpc_route",
		"gateway_api_http_route",
		"gateway_api_reference_grant",
		"istio_auth_policy",
		"istio_virtual_service",
	})

	first := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools:          []json.RawMessage{resourceTool(t, "k8s_generate_resource", firstDescription)},
	})
	second := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools:          []json.RawMessage{resourceTool(t, "k8s_generate_resource", secondDescription)},
	})
	if first.Tools.Entries[0].SHA256 != second.Tools.Entries[0].SHA256 {
		t.Fatalf("permutation hashes differ: %s != %s", first.Tools.Entries[0].SHA256, second.Tools.Entries[0].SHA256)
	}
	if first.SurfaceSHA256 != second.SurfaceSHA256 {
		t.Fatalf("permutation surface hashes differ: %s != %s", first.SurfaceSHA256, second.SurfaceSHA256)
	}
	if got := resourceTypeDescriptionFromObject(t, first.Tools.Entries[0].Object); got != wantDescription {
		t.Fatalf("normalized description = %q, want %q", got, wantDescription)
	}
}

func TestGenerateResourceNormalizationPreservesRealDrift(t *testing.T) {
	t.Parallel()

	identifiers := []string{
		"argo_analysis_template",
		"argo_rollout",
		"gateway_api_gateway",
		"gateway_api_gateway_class",
		"gateway_api_grpc_route",
		"gateway_api_http_route",
		"gateway_api_reference_grant",
		"istio_auth_policy",
		"istio_virtual_service",
	}
	baselineDescription := resourceTypeDescription(identifiers)
	baseline := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools:          []json.RawMessage{resourceTool(t, "k8s_generate_resource", baselineDescription)},
	})

	tests := []struct {
		name        string
		description string
	}{
		{
			name: "changed token",
			description: resourceTypeDescription([]string{
				"argo_analysis_template",
				"argo_rollouts",
				"gateway_api_gateway",
				"gateway_api_gateway_class",
				"gateway_api_grpc_route",
				"gateway_api_http_route",
				"gateway_api_reference_grant",
				"istio_auth_policy",
				"istio_virtual_service",
			}),
		},
		{
			name:        "changed prose",
			description: strings.Replace(baselineDescription, "Type of resource", "Kind of resource", 1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changed := mustServer(t, "fixture", nil, Surface{
				ToolsSupported: true,
				Tools:          []json.RawMessage{resourceTool(t, "k8s_generate_resource", test.description)},
			})
			if changed.Tools.Entries[0].SHA256 == baseline.Tools.Entries[0].SHA256 {
				t.Fatalf("changed metadata retained hash %s", changed.Tools.Entries[0].SHA256)
			}
			if got := resourceTypeDescriptionFromObject(t, changed.Tools.Entries[0].Object); got != test.description {
				t.Fatalf("description = %q, want unchanged %q", got, test.description)
			}
		})
	}
}

func TestGenerateResourceNormalizationDoesNotApplyToOtherTools(t *testing.T) {
	t.Parallel()

	firstDescription := resourceTypeDescription([]string{
		"istio_auth_policy", "istio_virtual_service", "gateway_api_reference_grant",
		"gateway_api_gateway_class", "gateway_api_grpc_route", "argo_analysis_template",
		"gateway_api_gateway", "gateway_api_http_route", "argo_rollout",
	})
	secondDescription := resourceTypeDescription([]string{
		"argo_rollout", "gateway_api_http_route", "gateway_api_gateway",
		"argo_analysis_template", "gateway_api_grpc_route", "gateway_api_gateway_class",
		"gateway_api_reference_grant", "istio_virtual_service", "istio_auth_policy",
	})
	first := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools:          []json.RawMessage{resourceTool(t, "unrelated_tool", firstDescription)},
	})
	second := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools:          []json.RawMessage{resourceTool(t, "unrelated_tool", secondDescription)},
	})
	if first.Tools.Entries[0].SHA256 == second.Tools.Entries[0].SHA256 {
		t.Fatal("unrelated tool description order was unexpectedly normalized")
	}
	if got := resourceTypeDescriptionFromObject(t, first.Tools.Entries[0].Object); got != firstDescription {
		t.Fatalf("unrelated description = %q, want unchanged %q", got, firstDescription)
	}
}

func TestValidateRejectsUnnormalizedGenerateResourceDescription(t *testing.T) {
	t.Parallel()

	unsortedDescription := resourceTypeDescription([]string{
		"istio_auth_policy", "istio_virtual_service", "gateway_api_reference_grant",
		"gateway_api_gateway_class", "gateway_api_grpc_route", "argo_analysis_template",
		"gateway_api_gateway", "gateway_api_http_route", "argo_rollout",
	})
	server := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools:          []json.RawMessage{resourceTool(t, "k8s_generate_resource", unsortedDescription)},
	})
	unnormalized, err := canonicalObject(resourceTool(t, "k8s_generate_resource", unsortedDescription))
	if err != nil {
		t.Fatalf("canonicalObject() error = %v", err)
	}
	server.Tools.Entries[0].Object = unnormalized
	file := File{SchemaVersion: CurrentSchemaVersion, Servers: []Server{server}}
	if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "not normalized") {
		t.Fatalf("Validate() error = %v, want normalization rejection", err)
	}
	encoded, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := Parse(encoded); err == nil || !strings.Contains(err.Error(), "not normalized") {
		t.Fatalf("Parse() error = %v, want normalization rejection", err)
	}
}

func TestJCSAndSHA256Vector(t *testing.T) {
	t.Parallel()

	server := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"z":1,"name":"echo","description":"caf\u00e9","inputSchema":{"required":["b","a"],"properties":{"z":{"type":"integer"},"a":{"type":"string"}},"type":"object"}}`),
		},
	})
	entry := server.Tools.Entries[0]
	const wantObject = `{"description":"café","inputSchema":{"properties":{"a":{"type":"string"},"z":{"type":"integer"}},"required":["b","a"],"type":"object"},"name":"echo","z":1}`
	const wantSHA256 = "ac3673606e6b69ce95f5002204a239f4bbe9949ff61aadf3fc297b6c7524b9de"
	const wantSurfaceSHA256 = "c72ee2ecf16c29e2cece2ae758b33a02344cb039d9f21007af1210a6210bf6ca"
	if got := string(entry.Object); got != wantObject {
		t.Fatalf("canonical object = %s, want %s", got, wantObject)
	}
	if entry.SHA256 != wantSHA256 {
		t.Fatalf("sha256 = %s, want %s", entry.SHA256, wantSHA256)
	}
	if server.SurfaceSHA256 != wantSurfaceSHA256 {
		t.Fatalf("surface sha256 = %s, want %s", server.SurfaceSHA256, wantSurfaceSHA256)
	}
}

func TestNewServerRejectsInvalidSurface(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		provenance Provenance
		surface    Surface
		want       string
	}{
		{
			name: "mutable image",
			provenance: func() Provenance {
				value := testProvenance()
				value.Image = "registry.example.test/mcp:latest"
				return value
			}(),
			want: "immutable",
		},
		{
			name:       "unsupported entries",
			provenance: testProvenance(),
			surface: Surface{
				Tools: []json.RawMessage{json.RawMessage(`{"name":"echo"}`)},
			},
			want: "unsupported",
		},
		{
			name:       "duplicate identity",
			provenance: testProvenance(),
			surface: Surface{
				ToolsSupported: true,
				Tools: []json.RawMessage{
					json.RawMessage(`{"name":"echo","description":"first"}`),
					json.RawMessage(`{"description":"second","name":"echo"}`),
				},
			},
			want: "duplicate identity",
		},
		{
			name:       "non-object",
			provenance: testProvenance(),
			surface: Surface{
				PromptsSupported: true,
				Prompts:          []json.RawMessage{json.RawMessage(`[]`)},
			},
			want: "JSON object",
		},
		{
			name:       "missing identity",
			provenance: testProvenance(),
			surface: Surface{
				ResourcesSupported: true,
				Resources:          []json.RawMessage{json.RawMessage(`{"name":"orphan"}`)},
			},
			want: `no "uri" identity`,
		},
		{
			name:       "templates without resources capability",
			provenance: testProvenance(),
			surface: Surface{
				ResourceTemplatesSupported: true,
				ResourceTemplates: []json.RawMessage{
					json.RawMessage(`{"name":"guide","uriTemplate":"test://guide/{id}"}`),
				},
			},
			want: "resourceTemplates cannot be supported",
		},
		{
			name:       "invalid identity",
			provenance: testProvenance(),
			surface: Surface{
				ResourceTemplatesSupported: true,
				ResourceTemplates:          []json.RawMessage{json.RawMessage(`{"uriTemplate":" spaced "}`)},
			},
			want: "without surrounding whitespace",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.surface.Initialize = testInitializeForSurface(test.surface)
			_, err := NewServer("fixture", test.provenance, test.surface)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewServer() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestNewServerRequiresInitializeObject(t *testing.T) {
	t.Parallel()

	_, err := NewServer("fixture", testProvenance(), Surface{})
	if err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Fatalf("NewServer() error = %v, want missing initialize rejection", err)
	}
}

func TestProvenanceValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Provenance)
		want   string
	}{
		{
			name:   "empty command",
			mutate: func(value *Provenance) { value.Command = []string{} },
			want:   "non-empty array",
		},
		{
			name:   "empty command item",
			mutate: func(value *Provenance) { value.Command = []string{""} },
			want:   "command[0]",
		},
		{
			name:   "null arguments",
			mutate: func(value *Provenance) { value.Arguments = nil },
			want:   "arguments must be an array",
		},
		{
			name:   "argument NUL",
			mutate: func(value *Provenance) { value.Arguments = []string{"bad\x00argument"} },
			want:   "NUL",
		},
		{
			name:   "relative discovery URL",
			mutate: func(value *Provenance) { value.Discovery.URL = "/mcp" },
			want:   "absolute http(s)",
		},
		{
			name:   "discovery credentials",
			mutate: func(value *Provenance) { value.Discovery.URL = "https://user@example.test/mcp" },
			want:   "credentials",
		},
		{
			name:   "discovery fragment",
			mutate: func(value *Provenance) { value.Discovery.URL = "https://example.test/mcp#fragment" },
			want:   "fragment",
		},
		{
			name:   "empty discovery protocol",
			mutate: func(value *Provenance) { value.Discovery.Protocol = "" },
			want:   "discovery protocol",
		},
		{
			name:   "invalid backend host",
			mutate: func(value *Provenance) { value.Backend.Host = "https://example.test" },
			want:   "backend host",
		},
		{
			name:   "invalid backend port",
			mutate: func(value *Provenance) { value.Backend.Port = 65536 },
			want:   "1..65535",
		},
		{
			name:   "relative backend path",
			mutate: func(value *Provenance) { value.Backend.Path = "mcp" },
			want:   "must be absolute",
		},
		{
			name:   "backend path query",
			mutate: func(value *Provenance) { value.Backend.Path = "/mcp?admin=true" },
			want:   "query or fragment",
		},
		{
			name:   "empty backend protocol",
			mutate: func(value *Provenance) { value.Backend.Protocol = "" },
			want:   "backend protocol",
		},
	}

	if err := testProvenance().Validate(); err != nil {
		t.Fatalf("valid provenance error = %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provenance := testProvenance()
			test.mutate(&provenance)
			if err := provenance.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsUnsortedDuplicateAndInconsistentPins(t *testing.T) {
	t.Parallel()

	twoTools := mustServer(t, "fixture", nil, Surface{
		ToolsSupported: true,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"alpha"}`),
			json.RawMessage(`{"name":"beta"}`),
		},
	})
	validFile, err := NewFile([]Server{twoTools})
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(File) File
		want   string
	}{
		{
			name: "unsupported with entries",
			mutate: func(file File) File {
				file.Servers[0].Tools.Supported = false
				return file
			},
			want: "unsupported",
		},
		{
			name: "unsorted entries",
			mutate: func(file File) File {
				entries := file.Servers[0].Tools.Entries
				entries[0], entries[1] = entries[1], entries[0]
				return file
			},
			want: "not sorted",
		},
		{
			name: "duplicate entries",
			mutate: func(file File) File {
				file.Servers[0].Tools.Entries[1] = file.Servers[0].Tools.Entries[0]
				return file
			},
			want: "duplicate identity",
		},
		{
			name: "identity mismatch",
			mutate: func(file File) File {
				file.Servers[0].Tools.Entries[0].Identity = "aardvark"
				return file
			},
			want: "object identity",
		},
		{
			name: "initialize capability mismatch",
			mutate: func(file File) File {
				object := json.RawMessage(
					`{"capabilities":{},"protocolVersion":"2025-06-18","serverInfo":{"name":"fixture","version":"1"}}`,
				)
				file.Servers[0].Initialize = PinnedObject{SHA256: digest(object), Object: object}
				return file
			},
			want: "capability tools advertised",
		},
		{
			name: "initialize hash mismatch",
			mutate: func(file File) File {
				file.Servers[0].Initialize.SHA256 = strings.Repeat("0", sha256HexLength)
				return file
			},
			want: "initialize sha256",
		},
		{
			name: "entry hash mismatch",
			mutate: func(file File) File {
				file.Servers[0].Tools.Entries[0].SHA256 = strings.Repeat("0", sha256HexLength)
				return file
			},
			want: "want",
		},
		{
			name: "surface hash mismatch",
			mutate: func(file File) File {
				file.Servers[0].SurfaceSHA256 = strings.Repeat("0", sha256HexLength)
				return file
			},
			want: "surfaceSha256",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := cloneFile(validFile)
			err := test.mutate(file).Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestFileValidationRejectsOrderingAndDuplicates(t *testing.T) {
	t.Parallel()

	alpha := mustServer(t, "alpha", nil, Surface{})
	beta := mustServer(t, "beta", nil, Surface{})
	for _, test := range []struct {
		name    string
		servers []Server
		want    string
	}{
		{name: "unsorted", servers: []Server{beta, alpha}, want: "not sorted"},
		{name: "duplicate", servers: []Server{alpha, alpha}, want: "duplicate server"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := (File{SchemaVersion: CurrentSchemaVersion, Servers: test.servers}).Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestParseRoundTripAndStrictRejection(t *testing.T) {
	t.Parallel()

	server := mustServer(t, "fixture", []string{"serve"}, Surface{ToolsSupported: true})
	file, err := NewFile([]Server{server})
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !reflect.DeepEqual(parsed, file) {
		t.Fatalf("Parse() = %+v, want %+v", parsed, file)
	}

	unknown := []byte(strings.Replace(string(encoded), `"schemaVersion": 1,`, `"schemaVersion": 1, "unknown": true,`, 1))
	if _, err := Parse(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Parse(unknown field) error = %v", err)
	}
	duplicateKey := []byte(`{"schemaVersion":1,"schemaVersion":1,"servers":[]}`)
	if _, err := Parse(duplicateKey); err == nil {
		t.Fatal("Parse(duplicate key) unexpectedly succeeded")
	}
	if _, err := Parse(append(encoded, []byte(` {}`)...)); err == nil {
		t.Fatal("Parse(multiple values) unexpectedly succeeded")
	}
}

func mustServer(t *testing.T, name string, arguments []string, surface Surface) Server {
	t.Helper()
	provenance := testProvenance()
	provenance.Arguments = append([]string{}, arguments...)
	if surface.Initialize == nil {
		surface.Initialize = testInitializeForSurface(surface)
	}
	server, err := NewServer(name, provenance, surface)
	if err != nil {
		t.Fatalf("NewServer(%q) error = %v", name, err)
	}
	return server
}

func testInitialize() json.RawMessage {
	return testInitializeForSurface(Surface{})
}

func testInitializeForSurface(surface Surface) json.RawMessage {
	capabilities := make(map[string]any)
	if surface.ToolsSupported {
		capabilities["tools"] = map[string]any{}
	}
	if surface.PromptsSupported {
		capabilities["prompts"] = map[string]any{}
	}
	if surface.ResourcesSupported {
		capabilities["resources"] = map[string]any{}
	}
	encoded, err := json.Marshal(map[string]any{
		"capabilities":    capabilities,
		"protocolVersion": "2025-06-18",
		"serverInfo":      map[string]any{"name": "fixture", "version": "1"},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func testProvenance() Provenance {
	return Provenance{
		Image:     testImage,
		Command:   []string{"/app/mcp-server"},
		Arguments: []string{},
		Discovery: Discovery{
			URL:      "http://127.0.0.1:8084/mcp",
			Protocol: "streamable-http",
		},
		Backend: Backend{
			Host:     "kagent-tools.kagent.svc.cluster.local",
			Port:     8084,
			Path:     "/mcp",
			Protocol: "StreamableHTTP",
		},
	}
}

func identities(entries []Entry) []string {
	values := make([]string, len(entries))
	for index, entry := range entries {
		values[index] = entry.Identity
	}
	return values
}

func cloneFile(file File) File {
	servers := make([]Server, len(file.Servers))
	for index, server := range file.Servers {
		servers[index] = cloneServer(server)
	}
	file.Servers = servers
	return file
}

func resourceTool(t *testing.T, name, description string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"name": name,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource_type": map[string]any{
					"type":        "string",
					"description": description,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("encode resource tool: %v", err)
	}
	return encoded
}

func resourceTypeDescription(identifiers []string) string {
	return "Type of resource to generate (" + strings.Join(identifiers, ", ") + ")"
}

func resourceTypeDescriptionFromObject(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var tool struct {
		InputSchema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("decode resource tool: %v", err)
	}
	return tool.InputSchema.Properties["resource_type"].Description
}
