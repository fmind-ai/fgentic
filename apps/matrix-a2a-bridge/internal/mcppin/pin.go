// Package mcppin builds and validates immutable snapshots of MCP server surfaces.
package mcppin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/gowebpki/jcs"
)

const (
	// CurrentSchemaVersion is the only pin-file schema understood by this package.
	CurrentSchemaVersion = 1
	sha256HexLength      = sha256.Size * 2
)

type collectionKind string

const (
	toolsKind             collectionKind = "tools"
	promptsKind           collectionKind = "prompts"
	resourcesKind         collectionKind = "resources"
	resourceTemplatesKind collectionKind = "resourceTemplates"
)

// File is a deterministic, reviewable MCP pin file.
type File struct {
	SchemaVersion int      `json:"schemaVersion"`
	Servers       []Server `json:"servers"`
}

// Surface is the collector-facing representation of an MCP server's advertised surface.
// Support is separate from entries because unsupported and supported-but-empty have different
// protocol meanings.
type Surface struct {
	Initialize                 json.RawMessage
	ToolsSupported             bool
	Tools                      []json.RawMessage
	PromptsSupported           bool
	Prompts                    []json.RawMessage
	ResourcesSupported         bool
	Resources                  []json.RawMessage
	ResourceTemplatesSupported bool
	ResourceTemplates          []json.RawMessage
}

// Server pins one named MCP server to its execution provenance, initialize response, and four
// list surfaces.
type Server struct {
	Name              string       `json:"name"`
	Provenance        Provenance   `json:"provenance"`
	Initialize        PinnedObject `json:"initialize"`
	Tools             Collection   `json:"tools"`
	Prompts           Collection   `json:"prompts"`
	Resources         Collection   `json:"resources"`
	ResourceTemplates Collection   `json:"resourceTemplates"`
	SurfaceSHA256     string       `json:"surfaceSha256"`
}

// PinnedObject retains one complete protocol object together with its JCS SHA-256.
type PinnedObject struct {
	SHA256 string          `json:"sha256"`
	Object json.RawMessage `json:"object"`
}

// Provenance binds a pin to the executable and routing configuration used to discover it.
type Provenance struct {
	Image     string    `json:"image"`
	Command   []string  `json:"command"`
	Arguments []string  `json:"arguments"`
	Discovery Discovery `json:"discovery"`
	Backend   Backend   `json:"backend"`
}

// Discovery identifies the MCP endpoint queried by the collector.
type Discovery struct {
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
}

// Backend identifies the configured network destination behind the MCP route.
type Backend struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Path     string `json:"path"`
	Protocol string `json:"protocol"`
}

// Collection records whether an MCP method is supported and, when supported, its pinned entries.
type Collection struct {
	Supported bool    `json:"supported"`
	Entries   []Entry `json:"entries"`
}

// Entry retains the complete MCP object together with its stable identity and JCS SHA-256.
type Entry struct {
	Identity string          `json:"identity"`
	SHA256   string          `json:"sha256"`
	Object   json.RawMessage `json:"object"`
}

// NewServer validates and canonicalizes a collector result. Collections and entries are sorted
// by stable identity; command and arguments deliberately retain their original order.
func NewServer(name string, provenance Provenance, surface Surface) (Server, error) {
	if err := validateServerName(name); err != nil {
		return Server{}, err
	}
	if err := provenance.Validate(); err != nil {
		return Server{}, fmt.Errorf("server %q: %w", name, err)
	}
	initialize, err := newPinnedObject(surface.Initialize)
	if err != nil {
		return Server{}, fmt.Errorf("server %q: initialize: %w", name, err)
	}

	tools, err := newCollection(toolsKind, surface.ToolsSupported, surface.Tools)
	if err != nil {
		return Server{}, fmt.Errorf("server %q: %w", name, err)
	}
	prompts, err := newCollection(promptsKind, surface.PromptsSupported, surface.Prompts)
	if err != nil {
		return Server{}, fmt.Errorf("server %q: %w", name, err)
	}
	resources, err := newCollection(resourcesKind, surface.ResourcesSupported, surface.Resources)
	if err != nil {
		return Server{}, fmt.Errorf("server %q: %w", name, err)
	}
	resourceTemplates, err := newCollection(
		resourceTemplatesKind,
		surface.ResourceTemplatesSupported,
		surface.ResourceTemplates,
	)
	if err != nil {
		return Server{}, fmt.Errorf("server %q: %w", name, err)
	}

	server := Server{
		Name:              name,
		Provenance:        cloneProvenance(provenance),
		Initialize:        initialize,
		Tools:             tools,
		Prompts:           prompts,
		Resources:         resources,
		ResourceTemplates: resourceTemplates,
	}
	if err := validateInitializeSurface(server); err != nil {
		return Server{}, fmt.Errorf("server %q: %w", name, err)
	}
	server.SurfaceSHA256, err = surfaceSHA256(server)
	if err != nil {
		return Server{}, fmt.Errorf("server %q: hash surface: %w", name, err)
	}
	return server, nil
}

