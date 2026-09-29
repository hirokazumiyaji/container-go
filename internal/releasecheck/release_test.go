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

// numericComponent is one canonical semver / Go module numeric part: a lone
// zero, or a non-zero digit optionally followed by more digits. Leading zeroes
// such as "03" are rejected so a typo like 0.03.0 cannot pass release checks
// while remaining unusable to Go tooling.
const numericComponent = `(?:0|[1-9]\d*)`

// changelogRelease matches a released section heading: "## [0.2.0] - 2026-09-02".
// Unreleased is deliberately excluded, so an unreleased entry cannot be
// mistaken for a release. Only canonical dotted versions match.
var changelogRelease = regexp.MustCompile(`(?m)^## \[(` + numericComponent + `\.` + numericComponent + `\.` + numericComponent + `)\] - (\d{4}-\d{2}-\d{2})$`)

// bareUnreleased matches a Keep a Changelog Unreleased heading on its own
// line. Trailing whitespace is allowed; a date suffix is not. A substring
// check for "## [Unreleased]" would accept "## [Unreleased] - 2026-09-29",
// which leaves no standalone Unreleased section for the next release cycle.
var bareUnreleased = regexp.MustCompile(`(?m)^## \[Unreleased\][ \t]*$`)

// installLine matches every version-bearing `go get` reference in a README.
// Both forms exist: the full install instruction, and the shorthand used in
// the pinning prose ("go get ...@v0.2.0"). Matching only the first would let
// the pinning advice drift while the check still passed.
var installLine = regexp.MustCompile(`go get (?:github\.com/hirokazumiyaji/container-go|\.\.\.)@(\S+)`)

// supportRow matches a SECURITY.md support-matrix row for a minor series.
var supportRow = regexp.MustCompile(`(?m)^\|\s*(` + numericComponent + `\.` + numericComponent + `)\.x\s*\|\s*(\w+)\s*\|$`)

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
func latestRelease(t *testing.T) string {
	t.Helper()
	m := changelogRelease.FindAllStringSubmatch(read(t, "CHANGELOG.md"), -1)
	if len(m) == 0 {
		t.Fatal("CHANGELOG.md has no released version section")
	}
	// The file is newest-first, but comparing rather than taking the first
	// section means a backport patch placed above a newer minor does not
	// become "latest". The workflow's tag check must agree with this.
	best := m[0]
	for _, cand := range m {
		if compareVersions(cand[1], best[1]) > 0 {
			best = cand
		}
	}
	return best[1]
}

// Every documented install instruction must name the latest release. A README
// that still points at an older version sends `go get` users to old code that
// predates every fix since.
func TestReadmeInstallVersionMatchesLatestRelease(t *testing.T) {
	latest := latestRelease(t)
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
	latest := latestRelease(t)
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
	if !bareUnreleased.MatchString(read(t, "CHANGELOG.md")) {
		t.Error("CHANGELOG.md: missing the '## [Unreleased]' section")
	}
}

// A dated Unreleased heading must not count as the required bare section.
// strings.Contains("...", "## [Unreleased]") used to accept it, and the
// date-validation loop skipped any line containing "Unreleased".
func TestBareUnreleasedHeading(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"## [Unreleased]\n", true},
		{"## [Unreleased] \n", true},
		{"## [Unreleased]\t\n", true},
		{"prefix\n## [Unreleased]\nsuffix\n", true},
		{"## [Unreleased] - 2026-09-29\n", false},
		{"## [Unreleased] - 2026-09-29\n## [0.1.0] - 2026-01-01\n", false},
		{"## [0.1.0] - 2026-01-01\n", false},
		{"# [Unreleased]\n", false},
		{"### [Unreleased]\n", false},
	}
	for _, tc := range cases {
		got := bareUnreleased.MatchString(tc.text)
		if got != tc.want {
			t.Errorf("bareUnreleased.MatchString(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// Release sections must be dated, and dates must be real calendar dates. The
// heading regex only guarantees the shape, so `## [0.2.0] - 2026-13-45` matches
// it; checking the shape again would be vacuous, so the date is parsed.
func TestChangelogReleaseDatesAreValid(t *testing.T) {
	changelog := read(t, "CHANGELOG.md")
	for _, m := range changelogRelease.FindAllStringSubmatch(changelog, -1) {
		version, date := m[1], m[2]
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			t.Errorf("CHANGELOG.md: release %s has an invalid date %q: %v", version, date, err)
		}
	}
	for _, err := range invalidChangelogHeadings(changelog) {
		t.Errorf("CHANGELOG.md: %s", err)
	}
}

// invalidChangelogHeadings reports ## [ headings that are neither a bare
// Unreleased section nor a dated canonical release.
func invalidChangelogHeadings(changelog string) []string {
	var errs []string
	for _, line := range strings.Split(changelog, "\n") {
		if !strings.HasPrefix(line, "## [") {
			continue
		}
		if bareUnreleased.MatchString(line) {
			continue
		}
		if strings.Contains(line, "Unreleased") {
			errs = append(errs, "Unreleased heading "+strconv.Quote(line)+" must be exactly '## [Unreleased]' (no date suffix)")
			continue
		}
		if !changelogRelease.MatchString(line) {
			errs = append(errs, "release heading "+strconv.Quote(line)+" is missing a YYYY-MM-DD date")
		}
	}
	return errs
}

func TestInvalidChangelogHeadingsRejectsDatedUnreleased(t *testing.T) {
	errs := invalidChangelogHeadings("## [Unreleased] - 2026-09-29\n## [0.1.0] - 2026-01-01\n")
	if len(errs) != 1 {
		t.Fatalf("got %d errors %v, want 1", len(errs), errs)
	}
	if !strings.Contains(errs[0], "## [Unreleased] - 2026-09-29") {
		t.Errorf("error %q does not mention the dated Unreleased heading", errs[0])
	}

	if got := invalidChangelogHeadings("## [Unreleased]\n## [0.1.0] - 2026-01-01\n"); len(got) != 0 {
		t.Errorf("bare Unreleased should be valid, got %v", got)
	}
	if got := invalidChangelogHeadings("## [Unreleased] \n"); len(got) != 0 {
		t.Errorf("bare Unreleased with trailing space should be valid, got %v", got)
	}
}

// Non-canonical versions (leading zeroes in a component) must not count as
// release headings. Go module tags require canonical form, so accepting
// "## [0.03.0] - ..." would let checks pass for a release tooling cannot use.
func TestChangelogRejectsNonCanonicalVersions(t *testing.T) {
	cases := []struct {
		heading string
		want    bool
	}{
		{"## [0.3.0] - 2026-09-02", true},
		{"## [0.0.0] - 2026-09-02", true},
		{"## [1.0.0] - 2026-09-02", true},
		{"## [10.20.30] - 2026-09-02", true},
		{"## [0.03.0] - 2026-09-02", false},
		{"## [00.3.0] - 2026-09-02", false},
		{"## [0.3.00] - 2026-09-02", false},
		{"## [01.2.3] - 2026-09-02", false},
	}
	for _, tc := range cases {
		got := changelogRelease.MatchString(tc.heading)
		if got != tc.want {
			t.Errorf("changelogRelease.MatchString(%q) = %v, want %v", tc.heading, got, tc.want)
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
