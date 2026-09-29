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
	// makeIntegration matches the make targets that drive a container backend.
	makeIntegration = regexp.MustCompile(`\bmake\s+(?:bench-)?integration\b`)
	// dockerOrContainerCmd matches a direct docker/container CLI invocation.
	dockerOrContainerCmd = regexp.MustCompile(`^\s*(?:docker|container)\s`)
	// runStepKey matches a step's run: key, with or without the list dash.
	runStepKey = regexp.MustCompile(`^(\s*)(?:-\s*)?run:\s*(.*)$`)
	// blockScalar marks a YAML block scalar introducer for run:.
	blockScalar = regexp.MustCompile(`^[|>][-+]?$`)
	// jobIfKey matches a job-level if: at four-space indentation.
	jobIfKey = regexp.MustCompile(`^    if:\s*(.*)$`)
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

// Every job needs an explicit timeout. GitHub Actions has no workflow-level
// timeout that applies to jobs, so timeout-minutes before jobs: — including
// in a comment — does not cover them. Without a per-job timeout a hang
// occupies a runner until the six-hour default.
func TestEveryJobHasTimeout(t *testing.T) {
	for _, path := range workflowFiles(t) {
		lines := readLines(t, path)
		for _, job := range jobs(lines) {
			if !jobHasTimeout(lines, job) {
				t.Errorf("%s: job %q has no timeout-minutes", path, job)
			}
		}
	}
}

// A timeout-minutes mention before jobs: must not exempt any job.
func TestTimeoutBeforeJobsDoesNotExemptJobs(t *testing.T) {
	lines := []string{
		"# timeout-minutes: 10",
		"name: example",
		"on: push",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: echo hi",
		"  lint:",
		"    runs-on: ubuntu-latest",
		"    timeout-minutes: 5",
		"    steps:",
		"      - run: echo hi",
	}
	var missing []string
	for _, job := range jobs(lines) {
		if !jobHasTimeout(lines, job) {
			missing = append(missing, job)
		}
	}
	if len(missing) != 1 || missing[0] != "build" {
		t.Fatalf("jobs missing timeout = %v, want [build]", missing)
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

// trustedTriggerIf is the exact allowlist every Docker-touching job must use.
// Trusted events are enumerated rather than negated: under pull_request_target
// or workflow_run a fork's code is still untrusted, so a "not pull_request"
// test would pass them through.
const trustedTriggerIf = "github.event_name == 'push' || github.event_name == 'workflow_dispatch' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)"

// A job that hands a container backend to the checked-out code must not run on
// a fork pull request, where that code is untrusted.
func TestUntrustedDockerJobsAreGuarded(t *testing.T) {
	for _, path := range workflowFiles(t) {
		lines := readLines(t, path)
		for _, job := range jobs(lines) {
			body := jobBodyLines(lines, job)
			if !jobDrivesDocker(body) {
				continue
			}
			if !hasTrustedTrigger(body) {
				t.Errorf("%s: job %q drives a container backend without the trusted-trigger allowlist; "+
					"add a job-level 'if:' that trusts only push, workflow_dispatch, and a same-repo PR",
					path, job)
			}
		}
	}
}

func TestHasTrustedTriggerRejectsNegatedPullRequest(t *testing.T) {
	// The previously unsafe form admits pull_request_target / workflow_run.
	body := []string{
		"    if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: make integration-docker",
	}
	if hasTrustedTrigger(body) {
		t.Fatal("negated pull_request condition must not pass as a trusted trigger")
	}
}

func TestHasTrustedTriggerAcceptsAllowlist(t *testing.T) {
	body := []string{
		"    if: >-",
		"      github.event_name == 'push' || github.event_name == 'workflow_dispatch' ||",
		"      (github.event_name == 'pull_request' &&",
		"      github.event.pull_request.head.repo.full_name == github.repository)",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - run: make integration-docker",
	}
	if !hasTrustedTrigger(body) {
		t.Fatal("folded allowlist if: must be accepted")
	}
}

func TestJobDrivesDockerDetectsMultilineRun(t *testing.T) {
	body := []string{
		"    steps:",
		"      - name: Integrate",
		"        run: |",
		"          make integration",
		"          echo done",
	}
	if !jobDrivesDocker(body) {
		t.Fatal("block-scalar run with make integration must be classified as Docker usage")
	}
}

func TestJobDrivesDockerDetectsInlineRun(t *testing.T) {
	body := []string{
		"    steps:",
		"      - run: make bench-integration",
	}
	if !jobDrivesDocker(body) {
		t.Fatal("inline make bench-integration must be classified as Docker usage")
	}
}

func TestJobDrivesDockerIgnoresCommentOnly(t *testing.T) {
	body := []string{
		"    # make integration runs locally",
		"    steps:",
		"      - run: go test ./...",
	}
	if jobDrivesDocker(body) {
		t.Fatal("comment mentioning make integration must not count as Docker usage")
	}
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	// Both extensions: GitHub accepts .yml and .yaml, and a workflow added
	// with the latter must not escape every rule below.
	seen := map[string]bool{}
	var out []string
	for _, ext := range []string{"*.yml", "*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(workflowDir, ext))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no workflow files found under %s", workflowDir)
	}
	return out
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
	return strings.Join(jobBodyLines(lines, job), "\n") + "\n"
}

func jobBodyLines(lines []string, job string) []string {
	var out []string
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
			out = append(out, line)
		}
	}
	return out
}