// NewFile validates servers, rejects duplicate names, and returns them in deterministic order.
func NewFile(servers []Server) (File, error) {
	ordered := make([]Server, len(servers))
	for index, server := range servers {
		if err := server.Validate(); err != nil {
			return File{}, fmt.Errorf("server at index %d: %w", index, err)
		}
		ordered[index] = cloneServer(server)
	}
	slices.SortFunc(ordered, func(left, right Server) int {
		return strings.Compare(left.Name, right.Name)
	})
	for index := 1; index < len(ordered); index++ {
		if ordered[index-1].Name == ordered[index].Name {
			return File{}, fmt.Errorf("duplicate server %q", ordered[index].Name)
		}
	}
	return File{SchemaVersion: CurrentSchemaVersion, Servers: ordered}, nil
}

// Parse decodes a pin file strictly and validates every identity and derived hash.
func Parse(data []byte) (File, error) {
	if _, err := jcs.Transform(data); err != nil {
		return File{}, fmt.Errorf("pin file is not valid canonicalizable I-JSON: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return File{}, fmt.Errorf("decode pin file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return File{}, fmt.Errorf("decode pin file: multiple JSON values")
		}
		return File{}, fmt.Errorf("decode pin file trailer: %w", err)
	}
	if err := file.Validate(); err != nil {
		return File{}, err
	}
	for serverIndex := range file.Servers {
		server := &file.Servers[serverIndex]
		canonical, err := canonicalObject(server.Initialize.Object)
		if err != nil {
			return File{}, fmt.Errorf("canonicalize validated initialize object: %w", err)
		}
		server.Initialize.Object = canonical
		for _, item := range []struct {
			kind       collectionKind
			collection *Collection
		}{
			{toolsKind, &server.Tools},
			{promptsKind, &server.Prompts},
			{resourcesKind, &server.Resources},
			{resourceTemplatesKind, &server.ResourceTemplates},
		} {
			for entryIndex := range item.collection.Entries {
				canonical, err := canonicalObject(
					item.collection.Entries[entryIndex].Object,
				)
				if err != nil {
					return File{}, fmt.Errorf("canonicalize validated %s object: %w", item.kind, err)
				}
				item.collection.Entries[entryIndex].Object = canonical
			}
		}
	}
	return file, nil
}

// Validate rejects malformed, unsorted, duplicate, or self-inconsistent pin files.
func (file File) Validate() error {
	if file.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("schemaVersion = %d, want %d", file.SchemaVersion, CurrentSchemaVersion)
	}
	if file.Servers == nil {
		return fmt.Errorf("servers must be an array")
	}
	for index, server := range file.Servers {
		if index > 0 && file.Servers[index-1].Name >= server.Name {
			if file.Servers[index-1].Name == server.Name {
				return fmt.Errorf("duplicate server %q", server.Name)
			}
			return fmt.Errorf("servers are not sorted: %q precedes %q", file.Servers[index-1].Name, server.Name)
		}
		if err := server.Validate(); err != nil {
			return fmt.Errorf("server %q: %w", server.Name, err)
		}
	}
	return nil
}

// Validate verifies one server and every derived entry and surface hash.
func (server Server) Validate() error {
	if err := validateServerName(server.Name); err != nil {
		return err
	}
	if err := server.Provenance.Validate(); err != nil {
		return err
	}
	if err := validatePinnedObject("initialize", server.Initialize); err != nil {
		return err
	}
	for _, pair := range []struct {
		kind       collectionKind
		collection Collection
	}{
		{toolsKind, server.Tools},
		{promptsKind, server.Prompts},
		{resourcesKind, server.Resources},
		{resourceTemplatesKind, server.ResourceTemplates},
	} {
		if err := validateCollection(pair.kind, pair.collection); err != nil {
			return err
		}
	}
	if err := validateInitializeSurface(server); err != nil {
		return err
	}
	wantHash, err := surfaceSHA256(server)
	if err != nil {
		return fmt.Errorf("hash surface: %w", err)
	}
	if server.SurfaceSHA256 != wantHash {
		return fmt.Errorf("surfaceSha256 = %q, want %q", server.SurfaceSHA256, wantHash)
	}
	return nil
}

