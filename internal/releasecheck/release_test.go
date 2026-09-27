// Package releasecheck holds the repository's release invariants as a test.
//
// They run in `go test ./...` rather than only in a release job, so version
// drift is caught on the change that introduces it instead of at tag time.
package releasecheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const repoRoot = "../.."

// changelogRelease matches a released section heading: "## [0.2.0] - 2026-09-02".
// Unreleased is deliberately excluded, so an unreleased entry cannot be
// mistaken for a release.
var changelogRelease = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\] - (\d{4}-\d{2}-\d{2})$`)

// installLine matches every version-bearing `go get` reference in a README.
// Both forms exist: the full install instruction, and the shorthand used in
// the pinning prose ("go get ...@v0.2.0"). Matching only the first would let
// the pinning advice drift while the check still passed.
var installLine = regexp.MustCompile(`go get (?:github\.com/hirokazumiyaji/container-go|\.\.\.)@(\S+)`)

// supportRow matches a SECURITY.md support-matrix row for a minor series.
var supportRow = regexp.MustCompile(`(?m)^\|\s*(\d+\.\d+)\.x\s*\|\s*(\w+)\s*\|$`)

func read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(data)
}

// latestRelease is the highest version with a dated CHANGELOG section. It is
// the reference every other version reference must agree with.
//
// Unexported because it takes a *testing.T, so it is only callable from a test
// and an exported name would promise a use that cannot exist.
func latestRelease(t *testing.T) (version, date string) {
	t.Helper()
	m := changelogRelease.FindAllStringSubmatch(read(t, "CHANGELOG.md"), -1)
	if len(m) == 0 {
		t.Fatal("CHANGELOG.md has no released version section")
	}
	// The file is newest-first, but do not rely on that ordering silently.
	best := m[0]
	for _, cand := range m {
		if compareVersions(cand[1], best[1]) > 0 {
			best = cand
		}
	}
	return best[1], best[2]
}

// Every documented install instruction must name the latest release. A README
// that still points at an older version sends `go get` users to old code that
// predates every fix since.
func TestReadmeInstallVersionMatchesLatestRelease(t *testing.T) {
	latest, _ := latestRelease(t)
	for _, readme := range []string{"README.md", "README.ja.md"} {
		matches := installLine.FindAllStringSubmatch(read(t, readme), -1)
		if len(matches) == 0 {
			t.Errorf("%s: no 'go get ...@version' install line found", readme)
			continue
		}
		for _, m := range matches {
			got := trimVersion(m[1])
			if got != "v"+latest {
				t.Errorf("%s: install line pins %s, want v%s (latest CHANGELOG release)", readme, got, latest)
			}
		}
	}
}

// SECURITY.md must claim support for the minor series that is actually
// released. This says nothing about the other rows: whether a project supports
// more than one series at a time is a policy choice, not a version-consistency
// invariant.
func TestSecuritySupportMatrixCoversLatestRelease(t *testing.T) {
	latest, _ := latestRelease(t)
	parts := strings.Split(latest, ".")
	if len(parts) < 2 {
		t.Fatalf("latest release %q is not major.minor.patch", latest)
	}
	minor := parts[0] + "." + parts[1]

	rows := supportRow.FindAllStringSubmatch(read(t, "SECURITY.md"), -1)
	if len(rows) == 0 {
		t.Fatal("SECURITY.md has no support-matrix rows")
	}
	supported := false
	found := false
	for _, r := range rows {
		if r[1] == minor {
			found = true
			supported = r[2] == "yes"
		}
	}
	if !found {
		t.Errorf("SECURITY.md: no support row for the released minor series %s.x", minor)
		return
	}
	if !supported {
		t.Errorf("SECURITY.md: released series %s.x is marked unsupported", minor)
	}
}

// The CHANGELOG's Unreleased section must exist, so a release cannot be cut
// with no record of what accumulated since the last one.
func TestChangelogHasUnreleasedSection(t *testing.T) {
	if !strings.Contains(read(t, "CHANGELOG.md"), "## [Unreleased]") {
		t.Error("CHANGELOG.md: missing the '## [Unreleased]' section")
	}
}

// Release sections must be dated, and dates must be real calendar dates. The
// heading regex only guarantees the shape, so `## [0.2.0] - 2026-13-45` matches
// it; checking the shape again would be vacuous, so the date is parsed.
func TestChangelogReleaseDatesAreValid(t *testing.T) {
	for _, m := range changelogRelease.FindAllStringSubmatch(read(t, "CHANGELOG.md"), -1) {
		version, date := m[1], m[2]
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			t.Errorf("CHANGELOG.md: release %s has an invalid date %q: %v", version, date, err)
		}
	}
	// A version heading with no date would not match changelogRelease at all,
	// so also assert none of the unparsed headings look like a release.
	for _, line := range strings.Split(read(t, "CHANGELOG.md"), "\n") {
		if !strings.HasPrefix(line, "## [") || strings.Contains(line, "Unreleased") {
			continue
		}
		if !changelogRelease.MatchString(line) {
			t.Errorf("CHANGELOG.md: release heading %q is missing a YYYY-MM-DD date", line)
		}
	}
}

// trimVersion strips the punctuation that follows a version inside prose, such
// as the closing backtick of `go get ...@v0.2.0` or a sentence-ending period.
func trimVersion(s string) string {
	return strings.Trim(s, "`\"'()[]{}.,;:。、」』")
}

// compareVersions orders dotted numeric versions. It returns >0 when a is
// newer than b.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}
