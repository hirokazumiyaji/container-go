package bench

import (
	"strings"
	"testing"
)

// currentImages is the image set the scenarios run against. The
// scenarios moved to the ECR Docker Hub mirror to avoid anonymous pull
// rate limits, and the fixture drifted behind them: it still named
// redis:7-alpine, so nothing in the repo checked the fixture against the
// references the benchmarks actually use.
//
// The references are spelled out here rather than reused from
// scenario_test.go, which is behind the integration build tag and so is
// not compiled for a plain `go test` - the same reason this check does
// not run in CI today.
var currentImages = map[string]bool{
	"public.ecr.aws/docker/library/redis:7-alpine": true,
	"public.ecr.aws/docker/library/nginx:alpine":   true,
}

// TestFixtureUsesCurrentImageReferences keeps the fixture honest about
// the image set, which is what let a column-width regression go unnoticed
// (the 44-character reference is what overflowed the old 18-character
// field) and what makes the table's output comparable across runs.
func TestFixtureUsesCurrentImageReferences(t *testing.T) {
	doc, err := ParseDoc(readFixture(t, "result-doc.json"))
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}
	for i, r := range doc.Results {
		if !currentImages[r.Image] {
			t.Errorf("result[%d] image = %q, which is not one of the scenario images %v",
				i, r.Image, currentImages)
		}
	}
	// At least one of each scenario image, so the fixture actually
	// exercises a multi-image table.
	seen := map[string]bool{}
	for _, r := range doc.Results {
		seen[r.Image] = true
	}
	if len(seen) < 2 {
		t.Errorf("fixture covers %d image(s); the scenarios use %d", len(seen), len(currentImages))
	}
}

// TestFixtureTableColumnsAlign runs the fixture through the renderer, so
// the committed data is checked against the column layout rather than
// only against the JSON schema.
func TestFixtureTableColumnsAlign(t *testing.T) {
	doc, err := ParseDoc(readFixture(t, "result-doc.json"))
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}
	table := Table(Summarize(doc.Results))
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("table has %d lines:\n%s", len(lines), table)
	}
	width := len(lines[0])
	for i, line := range lines {
		if len(line) != width {
			t.Errorf("line %d has width %d, header %d:\n%s", i, len(line), width, table)
		}
	}
}

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
