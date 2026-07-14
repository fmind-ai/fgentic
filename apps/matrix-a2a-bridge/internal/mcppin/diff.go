package mcppin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/gowebpki/jcs"
)

// Operation describes how a JSON value changed between two valid pin files.
type Operation string

const (
	// OperationAdded reports a value that exists only in the observed pin.
	OperationAdded Operation = "added"
	// OperationRemoved reports a value that exists only in the expected pin.
	OperationRemoved Operation = "removed"
	// OperationChanged reports a value that differs between both pins.
	OperationChanged Operation = "changed"
)

// Change is one deterministic, leaf-level pin difference. Entry paths always include the server,
// surface kind, and stable MCP identity before descending recursively into the complete object.
type Change struct {
	Operation Operation       `json:"operation"`
	Path      string          `json:"path"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
}

// Compare validates both files and returns deterministic recursive JSON-path differences.
// Derived entry and surface hashes are not reported separately: validation proves them, while the
// object or support-bit change explains why they differ.
func Compare(before, after File) ([]Change, error) {
	if err := before.Validate(); err != nil {
		return nil, fmt.Errorf("validate before pin: %w", err)
	}
	if err := after.Validate(); err != nil {
		return nil, fmt.Errorf("validate after pin: %w", err)
	}

	beforeServers := serversByName(before.Servers)
	afterServers := serversByName(after.Servers)
	names := sortedMapKeys(beforeServers, afterServers)
	changes := make([]Change, 0)
	for _, name := range names {
		path := indexedPath("$.servers", name)
		beforeServer, beforeOK := beforeServers[name]
		afterServer, afterOK := afterServers[name]
		switch {
		case !beforeOK:
			value, err := canonicalValue(afterServer)
			if err != nil {
				return nil, fmt.Errorf("encode added server %q: %w", name, err)
			}
			changes = append(changes, Change{Operation: OperationAdded, Path: path, After: value})
		case !afterOK:
			value, err := canonicalValue(beforeServer)
			if err != nil {
				return nil, fmt.Errorf("encode removed server %q: %w", name, err)
			}
			changes = append(changes, Change{Operation: OperationRemoved, Path: path, Before: value})
		default:
			serverChanges, err := compareServer(path, beforeServer, afterServer)
			if err != nil {
				return nil, fmt.Errorf("compare server %q: %w", name, err)
			}
			changes = append(changes, serverChanges...)
		}
	}
	return changes, nil
}

func compareServer(path string, before, after Server) ([]Change, error) {
	changes := make([]Change, 0)
	provenanceChanges, err := compareValues(path+".provenance", before.Provenance, after.Provenance)
	if err != nil {
		return nil, fmt.Errorf("compare provenance: %w", err)
	}
	changes = append(changes, provenanceChanges...)
	if before.Initialize.SHA256 != after.Initialize.SHA256 {
		initializeChanges, err := compareRaw(
			path+".initialize",
			before.Initialize.Object,
			after.Initialize.Object,
		)
		if err != nil {
			return nil, fmt.Errorf("compare initialize result: %w", err)
		}
		changes = append(changes, initializeChanges...)
	}

	for _, pair := range []struct {
		kind   collectionKind
		before Collection
		after  Collection
	}{
		{toolsKind, before.Tools, after.Tools},
		{promptsKind, before.Prompts, after.Prompts},
		{resourcesKind, before.Resources, after.Resources},
		{resourceTemplatesKind, before.ResourceTemplates, after.ResourceTemplates},
	} {
		collectionChanges, err := compareCollection(path, pair.kind, pair.before, pair.after)
		if err != nil {
			return nil, err
		}
		changes = append(changes, collectionChanges...)
	}
	return changes, nil
}

func compareCollection(serverPath string, kind collectionKind, before, after Collection) ([]Change, error) {
	collectionPath := serverPath + "." + string(kind)
	changes := make([]Change, 0)
	if before.Supported != after.Supported {
		beforeValue, err := canonicalValue(before.Supported)
		if err != nil {
			return nil, fmt.Errorf("encode old %s support: %w", kind, err)
		}
		afterValue, err := canonicalValue(after.Supported)
		if err != nil {
			return nil, fmt.Errorf("encode new %s support: %w", kind, err)
		}
		changes = append(changes, Change{
			Operation: OperationChanged,
			Path:      collectionPath + ".supported",
			Before:    beforeValue,
			After:     afterValue,
		})
	}

	beforeEntries := entriesByIdentity(before.Entries)
	afterEntries := entriesByIdentity(after.Entries)
	identities := sortedMapKeys(beforeEntries, afterEntries)
	for _, identity := range identities {
		entryPath := indexedPath(collectionPath, identity)
		beforeEntry, beforeOK := beforeEntries[identity]
		afterEntry, afterOK := afterEntries[identity]
		switch {
		case !beforeOK:
			object, err := canonicalObject(afterEntry.Object)
			if err != nil {
				return nil, fmt.Errorf("canonicalize added %s entry %q: %w", kind, identity, err)
			}
			changes = append(changes, Change{
				Operation: OperationAdded,
				Path:      entryPath,
				After:     json.RawMessage(object),
			})
		case !afterOK:
			object, err := canonicalObject(beforeEntry.Object)
			if err != nil {
				return nil, fmt.Errorf("canonicalize removed %s entry %q: %w", kind, identity, err)
			}
			changes = append(changes, Change{
				Operation: OperationRemoved,
				Path:      entryPath,
				Before:    json.RawMessage(object),
			})
		case beforeEntry.SHA256 != afterEntry.SHA256:
			entryChanges, err := compareRaw(entryPath, beforeEntry.Object, afterEntry.Object)
			if err != nil {
				return nil, fmt.Errorf("compare %s entry %q: %w", kind, identity, err)
			}
			changes = append(changes, entryChanges...)
		}
	}
	return changes, nil
}

func compareValues(path string, before, after any) ([]Change, error) {
	beforeJSON, err := canonicalValue(before)
	if err != nil {
		return nil, fmt.Errorf("encode old value: %w", err)
	}
	afterJSON, err := canonicalValue(after)
	if err != nil {
		return nil, fmt.Errorf("encode new value: %w", err)
	}
	return compareRaw(path, beforeJSON, afterJSON)
}

func compareRaw(path string, before, after json.RawMessage) ([]Change, error) {
	beforeValue, err := decodeValue(before)
	if err != nil {
		return nil, fmt.Errorf("decode old value at %s: %w", path, err)
	}
	afterValue, err := decodeValue(after)
	if err != nil {
		return nil, fmt.Errorf("decode new value at %s: %w", path, err)
	}
	changes := make([]Change, 0)
	if err := compareDecoded(path, beforeValue, afterValue, &changes); err != nil {
		return nil, err
	}
	return changes, nil
}

func compareDecoded(path string, before, after any, changes *[]Change) error {
	if reflect.DeepEqual(before, after) {
		return nil
	}

	beforeObject, beforeIsObject := before.(map[string]any)
	afterObject, afterIsObject := after.(map[string]any)
	if beforeIsObject && afterIsObject {
		keys := sortedMapKeys(beforeObject, afterObject)
		for _, key := range keys {
			childPath := propertyPath(path, key)
			beforeChild, beforeOK := beforeObject[key]
			afterChild, afterOK := afterObject[key]
			switch {
			case !beforeOK:
				value, err := canonicalValue(afterChild)
				if err != nil {
					return fmt.Errorf("encode added value at %s: %w", childPath, err)
				}
				*changes = append(*changes, Change{Operation: OperationAdded, Path: childPath, After: value})
			case !afterOK:
				value, err := canonicalValue(beforeChild)
				if err != nil {
					return fmt.Errorf("encode removed value at %s: %w", childPath, err)
				}
				*changes = append(*changes, Change{Operation: OperationRemoved, Path: childPath, Before: value})
			default:
				if err := compareDecoded(childPath, beforeChild, afterChild, changes); err != nil {
					return err
				}
			}
		}
		return nil
	}

	beforeArray, beforeIsArray := before.([]any)
	afterArray, afterIsArray := after.([]any)
	if beforeIsArray && afterIsArray {
		length := max(len(beforeArray), len(afterArray))
		for index := range length {
			childPath := path + "[" + strconv.Itoa(index) + "]"
			switch {
			case index >= len(beforeArray):
				value, err := canonicalValue(afterArray[index])
				if err != nil {
					return fmt.Errorf("encode added value at %s: %w", childPath, err)
				}
				*changes = append(*changes, Change{Operation: OperationAdded, Path: childPath, After: value})
			case index >= len(afterArray):
				value, err := canonicalValue(beforeArray[index])
				if err != nil {
					return fmt.Errorf("encode removed value at %s: %w", childPath, err)
				}
				*changes = append(*changes, Change{Operation: OperationRemoved, Path: childPath, Before: value})
			default:
				if err := compareDecoded(childPath, beforeArray[index], afterArray[index], changes); err != nil {
					return err
				}
			}
		}
		return nil
	}

	beforeJSON, err := canonicalValue(before)
	if err != nil {
		return fmt.Errorf("encode old changed value at %s: %w", path, err)
	}
	afterJSON, err := canonicalValue(after)
	if err != nil {
		return fmt.Errorf("encode new changed value at %s: %w", path, err)
	}
	*changes = append(*changes, Change{
		Operation: OperationChanged,
		Path:      path,
		Before:    beforeJSON,
		After:     afterJSON,
	})
	return nil
}

func decodeValue(raw json.RawMessage) (any, error) {
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	return value, nil
}

func canonicalValue(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	return json.RawMessage(canonical), nil
}

func serversByName(servers []Server) map[string]Server {
	indexed := make(map[string]Server, len(servers))
	for _, server := range servers {
		indexed[server.Name] = server
	}
	return indexed
}

func entriesByIdentity(entries []Entry) map[string]Entry {
	indexed := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		indexed[entry.Identity] = entry
	}
	return indexed
}

func sortedMapKeys[Value any](left, right map[string]Value) []string {
	keys := make([]string, 0, len(left)+len(right))
	seen := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range right {
		if _, ok := seen[key]; ok {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func indexedPath(path, identity string) string {
	return path + "[" + strconv.Quote(identity) + "]"
}

func propertyPath(path, property string) string {
	if isIdentifier(property) {
		return path + "." + property
	}
	return indexedPath(path, property)
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if index == 0 {
			if character != '_' && !unicode.IsLetter(character) {
				return false
			}
			continue
		}
		if character != '_' && !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

// String returns a stable single-line description suitable for drift diagnostics.
func (change Change) String() string {
	switch change.Operation {
	case OperationAdded:
		return fmt.Sprintf("added %s = %s", change.Path, strings.TrimSpace(string(change.After)))
	case OperationRemoved:
		return fmt.Sprintf("removed %s = %s", change.Path, strings.TrimSpace(string(change.Before)))
	case OperationChanged:
		return fmt.Sprintf(
			"changed %s: %s -> %s",
			change.Path,
			strings.TrimSpace(string(change.Before)),
			strings.TrimSpace(string(change.After)),
		)
	default:
		return fmt.Sprintf("%s %s", change.Operation, change.Path)
	}
}