func jobHasTimeout(lines []string, job string) bool {
	return strings.Contains(jobBody(lines, job), "timeout-minutes:")
}

// hasTrustedTrigger reports whether the job-level if: matches the trusted
// allowlist exactly (after collapsing whitespace). A same-repo substring
// alone is not enough: a negated pull_request test would still pass it.
func hasTrustedTrigger(body []string) bool {
	cond, ok := jobIfCondition(body)
	if !ok {
		return false
	}
	return normalizeExpr(cond) == normalizeExpr(trustedTriggerIf)
}

// jobIfCondition extracts the job-level if: value, including folded/block
// scalar continuations. Step-level if: keys are ignored (they sit under steps:).
func jobIfCondition(body []string) (string, bool) {
	inSteps := false
	for i, line := range body {
		trimmed := strings.TrimSpace(line)
		if trimmed == "steps:" {
			inSteps = true
			continue
		}
		if inSteps {
			continue
		}
		m := jobIfKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.TrimSpace(m[1])
		if blockScalar.MatchString(value) {
			var parts []string
			baseIndent := leadingSpaces(line)
			for _, cont := range body[i+1:] {
				if strings.TrimSpace(cont) == "" {
					continue
				}
				if leadingSpaces(cont) <= baseIndent {
					break
				}
				parts = append(parts, strings.TrimSpace(cont))
			}
			return strings.Join(parts, " "), true
		}
		return value, true
	}
	return "", false
}

func normalizeExpr(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// jobDrivesDocker reports whether any run: step in the job body invokes a
// container backend. Inline and block-scalar (multiline) run values are both
// inspected so `run: |` / `make integration` is not missed.
func jobDrivesDocker(body []string) bool {
	for i := 0; i < len(body); i++ {
		m := runStepKey.FindStringSubmatch(body[i])
		if m == nil {
			continue
		}
		indent, value := m[1], strings.TrimSpace(m[2])
		if blockScalar.MatchString(value) {
			baseIndent := len(indent)
			for j := i + 1; j < len(body); j++ {
				cont := body[j]
				if strings.TrimSpace(cont) == "" {
					continue
				}
				if leadingSpaces(cont) <= baseIndent {
					break
				}
				script := strings.TrimSpace(cont)
				if makeIntegration.MatchString(script) || dockerOrContainerCmd.MatchString(script) {
					return true
				}
			}
			continue
		}
		if makeIntegration.MatchString(value) || dockerOrContainerCmd.MatchString(value) {
			return true
		}
	}
	return false
}

func leadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' {
			break
		}
		n++
	}
	return n
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
