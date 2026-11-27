// Package strictjson decodes the JSON arrays backend CLIs emit, strictly
// enough to tell a value the CLI did not report from output this library
// cannot read.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNull reports a JSON null where a required value must decode.
var ErrNull = errors.New("must not be null")

// NonNull decodes a T and rejects an explicit JSON null. A field declared
// with NonNull is required: an omitted field keeps the zero value, while a
// present-but-null field fails the decode.
//
// Encoding/json matches struct tags case-insensitively and applies duplicate
// keys in document order before calling UnmarshalJSON, so a null cannot
// bypass the check through another spelling or a later duplicate key. The
// check runs in the same decode that fills the surrounding struct, so no
// second pass over the entry is needed to find nulls.
type NonNull[T any] struct {
	Value T
}

func (n *NonNull[T]) UnmarshalJSON(data []byte) error {
	if IsNull(data) {
		return ErrNull
	}
	return json.Unmarshal(data, &n.Value)
}

// MarshalJSON keeps a NonNull field's encoded shape identical to T.
func (n NonNull[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.Value)
}

// Array decodes data as a JSON array of T and reports an error, not a
// zero value, for output that is unusable:
//
//   - a top-level null instead of an array,
//   - a null element,
//   - an explicit null in a required field, or on the dotted path to one.
//     The diagnostic matches field names the way encoding/json matches
//     struct tags: an exact match or a case-insensitive one, and a later
//     duplicate key wins, as it does during decoding.
//
// T rejects nulls in the fields it requires during the typed decode, using
// NonNull. encoding/json decodes a JSON null into a zero value, which erases
// the difference between a field the CLI omitted and one that leaves the
// entry unreadable. A caller that cannot read an entry must not treat the
// entry as absent: a delete that verified nothing would then pass as a
// removal that happened.
//
// required names those fields for the error message. It is consulted only
// after the decode fails, because NonNull.UnmarshalJSON sees only its own
// value and cannot name the field; a successful entry is never re-decoded.
// An absent field is not a violation, so a non-container inspect object —
// one that carries no container state — still decodes, and so do the
// nullable maps and arrays real output uses for empty collections.
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
		// A required path can name a field the target type does not spell
		// with NonNull, such as a plain pointer or a dotted path into one.
		// Only an entry that actually contains the null literal can be
		// affected, so the ordered walk runs on that minority of entries and
		// never repeats the map decode normal output used to pay for.
		if bytes.Contains(entry, jsonNullLiteral) {
			if field, bad := nullField(entry, required); bad {
				return nil, fmt.Errorf("entry %d: field %q %w", i, field, ErrNull)
			}
		}
		var v T
		if err := json.Unmarshal(entry, &v); err != nil {
			if errors.Is(err, ErrNull) {
				if field, bad := nullField(entry, required); bad {
					return nil, fmt.Errorf("entry %d: field %q %w", i, field, ErrNull)
				}
			}
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		values = append(values, v)
	}
	return values, nil
}

// jsonNullLiteral is the only byte sequence a JSON null value can occupy.
var jsonNullLiteral = []byte("null")

// IsNull reports whether raw is the JSON null literal.
func IsNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), jsonNullLiteral)
}

// jsonMember is one member of a JSON object, kept in document order.
type jsonMember struct {
	key   string
	value json.RawMessage
}

// objectMembers decodes data as a JSON object and returns its members in
// document order. encoding/json applies duplicate keys in that order, so a
// case-insensitive required-field match has to observe the same order to find
// the value that wins the decode.
func objectMembers(data []byte) ([]jsonMember, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, false
	}
	var members []jsonMember
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		members = append(members, jsonMember{key: key, value: raw})
	}
	return members, true
}

// memberValue returns the value of the last member that matches key the way
// encoding/json matches a struct tag: the exact spelling matches, and any
// other spelling matches case-insensitively. The last match wins because
// encoding/json assigns every matching key in input order.
func memberValue(members []jsonMember, key string) (json.RawMessage, bool) {
	var raw json.RawMessage
	found := false
	for _, member := range members {
		if member.key == key || strings.EqualFold(member.key, key) {
			raw = member.value
			found = true
		}
	}
	return raw, found
}

// nullField reports the first required field present with a JSON null
// value, named by its dotted path. It runs only on the error path of Array,
// so the ordered walk it needs for a precise diagnostic costs nothing on the
// success path.
func nullField(entry json.RawMessage, required []string) (string, bool) {
	members, ok := objectMembers(entry)
	if !ok {
		// Not a JSON object; the typed decode reports the real error.
		return "", false
	}
	for _, field := range required {
		if nullAt(members, field) {
			return field, true
		}
	}
	return "", false
}

func nullAt(members []jsonMember, path string) bool {
	key, rest, _ := strings.Cut(path, ".")
	raw, ok := memberValue(members, key)
	if !ok {
		return false
	}
	if rest == "" {
		return IsNull(raw)
	}
	// A null intermediate object makes every nested required field
	// unreadable even though the requested leaf cannot be looked up.
	if IsNull(raw) {
		return true
	}
	child, ok := objectMembers(raw)
	if !ok {
		// Not a nested object; the typed decode reports the real error.
		return false
	}
	return nullAt(child, rest)
}