func newPinnedObject(object json.RawMessage) (PinnedObject, error) {
	canonical, err := canonicalObject(object)
	if err != nil {
		return PinnedObject{}, err
	}
	return PinnedObject{SHA256: digest(canonical), Object: canonical}, nil
}

func validatePinnedObject(field string, object PinnedObject) error {
	canonical, err := canonicalObject(object.Object)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if !validDigest(object.SHA256) {
		return fmt.Errorf("%s has invalid sha256 %q", field, object.SHA256)
	}
	wantHash := digest(canonical)
	if object.SHA256 != wantHash {
		return fmt.Errorf("%s sha256 = %q, want %q", field, object.SHA256, wantHash)
	}
	return nil
}

func validateInitializeSurface(server Server) error {
	initialize, ok, err := rawObject(server.Initialize.Object)
	if err != nil {
		return fmt.Errorf("initialize object: %w", err)
	}
	if !ok {
		return fmt.Errorf("initialize must be an object")
	}
	capabilities, ok, err := childObject(initialize, "capabilities")
	if err != nil {
		return fmt.Errorf("initialize capabilities: %w", err)
	}
	if !ok {
		return fmt.Errorf("initialize capabilities must be an object")
	}
	serverInfo, ok, err := childObject(initialize, "serverInfo")
	if err != nil {
		return fmt.Errorf("initialize serverInfo: %w", err)
	}
	if !ok {
		return fmt.Errorf("initialize serverInfo must be an object")
	}
	for _, field := range []struct {
		name      string
		supported bool
	}{
		{"tools", server.Tools.Supported},
		{"prompts", server.Prompts.Supported},
		{"resources", server.Resources.Supported},
	} {
		_, advertised := capabilities[field.name]
		if advertised {
			if _, ok, err := childObject(capabilities, field.name); err != nil {
				return fmt.Errorf("initialize capability %s: %w", field.name, err)
			} else if !ok {
				return fmt.Errorf("initialize capability %s must be an object", field.name)
			}
		}
		if advertised != field.supported {
			return fmt.Errorf(
				"initialize capability %s advertised = %t, collection supported = %t",
				field.name,
				advertised,
				field.supported,
			)
		}
	}
	if server.ResourceTemplates.Supported && !server.Resources.Supported {
		return fmt.Errorf("resourceTemplates cannot be supported when resources is unsupported")
	}
	if err := validateRequiredJSONString(initialize, "protocolVersion", "initialize protocolVersion"); err != nil {
		return err
	}
	if err := validateRequiredJSONString(serverInfo, "name", "initialize serverInfo name"); err != nil {
		return err
	}
	if err := validateRequiredJSONString(serverInfo, "version", "initialize serverInfo version"); err != nil {
		return err
	}
	if raw, exists := initialize["instructions"]; exists {
		var instructions string
		if err := json.Unmarshal(raw, &instructions); err != nil {
			return fmt.Errorf("initialize instructions must be a string")
		}
	}
	if _, exists := initialize["_meta"]; exists {
		if _, ok, err := childObject(initialize, "_meta"); err != nil {
			return fmt.Errorf("initialize _meta: %w", err)
		} else if !ok {
			return fmt.Errorf("initialize _meta must be an object")
		}
	}
	return nil
}

func validateRequiredJSONString(object map[string]json.RawMessage, field, label string) error {
	raw, exists := object[field]
	if !exists {
		return fmt.Errorf("%s is required", label)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s must be a string", label)
	}
	return validateNonEmptyField(label, value)
}

