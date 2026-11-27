package strictjson

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type entry struct {
	ID    NonNull[string] `json:"Id"`
	Name  string          `json:"Name"`
	State NonNull[*struct {
		Status NonNull[string] `json:"Status"`
	}] `json:"State"`
}

var required = []string{"Id", "Name", "State", "State.Status"}

func TestArrayDecodesEntries(t *testing.T) {
	data := []byte(`[{"Id":"a","Name":"/x","State":{"Status":"running"}}]`)
	got, err := Array[entry](data, required)
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if len(got) != 1 || got[0].ID.Value != "a" || got[0].State.Value == nil || got[0].State.Value.Status.Value != "running" {
		t.Fatalf("got = %+v", got)
	}
}

func TestArrayAllowsAbsentFields(t *testing.T) {
	// A network or volume entry carries no container State; skipping it is
	// the caller's decision, not a schema failure. An absent NonNull field
	// keeps its zero value.
	got, err := Array[entry]([]byte(`[{"Name":"x"}]`), required)
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if len(got) != 1 || got[0].State.Value != nil {
		t.Fatalf("got = %+v", got)
	}
}

func TestArrayAcceptsEmptyArray(t *testing.T) {
	got, err := Array[entry]([]byte(`[]`), required)
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestArrayRejectsUnreadableOutput(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		want    string
		syntax  bool
		typeErr bool
		isNull  bool
	}{
		{name: "malformed", data: `{not json`, syntax: true},
		{name: "object instead of array", data: `{"Id":"a"}`, typeErr: true},
		{name: "string instead of array", data: `"a"`, typeErr: true},
		{name: "top-level null", data: `null`, want: "expected a JSON array, got null"},
		{name: "null entry", data: `[null]`, want: "entry 0: expected an object, got null"},
		{name: "null entry after a valid one", data: `[{"Id":"a"},null]`, want: "entry 1: expected an object, got null"},
		{name: "null entry before a valid one", data: `[null,{"Id":"a"}]`, want: "entry 0: expected an object, got null"},
		{name: "null scalar field", data: `[{"Id":null}]`, want: `entry 0: field "Id" must not be null`, isNull: true},
		{name: "null nested field", data: `[{"Id":"a","State":{"Status":null}}]`, want: `entry 0: field "State.Status" must not be null`, isNull: true},
		{name: "null object field", data: `[{"Id":"a","State":null}]`, want: `entry 0: field "State" must not be null`, isNull: true},
		{name: "string element", data: `["a"]`, typeErr: true},
		{name: "wrong scalar type", data: `[{"Id":123}]`, typeErr: true},
		{name: "nested field of wrong type", data: `[{"State":"running"}]`, typeErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Array[entry]([]byte(tc.data), required)
			if err == nil {
				t.Fatal("Array: want error, got nil")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			var syntaxErr *json.SyntaxError
			if got := errors.As(err, &syntaxErr); got != tc.syntax {
				t.Errorf("errors.As(*json.SyntaxError) = %t, want %t: %v", got, tc.syntax, err)
			}
			var typeErr *json.UnmarshalTypeError
			if got := errors.As(err, &typeErr); got != tc.typeErr {
				t.Errorf("errors.As(*json.UnmarshalTypeError) = %t, want %t: %v", got, tc.typeErr, err)
			}
			if got := errors.Is(err, ErrNull); got != tc.isNull {
				t.Errorf("errors.Is(ErrNull) = %t, want %t: %v", got, tc.isNull, err)
			}
		})
	}
}

// NonNull rejects the null during the typed decode; the required list only
// names the field, so an entry without a null is never decoded twice.
func TestArrayReportsGeneratedFieldNamesFromRequiredList(t *testing.T) {
	_, err := Array[entry]([]byte(`[{"Id":null}]`), nil)
	if err == nil || !errors.Is(err, ErrNull) {
		t.Fatalf("err = %v, want ErrNull without a required list", err)
	}
	if !strings.Contains(err.Error(), "must not be null") {
		t.Errorf("err = %v, want a null diagnostic", err)
	}
}

// NonNull makes required fields strict without a second decode: the same
// encoding/json pass that fills the struct calls UnmarshalJSON for a present
// field, including through a case-insensitive or duplicate spelling.
func TestArrayMatchesRequiredFieldsLikeEncodingJSON(t *testing.T) {
	_, err := Array[entry]([]byte(`[{"id":null}]`), required)
	if err == nil || !errors.Is(err, ErrNull) {
		t.Fatalf("err = %v, want a null rejection through a lower-case key", err)
	}

	_, err = Array[entry]([]byte(`[{"Id":"a","state":{"status":null}}]`), required)
	if err == nil || !errors.Is(err, ErrNull) {
		t.Fatalf("err = %v, want a nested null rejection through lower-case keys", err)
	}

	// A null spelling fails at the member that carries it, so no duplicate
	// key can smuggle a null past the check.
	for _, data := range []string{`[{"Id":null,"ID":"a"}]`, `[{"Id":"a","ID":null}]`} {
		if _, err := Array[entry]([]byte(data), required); !errors.Is(err, ErrNull) {
			t.Fatalf("Array(%s) error = %v, want ErrNull", data, err)
		}
	}
}

