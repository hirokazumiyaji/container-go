package strictjson

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type entry struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State *struct {
		Status string `json:"Status"`
	} `json:"State"`
}

var required = []string{"Id", "Name", "State", "State.Status"}

func TestArrayDecodesEntries(t *testing.T) {
	data := []byte(`[{"Id":"a","Name":"/x","State":{"Status":"running"}}]`)
	got, err := Array[entry](data, required)
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" || got[0].State == nil || got[0].State.Status != "running" {
		t.Fatalf("got = %+v", got)
	}
}

func TestArrayAllowsAbsentFields(t *testing.T) {
	// A network or volume entry carries no container State; skipping it is
	// the caller's decision, not a schema failure.
	got, err := Array[entry]([]byte(`[{"Id":"a","Name":"x"}]`), required)
	if err != nil {
		t.Fatalf("Array: %v", err)
	}
	if len(got) != 1 || got[0].State != nil {
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
	}{
		{name: "malformed", data: `{not json`, syntax: true},
		{name: "object instead of array", data: `{"Id":"a"}`, typeErr: true},
		{name: "string instead of array", data: `"a"`, typeErr: true},
		{name: "top-level null", data: `null`, want: "expected a JSON array, got null"},
		{name: "null entry", data: `[null]`, want: "entry 0: expected an object, got null"},
		{name: "null entry after a valid one", data: `[{"Id":"a"},null]`, want: "entry 1: expected an object, got null"},
		{name: "null entry before a valid one", data: `[null,{"Id":"a"}]`, want: "entry 0: expected an object, got null"},
		{name: "null scalar field", data: `[{"Id":null}]`, want: `entry 0: field "Id" must not be null`},
		{name: "null nested field", data: `[{"State":{"Status":null}}]`, want: `entry 0: field "State.Status" must not be null`},
		{name: "null object field", data: `[{"Id":"a","State":null}]`, want: `entry 0: field "State" must not be null`},
		{name: "string element", data: `["a"]`, typeErr: true},
		{name: "null element typed", data: `[{"Id":123}]`, typeErr: true},
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
		})
	}
}

// The strictness is opt-in per field: a nullable map or array a CLI emits
// for an empty collection must still decode.
func TestArrayAllowsNullForUnlistedFields(t *testing.T) {
	type loose struct {
		Labels map[string]string   `json:"Labels"`
		Ports  map[string][]string `json:"Ports"`
		State  *struct {           //nolint:unused // shape only
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