func newCollection(kind collectionKind, supported bool, objects []json.RawMessage) (Collection, error) {
	if !supported && len(objects) != 0 {
		return Collection{}, fmt.Errorf("%s is unsupported but has %d entries", kind, len(objects))
	}
	entries := make([]Entry, 0, len(objects))
	for index, object := range objects {
		entry, err := newEntry(kind, object)
		if err != nil {
			return Collection{}, fmt.Errorf("%s entry %d: %w", kind, index, err)
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(left, right Entry) int {
		return strings.Compare(left.Identity, right.Identity)
	})
	for index := 1; index < len(entries); index++ {
		if entries[index-1].Identity == entries[index].Identity {
			return Collection{}, fmt.Errorf("%s has duplicate identity %q", kind, entries[index].Identity)
		}
	}
	return Collection{Supported: supported, Entries: entries}, nil
}

func newEntry(kind collectionKind, object json.RawMessage) (Entry, error) {
	canonical, err := canonicalObject(object)
	if err != nil {
		return Entry{}, err
	}
	identity, err := objectIdentity(kind, canonical)
	if err != nil {
		return Entry{}, err
	}
	canonical, err = normalizeObject(kind, identity, canonical)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Identity: identity,
		SHA256:   digest(canonical),
		Object:   json.RawMessage(canonical),
	}, nil
}

func validateCollection(kind collectionKind, collection Collection) error {
	if collection.Entries == nil {
		return fmt.Errorf("%s entries must be an array", kind)
	}
	if !collection.Supported && len(collection.Entries) != 0 {
		return fmt.Errorf("%s is unsupported but has %d entries", kind, len(collection.Entries))
	}
	for index, entry := range collection.Entries {
		if index > 0 && collection.Entries[index-1].Identity >= entry.Identity {
			if collection.Entries[index-1].Identity == entry.Identity {
				return fmt.Errorf("%s has duplicate identity %q", kind, entry.Identity)
			}
			return fmt.Errorf(
				"%s entries are not sorted: %q precedes %q",
				kind,
				collection.Entries[index-1].Identity,
				entry.Identity,
			)
		}
		canonical, err := canonicalObject(entry.Object)
		if err != nil {
			return fmt.Errorf("%s entry %q: %w", kind, entry.Identity, err)
		}
		identity, err := objectIdentity(kind, canonical)
		if err != nil {
			return fmt.Errorf("%s entry %q: %w", kind, entry.Identity, err)
		}
		if entry.Identity != identity {
			return fmt.Errorf("%s entry identity = %q, object identity = %q", kind, entry.Identity, identity)
		}
		normalized, err := normalizeObject(kind, identity, canonical)
		if err != nil {
			return fmt.Errorf("%s entry %q: %w", kind, entry.Identity, err)
		}
		if !bytes.Equal(canonical, normalized) {
			return fmt.Errorf("%s entry %q object is not normalized", kind, entry.Identity)
		}
		if !validDigest(entry.SHA256) {
			return fmt.Errorf("%s entry %q has invalid sha256 %q", kind, entry.Identity, entry.SHA256)
		}
		wantHash := digest(canonical)
		if entry.SHA256 != wantHash {
			return fmt.Errorf("%s entry %q sha256 = %q, want %q", kind, entry.Identity, entry.SHA256, wantHash)
		}
	}
	return nil
}

func normalizeObject(kind collectionKind, identity string, canonical []byte) ([]byte, error) {
	if kind != toolsKind || identity != "k8s_generate_resource" {
		return canonical, nil
	}

	// kagent-tools builds this exact description by ranging over a Go map, so its nine
	// identifiers change order across otherwise identical processes. Keep the exception narrow:
	// any token, grammar, path, or tool-name change must still surface as reviewable drift.
	object, ok, err := rawObject(canonical)
	if err != nil || !ok {
		return canonical, err
	}
	inputSchema, ok, err := childObject(object, "inputSchema")
	if err != nil || !ok {
		return canonical, err
	}
	properties, ok, err := childObject(inputSchema, "properties")
	if err != nil || !ok {
		return canonical, err
	}
	resourceType, ok, err := childObject(properties, "resource_type")
	if err != nil || !ok {
		return canonical, err
	}
	descriptionRaw, ok := resourceType["description"]
	if !ok || len(descriptionRaw) == 0 || descriptionRaw[0] != '"' {
		return canonical, nil
	}
	var description string
	if err := json.Unmarshal(descriptionRaw, &description); err != nil {
		return nil, fmt.Errorf("decode k8s_generate_resource resource_type description: %w", err)
	}
	normalizedDescription, ok := normalizeResourceTypeDescription(description)
	if !ok || normalizedDescription == description {
		return canonical, nil
	}

	descriptionRaw, err = json.Marshal(normalizedDescription)
	if err != nil {
		return nil, fmt.Errorf("encode normalized resource_type description: %w", err)
	}
	resourceType["description"] = descriptionRaw
	if properties["resource_type"], err = json.Marshal(resourceType); err != nil {
		return nil, fmt.Errorf("encode normalized resource_type schema: %w", err)
	}
	if inputSchema["properties"], err = json.Marshal(properties); err != nil {
		return nil, fmt.Errorf("encode normalized tool properties: %w", err)
	}
	if object["inputSchema"], err = json.Marshal(inputSchema); err != nil {
		return nil, fmt.Errorf("encode normalized tool input schema: %w", err)
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode normalized tool: %w", err)
	}
	normalized, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize normalized tool: %w", err)
	}
	return normalized, nil
}