// A type whose required field is not declared NonNull still decodes: the
// strictness is opt-in per field, so nullable maps and arrays the CLI emits
// for an empty collection keep working.
func TestArrayAllowsNullForUnlistedFields(t *testing.T) {
	type loose struct {
		Labels map[string]string   `json:"Labels"`
		Ports  map[string][]string `json:"Ports"`
		State  *struct {          
			Status string `json:"Status"`
		} `json:"State"`
	}
	data := []byte(`[{"Labels":null,"Ports":{"80/tcp":null},"State":{"Status":"running"}}]`)
	got, err := Array[loose](data, []string{"State"})
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if len(got) != 1 || got[0].Labels != nil || got[0].State == nil {
		t.Fatalf("got = %+v", got)
	}
}

// A NonNull value round-trips in its field's normal JSON shape.
func TestNonNullMarshalDecodesAsValue(t *testing.T) {
	type wrapped struct {
		ID NonNull[string] `json:"Id"`
	}
	encoded, err := json.Marshal(wrapped{ID: NonNull[string]{Value: "a"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(encoded) != `{"Id":"a"}` {
		t.Fatalf("Marshal = %s, want %s", encoded, `{"Id":"a"}`)
	}
}

func TestIsNull(t *testing.T) {
	if !IsNull(json.RawMessage("null")) || !IsNull(json.RawMessage("  null\n")) {
		t.Error("IsNull(null): want true")
	}
	for _, raw := range []string{`"null"`, `{}`, `0`, ``, `"x"`} {
		if IsNull(json.RawMessage(raw)) {
			t.Errorf("IsNull(%q) = true, want false", raw)
		}
	}
}

func TestArrayMatchesRequiredFieldsCaseInsensitively(t *testing.T) {
	// encoding/json decodes {"id": null} into the zero value for the Id
	// field, so the null rejection must resolve field names the same way.
	_, err := Array[entry]([]byte(`[{"id":null,"Name":"/x","State":{"Status":"running"}}]`), required)
	if err == nil {
		t.Fatal("Array accepted a null Id through a lower-case key")
	}
	if !strings.Contains(err.Error(), `field "Id" must not be null`) {
		t.Errorf("err = %v, want the Id null rejection", err)
	}

	_, err = Array[entry]([]byte(`[{"Id":"a","state":{"status":null}}]`), []string{"State.Status"})
	if err == nil {
		t.Fatal("Array accepted a null nested field through lower-case keys")
	}
	if !strings.Contains(err.Error(), `field "State.Status" must not be null`) {
		t.Errorf("err = %v, want the State.Status null rejection", err)
	}
}

func TestArrayRejectsNullIntermediateOfRequiredPath(t *testing.T) {
	type nested struct {
		State *struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	_, err := Array[nested]([]byte(`[{"State":null}]`), []string{"State.Status"})
	if err == nil {
		t.Fatal("Array accepted a required nested path whose parent object is null")
	}
	if !strings.Contains(err.Error(), `field "State.Status" must not be null`) {
		t.Errorf("err = %v, want the nested null rejection", err)
	}
}

func TestArrayResolvesDuplicateKeysInDocumentOrder(t *testing.T) {
	// encoding/json assigns each matching key in input order, so the later
	// null clears the pointer the earlier key populated.
	_, err := Array[entry]([]byte(`[{"STATE":{"Status":"running"},"state":null}]`), []string{"State"})
	if err == nil || !strings.Contains(err.Error(), `field "State" must not be null`) {
		t.Fatalf("err = %v, want the later case-insensitive null to win", err)
	}

	// The reverse order is still rejected: a required field is decoded
	// through NonNull, which fails as soon as any occurrence of the key is
	// null. That is stricter than encoding/json, which would let the later
	// non-null object win, and it is the fail-closed direction: an entry
	// that spells a required field as null is never treated as readable.
	if _, err := Array[entry]([]byte(`[{"state":null,"STATE":{"Status":"running"}}]`), []string{"State"}); err == nil {
		t.Fatal("Array accepted an entry with a null occurrence of a required field")
	}

	// The same rule applies to a dotted required path.
	_, err = Array[entry]([]byte(`[{"State":{"Status":"running"},"state":null}]`), []string{"State.Status"})
	if err == nil || !strings.Contains(err.Error(), `field "State.Status" must not be null`) {
		t.Fatalf("err = %v, want the later case-insensitive null parent to win", err)
	}
}
