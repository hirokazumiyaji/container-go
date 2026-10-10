// Package strictjson decodes the JSON arrays backend CLIs emit, strictly
// enough to tell a value the CLI did not report from output this library
// cannot read.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Array decodes data as a JSON array of T and reports an error, not a
// zero value, for output that is unusable:
//
//   - a top-level null instead of an array,
//   - a null element,
//   - an explicit null in one of required, a dotted path into a nested
//     object.
//
// encoding/json decodes a JSON null into a zero value, which erases the
// difference between a field the CLI omitted and one that leaves the
// entry unreadable. A caller that cannot read an entry must not treat the
// entry as absent: a delete that verified nothing would then pass as a
// removal that happened.
//
// An absent field is not a violation, so a non-container inspect object —
// one that carries no container state — still decodes, and so do the
// nullable maps and arrays real output uses for empty collections. Only
// list the fields a caller reads to identify and interpret an entry.
func Array[T any](data []byte, required []string) ([]T, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, fmt.Errorf("expected a JSON array, got null")
	}
	values := make([]T, 0, len(entries))
	for i, entry := range entries {
		if IsNull(entry) {
			return nil, fmt.Errorf("entry %d: expected an object, got null", i)
		}
		if field, bad := nullField(entry, required); bad {
			return nil, fmt.Errorf("entry %d: field %q must not be null", i, field)
		}
		var v T
		if err := json.Unmarshal(entry, &v); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		values = append(values, v)
	}
	return values, nil
}

// IsNull reports whether raw is the JSON null literal.
func IsNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// nullField reports the first required field present with a JSON null
// value, named by its dotted path.
func nullField(entry json.RawMessage, required []string) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil {
		// Not a JSON object; the typed decode reports the real error.
		return "", false
	}
	for _, field := range required {
		if nullAt(obj, field) {
			return field, true
		}
	}
	return "", false
}

func nullAt(obj map[string]json.RawMessage, path string) bool {
	key, rest, _ := strings.Cut(path, ".")
	raw, ok := obj[key]
	if !ok {
		return false
	}
	if rest == "" {
		return IsNull(raw)
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(raw, &child); err != nil {
		// Not a nested object; the typed decode reports the real error.
		return false
	}
	return nullAt(child, rest)
}