func childObject(parent map[string]json.RawMessage, field string) (map[string]json.RawMessage, bool, error) {
	raw, ok := parent[field]
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil, false, nil
	}
	object, ok, err := rawObject(raw)
	if err != nil {
		return nil, false, fmt.Errorf("decode %s object: %w", field, err)
	}
	return object, ok, nil
}

func rawObject(raw []byte) (map[string]json.RawMessage, bool, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false, fmt.Errorf("decode JSON object: %w", err)
	}
	return object, true, nil
}

func normalizeResourceTypeDescription(description string) (string, bool) {
	const (
		prefix          = "Type of resource to generate ("
		suffix          = ")"
		identifierCount = 9
	)
	if !strings.HasPrefix(description, prefix) || !strings.HasSuffix(description, suffix) {
		return description, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(description, prefix), suffix)
	identifiers := strings.Split(body, ", ")
	if len(identifiers) != identifierCount {
		return description, false
	}
	for _, identifier := range identifiers {
		if !isLowerSnakeCase(identifier) {
			return description, false
		}
	}
	slices.Sort(identifiers)
	return prefix + strings.Join(identifiers, ", ") + suffix, true
}

func isLowerSnakeCase(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' || value[len(value)-1] == '_' {
		return false
	}
	previousUnderscore := false
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			previousUnderscore = false
		case character == '_' && !previousUnderscore:
			previousUnderscore = true
		default:
			return false
		}
	}
	return true
}

func canonicalObject(raw json.RawMessage) ([]byte, error) {
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("object is not valid canonicalizable I-JSON: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil {
		return nil, fmt.Errorf("object must be a JSON object: %w", err)
	}
	if object == nil {
		return nil, fmt.Errorf("object must be a JSON object")
	}
	return canonical, nil
}

func objectIdentity(kind collectionKind, canonical []byte) (string, error) {
	field := "name"
	switch kind {
	case toolsKind, promptsKind:
	case resourcesKind:
		field = "uri"
	case resourceTemplatesKind:
		field = "uriTemplate"
	default:
		return "", fmt.Errorf("unknown collection kind %q", kind)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil {
		return "", fmt.Errorf("decode object identity: %w", err)
	}
	rawIdentity, ok := object[field]
	if !ok {
		return "", fmt.Errorf("object has no %q identity", field)
	}
	var identity string
	if err := json.Unmarshal(rawIdentity, &identity); err != nil {
		return "", fmt.Errorf("object %q identity is not a string", field)
	}
	if err := validateNonEmptyField(fmt.Sprintf("object %q identity", field), identity); err != nil {
		return "", err
	}
	return identity, nil
}

type digestEntry struct {
	Identity string `json:"identity"`
	SHA256   string `json:"sha256"`
}

type digestCollection struct {
	Supported bool          `json:"supported"`
	Entries   []digestEntry `json:"entries"`
}

type digestSurface struct {
	Initialize        string           `json:"initialize"`
	Tools             digestCollection `json:"tools"`
	Prompts           digestCollection `json:"prompts"`
	Resources         digestCollection `json:"resources"`
	ResourceTemplates digestCollection `json:"resourceTemplates"`
}

func surfaceSHA256(server Server) (string, error) {
	surface := digestSurface{
		Initialize:        server.Initialize.SHA256,
		Tools:             collectionDigest(server.Tools),
		Prompts:           collectionDigest(server.Prompts),
		Resources:         collectionDigest(server.Resources),
		ResourceTemplates: collectionDigest(server.ResourceTemplates),
	}
	encoded, err := json.Marshal(surface)
	if err != nil {
		return "", fmt.Errorf("encode surface digest: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return "", fmt.Errorf("canonicalize surface digest: %w", err)
	}
	return digest(canonical), nil
}

func collectionDigest(collection Collection) digestCollection {
	entries := make([]digestEntry, len(collection.Entries))
	for index, entry := range collection.Entries {
		entries[index] = digestEntry{Identity: entry.Identity, SHA256: entry.SHA256}
	}
	return digestCollection{Supported: collection.Supported, Entries: entries}
}

func validateServerName(name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("server name must be non-empty without surrounding whitespace")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("server name %q contains a control character", name)
		}
	}
	return nil
}

