// Package docscheck keeps user-facing documentation from presenting a
// closed issue as a current limitation.
//
// The rule is a test rather than a checklist so `go test ./...` catches a
// regression without network access or the `gh` CLI. Release history and
// issue numbers belong in CHANGELOG.md, not in a current-behavior claim.
package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// documentationFiles are the files that describe current behavior. The
// CHANGELOG is deliberately absent: historical issue references there are
// legitimate.
var documentationFiles = []string{
	"README.md",
	"README.ja.md",
	"docs/design.md",
	"docs/design.ja.md",
	"doc.go",
}

// staleLimitationPatterns match the phrasing used when a document presents
// pending issue work as a present constraint. They are intentionally narrow
// so a historical note, a release name, or an ordinary sentence is not
// flagged.
var staleLimitationPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"tracks an issue", regexp.MustCompile(`(?i)\btracks?\s+#\d+`)},
	{"issue tracks work", regexp.MustCompile(`(?i)#\d+\s+tracks?\b`)},
	{"tracked by an issue", regexp.MustCompile(`(?i)\btracked by\s+#\d+`)},
	{"a change must be applied", regexp.MustCompile(`(?i)\bmust be applied\b`)},
	{"blocked until an issue", regexp.MustCompile(`(?i)\buntil\s+#\d+`)},
	{"pending an issue", regexp.MustCompile(`(?i)\bpending\s+#\d+`)},
	{"follow-up issue boundaries", regexp.MustCompile(`(?i)follow-up issue (?:branches|boundaries)`)},
	{"issue が担当", regexp.MustCompile(`#\d+\s*が担当`)},
	{"issue を適用", regexp.MustCompile(`#\d+\s*を適用`)},
	{"issue を参照", regexp.MustCompile(`#\d+\s*を参照`)},
	{"後続 issue", regexp.MustCompile(`後続\s*(?:issue|課題)`)},
}

func TestDocumentationDoesNotPresentClosedIssuesAsCurrentLimitations(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range documentationFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if pattern := staleLimitationPattern(line); pattern != "" {
				t.Errorf("%s:%d: %s; describe current behavior instead (historical issue references belong in CHANGELOG.md)",
					name, i+1, pattern)
			}
		}
	}
}

func TestStaleLimitationPatternsMatchExamples(t *testing.T) {
	stale := []string{
		"Apple log options are tracked by #82.",
		"#82 tracks Apple log options.",
		"#82 must be applied before Apple support is documented.",
		"Do not use it until #89 is applied.",
		"Public validation is pending #102.",
		"See the follow-up issue boundaries.",
		"#82 が担当します。",
		"#82 を適用するまで使わないでください。",
		"#82 を参照してください。",
		"後続 issue の制約です。",
	}
	for _, line := range stale {
		if pattern := staleLimitationPattern(line); pattern == "" {
			t.Errorf("stale phrasing was not detected: %q", line)
		}
	}

	// These are ordinary release notes or current-behavior sentences, not
	// closed-issue-as-limitation claims.
	allowed := []string{
		"Fixed in #123.",
		"Apple Container has no --since log option.",
		"The reaper registers Docker's full 64-hex immutable ID.",
	}
	for _, line := range allowed {
		if pattern := staleLimitationPattern(line); pattern != "" {
			t.Errorf("ordinary sentence was flagged as %q: %q", pattern, line)
		}
	}
}

func staleLimitationPattern(line string) string {
	for _, pattern := range staleLimitationPatterns {
		if pattern.re.MatchString(line) {
			return pattern.name
		}
	}
	return ""
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
