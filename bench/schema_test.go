package bench

import (
	"testing"
)

// TestFixtureMatchesSchema validates the committed fixture against the
// result schema so field renames are caught without a backend.
func TestFixtureMatchesSchema(t *testing.T) {
	data := readFixture(t, "result-doc.json")
	doc, err := ParseDoc(data)
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}
	if doc.Env.OS == "" || doc.Env.Arch == "" || doc.Env.Go == "" {
		t.Errorf("env incomplete: %+v", doc.Env)
	}
	if len(doc.Results) == 0 {
		t.Fatal("fixture has no results")
	}
	for i, r := range doc.Results {
		if r.Backend == "" || r.Library == "" || r.Image == "" || r.Scenario == "" {
			t.Errorf("result[%d] missing identity fields: %+v", i, r)
		}
		if r.Iteration < 1 {
			t.Errorf("result[%d] iteration = %d, want >= 1", i, r.Iteration)
		}
		if r.DurationNS < 0 {
			t.Errorf("result[%d] duration is negative", i)
		}
		if r.Subprocesses < 0 {
			t.Errorf("result[%d] subprocesses is negative", i)
		}
	}
	table := Table(Summarize(doc.Results))
	if table == "" {
		t.Fatal("table output is empty")
	}
}