func validateImmutableImage(image string) error {
	const separator = "@sha256:"
	if strings.TrimSpace(image) != image || strings.Count(image, "@") != 1 {
		return fmt.Errorf("image %q must be an immutable name@sha256 digest", image)
	}
	prefix, hash, found := strings.Cut(image, separator)
	if !found || prefix == "" || strings.ContainsAny(prefix, " \t\r\n") || !validDigest(hash) {
		return fmt.Errorf("image %q must be an immutable name@sha256 digest", image)
	}
	return nil
}

// Validate verifies the complete execution and routing provenance.
func (provenance Provenance) Validate() error {
	if err := validateImmutableImage(provenance.Image); err != nil {
		return err
	}
	if len(provenance.Command) == 0 {
		return fmt.Errorf("command must be a non-empty array")
	}
	for index, value := range provenance.Command {
		if err := validateNonEmptyField("command["+strconv.Itoa(index)+"]", value); err != nil {
			return err
		}
	}
	if provenance.Arguments == nil {
		return fmt.Errorf("arguments must be an array")
	}
	for index, value := range provenance.Arguments {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("arguments[%d] contains a NUL character", index)
		}
	}

	discoveryURL, err := url.Parse(provenance.Discovery.URL)
	if err != nil || discoveryURL.Host == "" ||
		(discoveryURL.Scheme != "http" && discoveryURL.Scheme != "https") {
		return fmt.Errorf("discovery url %q must be an absolute http(s) URL", provenance.Discovery.URL)
	}
	if discoveryURL.User != nil || discoveryURL.Fragment != "" {
		return fmt.Errorf("discovery url must not contain credentials or a fragment")
	}
	if err := validateNonEmptyField("discovery protocol", provenance.Discovery.Protocol); err != nil {
		return err
	}

	if err := validateNonEmptyField("backend host", provenance.Backend.Host); err != nil {
		return err
	}
	if strings.ContainsAny(provenance.Backend.Host, " /\\?#@") {
		return fmt.Errorf("backend host %q is invalid", provenance.Backend.Host)
	}
	if provenance.Backend.Port < 1 || provenance.Backend.Port > 65535 {
		return fmt.Errorf("backend port %d is outside 1..65535", provenance.Backend.Port)
	}
	if err := validateNonEmptyField("backend path", provenance.Backend.Path); err != nil {
		return err
	}
	if !path.IsAbs(provenance.Backend.Path) {
		return fmt.Errorf("backend path %q must be absolute", provenance.Backend.Path)
	}
	if strings.ContainsAny(provenance.Backend.Path, "?#") {
		return fmt.Errorf("backend path %q must not contain a query or fragment", provenance.Backend.Path)
	}
	if err := validateNonEmptyField("backend protocol", provenance.Backend.Protocol); err != nil {
		return err
	}
	return nil
}

func validateNonEmptyField(field, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be non-empty without surrounding whitespace", field)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256HexLength {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func cloneServer(server Server) Server {
	server.Provenance = cloneProvenance(server.Provenance)
	server.Initialize.Object = slices.Clone(server.Initialize.Object)
	server.Tools = cloneCollection(server.Tools)
	server.Prompts = cloneCollection(server.Prompts)
	server.Resources = cloneCollection(server.Resources)
	server.ResourceTemplates = cloneCollection(server.ResourceTemplates)
	return server
}

func cloneProvenance(provenance Provenance) Provenance {
	provenance.Command = slices.Clone(provenance.Command)
	provenance.Arguments = slices.Clone(provenance.Arguments)
	return provenance
}

func cloneCollection(collection Collection) Collection {
	entries := make([]Entry, len(collection.Entries))
	for index, entry := range collection.Entries {
		entry.Object = slices.Clone(entry.Object)
		entries[index] = entry
	}
	collection.Entries = entries
	return collection
}
