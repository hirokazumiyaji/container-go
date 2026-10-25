package container

import (
	"reflect"
	"strings"
	"testing"
)

const fuzzInspectTarget = "myctr"

// fuzzDockerInspectID is a full container ID: parseInspect rejects a name
// match whose Id is not the 64-character lowercase form Docker prints.
var fuzzDockerInspectID = strings.Repeat("a", 64)

// parseInspectFuzzSeeds covers the output shapes the two backends emit and the
// malformed output the strict decoders must reject. The fuzz target below runs
// these seeds under ordinary `go test`; CI does not run a timed fuzz job.
var parseInspectFuzzSeeds = []struct {
	name string
	data string
}{
	{"empty output", ""},
	{"whitespace only", " \n\t "},
	{"not JSON", `{not json`},
	{"top-level null", `null`},
	{"empty array", `[]`},
	{"null entry", `[null]`},
	{"empty object", `[{}]`},
	{"truncated array", `[{"id":"myctr"`},
	{"wrong field types", `[{"Id":42,"Name":true,"State":"running"}]`},
	{"docker running", `[{"Id":"` + fuzzDockerInspectID + `","Name":"/myctr","State":{"Status":"running"}}]`},
	{"docker missing state", `[{"Id":"` + fuzzDockerInspectID + `","Name":"/myctr"}]`},
	{"docker null state", `[{"Id":"` + fuzzDockerInspectID + `","Name":"/myctr","State":null}]`},
	{"docker empty status", `[{"Id":"` + fuzzDockerInspectID + `","Name":"/myctr","State":{"Status":" "}}]`},
	{"docker malformed ID", `[{"Id":"not-a-full-id","Name":"/myctr","State":{"Status":"running"}}]`},
	{"docker network before container", `[{"Id":"` + strings.Repeat("b", 64) + `","Name":"/myctr","Driver":"bridge"},` +
		`{"Id":"` + fuzzDockerInspectID + `","Name":"/myctr","State":{"Status":"running"}}]`},
	{"apple running", `[{"id":"myctr","configuration":{},"status":{"state":"running"}}]`},
	{"apple missing state", `[{"id":"myctr","configuration":{},"status":{}}]`},
	{"apple null status", `[{"id":"myctr","configuration":{},"status":null}]`},
	{"apple null configuration", `[{"id":"myctr","configuration":null,"status":{"state":"running"}}]`},
	{"invalid UTF-8", string([]byte{0xff, 0xfe, '[', ']'})},
}

// TestParseInspectSeedCorpus makes the fuzz seeds named table tests. Every
// input must parse deterministically: the same result, or the same error.
// Malformed shapes are seeded so the fuzz corpus exercises the strict
// decoders' error paths rather than only valid inspect output.
func TestParseInspectSeedCorpus(t *testing.T) {
	for _, seed := range parseInspectFuzzSeeds {
		t.Run(seed.name, func(t *testing.T) {
			checkParseInspectInvariants(t, dockerEngine{}, []byte(seed.data))
			checkParseInspectInvariants(t, appleEngine{}, []byte(seed.data))
		})
	}
}

func FuzzParseInspect(f *testing.F) {
	for _, seed := range parseInspectFuzzSeeds {
		f.Add([]byte(seed.data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip("inspect output above the fuzz size cap")
		}
		checkParseInspectInvariants(t, dockerEngine{}, data)
		checkParseInspectInvariants(t, appleEngine{}, data)
	})
}

// checkParseInspectInvariants runs the same input twice. A parser that
// accepted an entry the first time but not the second would make a lifecycle
// decision depend on decode order rather than the backend's answer.
func checkParseInspectInvariants(t *testing.T, eng engine, data []byte) {
	t.Helper()
	first, firstErr := eng.parseInspect(data, fuzzInspectTarget)
	second, secondErr := eng.parseInspect(data, fuzzInspectTarget)
	if (firstErr == nil) != (secondErr == nil) {
		t.Fatalf("%s parseInspect is not deterministic: %v then %v", eng.name(), firstErr, secondErr)
	}
	if firstErr != nil {
		if firstErr.Error() != secondErr.Error() {
			t.Fatalf("%s parseInspect error changed between calls: %v then %v", eng.name(), firstErr, secondErr)
		}
		return
	}
	if first == nil || second == nil {
		t.Fatalf("%s parseInspect returned nil info without an error", eng.name())
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("%s parseInspect returned different info for the same input: %+v then %+v", eng.name(), first, second)
	}
	if strings.TrimPrefix(first.name, "/") != fuzzInspectTarget {
		t.Errorf("%s parseInspect accepted a match named %q, want %q", eng.name(), first.name, fuzzInspectTarget)
	}
	if eng.name() == "docker" && !dockerIDRE.MatchString(first.uid) {
		t.Errorf("docker parseInspect accepted a match with invalid ID %q", first.uid)
	}
}
