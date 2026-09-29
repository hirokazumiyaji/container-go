package bench

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// ecrMirrorImage is the current image reference for the redis scenario.
// The 44-character path is what broke the old fixed 18-character column:
// fmt's %-18s does not truncate, so the image overflowed its field and
// every later column on the row shifted.
const ecrMirrorImage = "public.ecr.aws/docker/library/redis:7-alpine"

// tableRows splits a rendered table into its lines, dropping the trailing
// empty line.
func tableRows(table string) []string {
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	return lines
}

// TestTableColumnsAlignWithCurrentImageReferences is the column-width
// regression. Every row must be the same rendered width, which is only
// true if each column is at least as wide as its longest value.
func TestTableColumnsAlignWithCurrentImageReferences(t *testing.T) {
	results := []Result{
		{
			Backend: "docker", Library: LibraryContainerGo, Image: ecrMirrorImage,
			Scenario: "run/warm", Iteration: 1, DurationNS: int64(300 * time.Millisecond), Subprocesses: 3,
		},
		{
			Backend: "docker", Library: LibraryTestcontainersGo, Image: ecrMirrorImage,
			Scenario: "tc/single", Iteration: 1, DurationNS: int64(900 * time.Millisecond), Subprocesses: 7,
		},
	}
	table := Table(Summarize(results))
	rows := tableRows(table)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (header plus two)", len(rows))
	}
	width := len(rows[0])
	for i, row := range rows {
		if len(row) != width {
			t.Errorf("row %d has width %d, header has %d; a column overflowed:\n%s",
				i, len(row), width, table)
		}
	}
	// The image must be rendered in full: truncating it would make two
	// different scenarios indistinguishable.
	if !strings.Contains(rows[1], ecrMirrorImage) {
		t.Errorf("image reference was not rendered in full:\n%s", table)
	}
}

// TestTableWidthsFollowTheData checks the width is derived rather than
// fixed, in both directions: a long image widens the column, and a short
// one does not inherit the widest reference from a previous table.
func TestTableWidthsFollowTheData(t *testing.T) {
	long := Table(Summarize([]Result{{
		Backend: "docker", Library: LibraryContainerGo, Image: ecrMirrorImage,
		Scenario: "run/warm", Iteration: 1, DurationNS: int64(300 * time.Millisecond), Subprocesses: 3,
	}}))
	short := Table(Summarize([]Result{{
		Backend: "docker", Library: LibraryContainerGo, Image: "redis",
		Scenario: "run/warm", Iteration: 1, DurationNS: int64(300 * time.Millisecond), Subprocesses: 3,
	}}))
	if len(tableRows(long)[0]) <= len(tableRows(short)[0]) {
		t.Errorf("a 44-character image did not widen the table (%d vs %d columns)",
			len(tableRows(long)[0]), len(tableRows(short)[0]))
	}
	// And the short table must not be padded out to the long one's width.
	if strings.Contains(short, ecrMirrorImage) {
		t.Errorf("the short table inherited the long table's width:\n%s", short)
	}
}

// TestTableShowsUnmeasuredSubprocessesAsNA is the second half of the
// issue. A scenario that does not count subprocesses records a zero,
// which the table used to print as "0" - indistinguishable from a
// measured zero, and the reason docs/benchmarks.md could compare an
// unmeasured container-go column against a measured
// testcontainers-go one.
func TestTableShowsUnmeasuredSubprocessesAsNA(t *testing.T) {
	table := Table(Summarize([]Result{
		{
			Backend: "docker", Library: LibraryContainerGo, Image: ecrMirrorImage,
			Scenario: "run/warm", Iteration: 1, DurationNS: int64(300 * time.Millisecond),
			// Not counted: the scenario never set Subprocesses.
		},
		{
			Backend: "docker", Library: LibraryTestcontainersGo, Image: ecrMirrorImage,
			Scenario: "tc/single", Iteration: 1, DurationNS: int64(900 * time.Millisecond), Subprocesses: 7,
		},
	}))
	rows := tableRows(table)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// Row 1 is the container-go entry, which did not measure.
	if !strings.HasSuffix(strings.TrimRight(rows[1], " "), unmeasuredSubprocesses) {
		t.Errorf("unmeasured spawn column = %q, want %q:\n%s", rows[1], unmeasuredSubprocesses, table)
	}
	// Row 2 was measured, so it keeps its number.
	if !strings.HasSuffix(strings.TrimRight(rows[2], " "), "7") {
		t.Errorf("measured spawn column = %q, want 7:\n%s", rows[2], table)
	}
	if bytes.Contains([]byte(table), []byte(" 0\n")) {
		t.Errorf("a bare 0 appears in the spawn column; unmeasured must not read as measured:\n%s", table)
	}
}

// TestTableHeaderStillFitsWhenThereAreNoResults keeps the header
// readable on an empty run, where there is no data to size columns from.
func TestTableHeaderStillFitsWhenThereAreNoResults(t *testing.T) {
	table := Table(nil)
	rows := tableRows(table)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	for _, want := range []string{"BACKEND", "LIBRARY", "IMAGE", "SCENARIO", "SPAWN"} {
		if !strings.Contains(rows[0], want) {
			t.Errorf("header missing %q: %q", want, rows[0])
		}
	}
}
