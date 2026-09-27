// Package workflowcheck holds the repository's CI workflow policy as a test.
//
// The rules live in Go rather than only in CI so they are checked by
// `go test ./...` on a developer machine, not just after a push.
package workflowcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const workflowDir = "../../.github/workflows"

var (
	// usesLine matches a `uses:` step, capturing the reference.
	usesLine = regexp.MustCompile(`uses:\s*(\S+)`)
	// jobLine matches a job key at two-space indentation.
	jobLine = regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_-]+):\s*$`)
	// shaRef matches an action pinned to a full commit SHA.
	shaRef = regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	// localAction is a reference to a path inside this repository, which has
	// no upstream commit to pin.
	localAction = regexp.MustCompile(`^\./`)
)

// Every third-party action must be pinned to a full commit SHA. A mutable tag
// means the same commit of this repository can be checked out by different
// code, so a compromised or force-pushed upstream tag changes what CI runs.
func TestActionsArePinnedToCommitSHA(t *testing.T) {
	for _, path := range workflowFiles(t) {
		for i, line := range readLines(t, path) {
			m := usesLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := m[1]
			if localAction.MatchString(ref) {
				continue
			}
			if !shaRef.MatchString(ref) {
				t.Errorf("%s:%d: action %q is not pinned to a full commit SHA", path, i+1, ref)
				continue
			}
			// A pin is unreadable without the tag it came from.
			if !strings.Contains(line, "#") {
				t.Errorf("%s:%d: action %q has no version comment to show which release it is", path, i+1, ref)
			}
		}
	}
}

// Every job needs an explicit timeout. Without one a hang occupies a runner
// until the six-hour default, which is far longer than any job here should
// take.
func TestEveryJobHasTimeout(t *testing.T) {
	for _, path := range workflowFiles(t) {
		lines := readLines(t, path)
		// A workflow-level default covers jobs that omit it, so a workflow
		// carrying one is not required to repeat it per job.
		if hasWorkflowLevelTimeout(lines) {
			continue
		}
		for _, job := range jobs(lines) {
			if !jobHasTimeout(lines, job) {
				t.Errorf("%s: job %q has no timeout-minutes", path, job)
			}
		}
	}
}

// checkout must not leave the token in .git/config, where any later step
// (including code under test) could read it.
func TestCheckoutDoesNotPersistCredentials(t *testing.T) {
	for _, path := range workflowFiles(t) {
		lines := readLines(t, path)
		for i, line := range lines {
			if !strings.Contains(line, "actions/checkout@") {
				continue
			}
			if !blockHas(lines, i, "persist-credentials: false") {
				t.Errorf("%s:%d: checkout does not set persist-credentials: false", path, i+1)
			}
		}
	}
}

// A job that gives a Docker daemon to the workflow must not run on a fork
// pull request, where the code under test is untrusted.
func TestUntrustedDockerJobsAreGuarded(t *testing.T) {
	for _, path := range workflowFiles(t) {
		lines := readLines(t, path)
		for _, job := range jobs(lines) {
			body := jobBody(lines, job)
			if !strings.Contains(body, "integration-docker") {
				continue
			}
			guard := "github.event.pull_request.head.repo.full_name == github.repository"
			if !strings.Contains(body, guard) {
				t.Errorf("%s: job %q runs the Docker integration with no fork guard; add 'if: ... %s'",
					path, job, guard)
			}
		}
	}
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no workflow files found under %s", workflowDir)
	}
	return matches
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}

// jobs returns the job keys of a workflow. The top-level keys before the
// first job are on:, permissions:, concurrency:, and env.
func jobs(lines []string) []string {
	inJobs := false
	var out []string
	for _, line := range lines {
		if line == "jobs:" {
			inJobs = true
			continue
		}
		if !inJobs {
			continue
		}
		if m := jobLine.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

func jobBody(lines []string, job string) string {
	var b strings.Builder
	inJob := false
	for _, line := range lines {
		if m := jobLine.FindStringSubmatch(line); m != nil {
			if inJob {
				break
			}
			inJob = m[1] == job
			continue
		}
		if inJob {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func jobHasTimeout(lines []string, job string) bool {
	return strings.Contains(jobBody(lines, job), "timeout-minutes:")
}

func hasWorkflowLevelTimeout(lines []string) bool {
	inJobs := false
	for _, line := range lines {
		if line == "jobs:" {
			inJobs = true
			continue
		}
		if !inJobs {
			if strings.Contains(line, "timeout-minutes:") {
				return true
			}
			continue
		}
		if jobLine.MatchString(line) {
			return false
		}
	}
	return false
}

// blockHas reports whether one of the next few lines after i contains want.
func blockHas(lines []string, i int, want string) bool {
	for _, line := range lines[i+1:] {
		if usesLine.MatchString(line) {
			return false
		}
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
