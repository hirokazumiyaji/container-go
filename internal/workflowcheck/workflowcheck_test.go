// Package workflowcheck holds the repository's CI workflow policy as a test.
//
// The rules live in Go rather than only in CI so they are checked by
// `go test ./...` on a developer machine, not just after a push.
package workflowcheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// jobTimeoutKey matches a job-level timeout-minutes: at four-space indentation.
	jobTimeoutKey = regexp.MustCompile(`^    timeout-minutes:\s*`)
	// stepWithKey matches a step-level with: mapping key.
	stepWithKey = regexp.MustCompile(`^(\s*)with:\s*(?:#.*)?$`)
	// vetIntegrationVar captures the command the Makefile centralizes as
	// VET_INTEGRATION, which every tagged vet gate is expected to share.
	vetIntegrationVar = regexp.MustCompile(`^VET_INTEGRATION\s*:?=\s*(.+)$`)
	// buildTagsKey matches golangci-lint's run.build-tags mapping key.
	buildTagsKey = regexp.MustCompile(`^\s*build-tags:\s*$`)
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

// jobHasTimeout must require the four-space job-level key, not a comment,
// env value, or run-script occurrence of the same literal.
func TestJobHasTimeoutRequiresJobLevelKey(t *testing.T) {
	lines := []string{
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    # timeout-minutes: 10",
		"    env:",
		"      NOTE: timeout-minutes: 99",
		"    steps:",
		"      - run: echo timeout-minutes: 1",
		"  lint:",
		"    runs-on: ubuntu-latest",
		"    timeout-minutes: 5",
		"    steps:",
		"      - run: echo hi",
	}
	if jobHasTimeout(lines, "build") {
		t.Fatal("comment/env/run timeout-minutes must not count as a job timeout")
	}
	if !jobHasTimeout(lines, "lint") {
		t.Fatal("job-level timeout-minutes must count")
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
			if !checkoutWithHas(lines, i, "persist-credentials: false") {
				t.Errorf("%s:%d: checkout does not set persist-credentials: false", path, i+1)
			}
		}
	}
}

// persist-credentials must appear in checkout's own with: mapping. A later
// comment, run script, or unrelated step before the next uses: must not count.
func TestCheckoutWithRequiresPersistCredentialsInWith(t *testing.T) {
	missing := []string{
		"      - uses: actions/checkout@0123456789abcdef0123456789abcdef01234567",
		"      # persist-credentials: false",
		"      - run: echo persist-credentials: false",
		"      - uses: actions/setup-go@0123456789abcdef0123456789abcdef01234567",
	}
	if checkoutWithHas(missing, 0, "persist-credentials: false") {
		t.Fatal("literal outside checkout with: must not count")
	}

	commentOnly := []string{
		"      - uses: actions/checkout@0123456789abcdef0123456789abcdef01234567",
		"        with:",
		"          # persist-credentials: false",
		"          fetch-depth: 1",
	}
	if checkoutWithHas(commentOnly, 0, "persist-credentials: false") {
		t.Fatal("comment inside with: must not count as the option")
	}

	good := []string{
		"      - uses: actions/checkout@0123456789abcdef0123456789abcdef01234567",
		"        with:",
		"          persist-credentials: false",
		"      - uses: actions/setup-go@0123456789abcdef0123456789abcdef01234567",
	}
	if !checkoutWithHas(good, 0, "persist-credentials: false") {
		t.Fatal("with: persist-credentials: false must count")
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

// A step-level if: (or a comment naming the guard) must not count as the job
// fork guard. Kept from the cross-platform branch's parallel hardening;
// main's allowlist parser already ignores both, and this pins that.
func TestHasTrustedTriggerRejectsStepLevelOrComment(t *testing.T) {
	body := []string{
		"    # github.event.pull_request.head.repo.full_name == github.repository",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - if: github.event_name == 'push' || github.event_name == 'workflow_dispatch' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)",
		"        run: make integration-docker",
	}
	if hasTrustedTrigger(body) {
		t.Fatal("step-level if: or comment must not count as the job fork guard")
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

// The cross-platform legs run the tagged vet natively, which is the only way
// the Windows-only integration files get type-checked, but they cannot call
// `make vet-integration`: the Windows runner image ships no GNU Make. That
// leaves a second copy of the integration tag in the workflow, and Go ignores
// an unknown build tag and exits 0, so a rename that missed the copy would
// silently stop compiling the very files the step exists for. Compare the
// commands rather than trusting the cross-reference comment to stay honest.
func TestTaggedVetMatchesMakefileCommand(t *testing.T) {
	want := makefileVetIntegration(t)
	found := 0
	for _, path := range workflowFiles(t) {
		lines := readLines(t, path)
		for _, job := range jobs(lines) {
			for _, line := range jobBodyLines(lines, job) {
				m := runStepKey.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				cmd := strings.TrimSpace(m[2])
				if !strings.HasPrefix(cmd, "go vet -tags ") {
					continue
				}
				found++
				if cmd != want {
					t.Errorf("%s: job %q runs %q, want the Makefile VET_INTEGRATION command %q",
						path, job, cmd, want)
				}
			}
		}
	}
	if found == 0 {
		t.Fatalf("no tagged vet step found under %s; the Windows-only integration files would go unchecked", workflowDir)
	}
}

// .golangci.yml repeats the integration tag in run.build-tags so the lint job
// analyzes the tagged files. Nothing else ties that copy to the Makefile's, and
// Go ignores an unknown build tag, so a rename that missed this file would keep
// lint green while it quietly stopped analyzing the tagged code.
func TestGolangciBuildTagsMatchMakefileCommand(t *testing.T) {
	tag := vetIntegrationTag(t)
	if tags := golangciBuildTags(t); !slices.Contains(tags, tag) {
		t.Errorf(".golangci.yml: run.build-tags = %v, want it to contain %q from the Makefile VET_INTEGRATION command",
			tags, tag)
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
	// A Windows checkout has CRLF endings. These helpers compare whole lines
	// (`line == "jobs:"`) as well as anchoring patterns with $, so a trailing
	// CR would make them match nothing: every jobs()-based rule would pass
	// vacuously on the one platform some of them exist to guard.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.Split(text, "\n")
}

func makefileVetIntegration(t *testing.T) string {
	t.Helper()
	path := filepath.Join(workflowDir, "..", "..", "Makefile")
	for _, line := range readLines(t, path) {
		if m := vetIntegrationVar.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	t.Fatalf("%s: no VET_INTEGRATION assignment found", path)
	return ""
}

// vetIntegrationTag returns the build tag the VET_INTEGRATION command passes.
func vetIntegrationTag(t *testing.T) string {
	t.Helper()
	fields := strings.Fields(makefileVetIntegration(t))
	for i, field := range fields {
		if field == "-tags" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Fatalf("VET_INTEGRATION does not pass -tags: %q", strings.Join(fields, " "))
	return ""
}

// golangciBuildTags returns the entries under run.build-tags in .golangci.yml.
func golangciBuildTags(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(workflowDir, "..", "..", ".golangci.yml")
	lines := readLines(t, path)
	for i, line := range lines {
		if !buildTagsKey.MatchString(line) {
			continue
		}
		keyIndent := leadingSpaces(line)
		var out []string
		for _, next := range lines[i+1:] {
			trim := strings.TrimSpace(next)
			if trim == "" {
				continue
			}
			if leadingSpaces(next) <= keyIndent || !strings.HasPrefix(trim, "- ") {
				break
			}
			out = append(out, strings.TrimSpace(strings.TrimPrefix(trim, "- ")))
		}
		return out
	}
	t.Fatalf("%s: no run.build-tags key found", path)
	return nil
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
	for _, line := range jobBodyLines(lines, job) {
		if jobTimeoutKey.MatchString(line) {
			return true
		}
	}
	return false
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

// stepKeyIndent returns the indentation of sibling keys under the step that
// owns usesIdx. For `- uses: ...` that is the column of `uses`; for a bare
// `uses:` line (when name: carried the dash) it is the line's own indent.
func stepKeyIndent(usesLine string) int {
	trimmed := strings.TrimSpace(usesLine)
	if !strings.HasPrefix(trimmed, "-") {
		return leadingSpaces(usesLine)
	}
	dash := strings.Index(usesLine, "-")
	rest := usesLine[dash+1:]
	return dash + 1 + leadingSpaces(rest)
}

// checkoutWithHas reports whether the checkout step at usesIdx sets want
// inside its with: mapping. Later steps, comments, and run scripts are ignored.
func checkoutWithHas(lines []string, usesIdx int, want string) bool {
	keyIndent := stepKeyIndent(lines[usesIdx])
	listIndent := leadingSpaces(lines[usesIdx])
	inWith := false
	withIndent := -1
	for _, line := range lines[usesIdx+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := leadingSpaces(line)
		trim := strings.TrimSpace(line)

		// A new list item at the same (or outer) dash indent ends the step.
		if strings.HasPrefix(trim, "-") && indent <= listIndent {
			return false
		}
		if indent < keyIndent {
			return false
		}

		if !inWith {
			if indent == keyIndent && stepWithKey.MatchString(line) {
				inWith = true
				withIndent = indent
			}
			continue
		}

		if indent <= withIndent {
			return false
		}
		// Strip a trailing comment so only a real mapping entry counts.
		key := strings.TrimSpace(strings.SplitN(trim, "#", 2)[0])
		if key == want {
			return true
		}
	}
	return false
}

var (
	// toolVersionPin matches a `go run`-style pin to a released tool version.
	// Versions belong in tools/go.mod, where go.sum verifies the module tree;
	// a copy here or in a workflow is what the test below rejects.
	toolVersionPin = regexp.MustCompile(`(?:govulncheck|actionlint)@v\S+`)
	// integrationImageLine matches the exported image constants in
	// internal/integrationtest/images.go.
	integrationImageLine = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9]*Image)\s*=\s*"([^"]+)"`)
	// deadcodeAllowedLine matches one allowlist assignment in the Makefile.
	deadcodeAllowedLine = regexp.MustCompile(`^DEADCODE_ALLOWED(_BENCH)?\s*:=\s*(.+)$`)
)

func parseModRequires(t *testing.T, path string) map[string]string {
	t.Helper()
	lines := readLines(t, path)
	requires := make(map[string]string)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "v") {
			requires[fields[0]] = fields[1]
		}
	}
	return requires
}

func makefileText(t *testing.T) string {
	t.Helper()
	return strings.Join(readLines(t, filepath.Join("..", "..", "Makefile")), "\n")
}

// Dependabot tracks developer tool dependencies declared in tools/go.mod.
// govulncheck and actionlint have to be built from that module: `go run
// pkg@version` runs outside the current module, so the downloaded tree is
// never checked against tools/go.sum and the version Dependabot bumps can
// differ from the one CI executes.
func TestToolsAreBuiltFromToolsModule(t *testing.T) {
	toolsModPath := filepath.Join("..", "..", "tools", "go.mod")
	requires := parseModRequires(t, toolsModPath)
	for _, module := range []string{"golang.org/x/vuln", "github.com/rhysd/actionlint"} {
		if requires[module] == "" {
			t.Errorf("%s: missing require for %s", toolsModPath, module)
		}
	}

	makefile := makefileText(t)
	for _, pin := range toolVersionPin.FindAllString(makefile, -1) {
		t.Errorf("Makefile pins %q; build the tool from tools/go.mod instead", pin)
	}
	for _, want := range []string{
		"GOVULNCHECK_TOOL := golang.org/x/vuln/cmd/govulncheck",
		"ACTIONLINT_TOOL := github.com/rhysd/actionlint/cmd/actionlint",
		"$(GOVULNCHECK_TOOL)",
		"$(ACTIONLINT_TOOL)",
	} {
		if !strings.Contains(makefile, want) {
			t.Errorf("Makefile does not use the tools/go.mod tool build (%q)", want)
		}
	}

	for _, wf := range workflowFiles(t) {
		body := strings.Join(readLines(t, wf), "\n")
		for _, pin := range toolVersionPin.FindAllString(body, -1) {
			t.Errorf("%s pins %q; build the tool from tools/go.mod instead", wf, pin)
		}
	}
	ci := strings.Join(readLines(t, filepath.Join(workflowDir, "ci.yml")), "\n")
	for _, tool := range []string{
		"golang.org/x/vuln/cmd/govulncheck",
		"github.com/rhysd/actionlint/cmd/actionlint",
	} {
		if !strings.Contains(ci, tool) {
			t.Errorf("ci.yml no longer checks with %s", tool)
		}
	}
}

// actionlint's shellcheck integration is what checks the run: blocks as shell
// scripts. `-shellcheck=` turns it off, leaving only Actions-expression
// validation.
func TestActionlintDoesNotDisableShellcheck(t *testing.T) {
	paths := append(workflowFiles(t), filepath.Join("..", "..", "Makefile"))
	for _, path := range paths {
		if strings.Contains(strings.Join(readLines(t, path), "\n"), "-shellcheck=") {
			t.Errorf("%s disables actionlint's shellcheck integration", path)
		}
	}
}

// The integration suites import their image refs from
// internal/integrationtest/images.go, and ci.yml's Docker-image cache lists
// the same refs. Before this test the list existed three times with nothing
// checking the copies agreed, so a mirror or tag change could leave CI
// pre-pulling an image the tests no longer use.
func TestIntegrationDockerImageListIsSingleSource(t *testing.T) {
	images := integrationImageConstants(t)
	want := make([]string, 0, len(images))
	for _, ref := range images {
		if !strings.HasPrefix(ref, "public.ecr.aws/docker/library/") {
			t.Errorf("image %q is not a public ECR library mirror", ref)
		}
		want = append(want, ref)
	}
	slices.Sort(want)

	ci := strings.Join(readLines(t, filepath.Join(workflowDir, "ci.yml")), "\n")
	got := integrationWorkflowImages(t)
	if len(got) != len(want) {
		t.Fatalf("ci.yml target_images = %v, want %v", got, want)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("ci.yml target_images = %v, want %v from internal/integrationtest/images.go", got, want)
	}
	for _, ref := range want {
		if n := strings.Count(ci, ref); n != 1 {
			t.Errorf("ci.yml mentions %q %d times; the image list must be defined once", ref, n)
		}
	}

	// The Docker Hub fallback is the mirror path without its prefix, so the
	// two lists cannot drift.
	if !strings.Contains(ci, `dockerhub_prefix="public.ecr.aws/docker/library/"`) {
		t.Error("ci.yml does not define the Docker Hub mirror prefix")
	}
	if !strings.Contains(ci, "fallback_images+=(") {
		t.Error("ci.yml does not derive the fallback image list from target_images")
	}

	for _, rel := range []string{"integration_image_test.go", "integration_image_ext_test.go"} {
		body := strings.Join(readLines(t, filepath.Join("..", "..", rel)), "\n")
		for _, ref := range want {
			if strings.Contains(body, ref) {
				t.Errorf("%s restates %q; alias internal/integrationtest instead", rel, ref)
			}
		}
		for _, name := range []string{"RedisImage", "NginxImage", "AlpineImage"} {
			if !strings.Contains(body, "integrationtest."+name) {
				t.Errorf("%s does not alias integrationtest.%s", rel, name)
			}
		}
	}

	// The cache key must follow the image list, or a ref change reuses the
	// previous tarball.
	if !strings.Contains(ci, "hashFiles('internal/integrationtest/images.go')") {
		t.Error("ci.yml cache key does not hash internal/integrationtest/images.go")
	}
}

// actions/cache skips its post-job upload only on an exact key hit. A
// restore-keys partial hit restores an older tarball, and unless the script
// rewrites it after pulling what was missing, the job uploads the stale set
// under the new key and the cache never catches up.
func TestIntegrationDockerCacheRebuildsOnPartialHit(t *testing.T) {
	lines := readLines(t, filepath.Join(workflowDir, "ci.yml"))
	body := strings.Join(jobBodyLines(lines, "integration-docker"), "\n")
	for _, want := range []string{
		"id: cache-images",
		"CACHE_EXACT_HIT: ${{ steps.cache-images.outputs.cache-hit }}",
		`if [ "$CACHE_EXACT_HIT" != "true" ] || [ "$loaded" -ne 1 ]; then`,
		`docker save -o /tmp/docker-images.tar "${target_images[@]}"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("integration-docker job is missing %q", want)
		}
	}
	if strings.Contains(body, "if [ ! -f /tmp/docker-images.tar ]; then") {
		t.Error("integration-docker job only saves when the tarball is missing; a partial hit keeps the stale file")
	}
	// Primary and fallback pulls both need retries against a throttled
	// registry.
	for _, want := range []string{
		`pull_with_retry "$target"`,
		`pull_with_retry "$fallback"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("integration-docker job does not retry %s", want)
		}
	}
}

// The release gate runs `make release-check`, so its pull_request filter has
// to list every input that can flip a gate result. A missing entry means a PR
// touching that input runs no release check at all.
func TestReleaseCheckPathsCoverGateInputs(t *testing.T) {
	paths := releaseCheckPaths(t)
	for _, want := range []string{
		"CHANGELOG.md",
		"README.md",
		"README.ja.md",
		"SECURITY.md",
		"docs/**",
		"testdata/**",
		"internal/**/testdata/**",
		"go.mod",
		"go.sum",
		"bench/go.mod",
		"bench/go.sum",
		"**/*.go",
		"bench/**",
		"tools/**",
		"Makefile",
		".golangci.yml",
		".github/workflows/**",
		".github/dependabot.yml",
		"internal/releasecheck/**",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("release-check.yml pull_request.paths is missing %q", want)
		}
	}
}

// DEADCODE_ALLOWED documents functions that only an external caller can
// reach. An entry for a deleted function hides nothing, and an entry for a
// function in-repo tests reach would mask a regression. bench/ is a separate
// module, so its exported result-schema API needs its own checked list and the
// target has to run deadcode inside that module.
func TestDeadcodeAllowlistIsAnchoredAndCoversBench(t *testing.T) {
	makefile := makefileText(t)
	allowed := deadcodeAllowlist(t, makefile, "DEADCODE_ALLOWED")
	benchAllowed := deadcodeAllowlist(t, makefile, "DEADCODE_ALLOWED_BENCH")
	if len(allowed) == 0 || len(benchAllowed) == 0 {
		t.Fatalf("DEADCODE_ALLOWED = %v, DEADCODE_ALLOWED_BENCH = %v; both must be non-empty", allowed, benchAllowed)
	}

	for _, stale := range []string{"DiagnosticSecrets", "SecretValues"} {
		if slices.Contains(allowed, stale) {
			t.Errorf("DEADCODE_ALLOWED still names %q, which no longer exists", stale)
		}
	}
	// Unit tests reach these, so deadcode reports them as reachable; listing
	// them would suppress a future regression instead of documenting API.
	for _, reachable := range []string{"WithClientDockerConfig", "WithClientPlatform"} {
		if slices.Contains(allowed, reachable) {
			t.Errorf("DEADCODE_ALLOWED names %q, which in-repo code reaches", reachable)
		}
	}

	for _, name := range allowed {
		if !declaredFunction(t, filepath.Join("..", ".."), name) {
			t.Errorf("DEADCODE_ALLOWED names %q, which is not declared", name)
		}
	}
	for _, name := range benchAllowed {
		if !declaredFunction(t, filepath.Join("..", "..", "bench"), name) {
			t.Errorf("DEADCODE_ALLOWED_BENCH names %q, which is not declared in bench/", name)
		}
	}

	// The report is reduced to its symbol and compared as a whole token; a
	// substring match would let a longer name hide behind an allowed prefix.
	if !strings.Contains(makefile, "grep -Evx") {
		t.Error("deadcode target does not match allowlisted symbols as whole tokens")
	}
	if !strings.Contains(makefile, `(cd bench && "$$tmp/deadcode" -test -tags integration ./...`) {
		t.Error("deadcode target does not run the bench module")
	}
}

// integrationImageConstants returns name to ref for the exported constants in
// internal/integrationtest/images.go.
func integrationImageConstants(t *testing.T) map[string]string {
	t.Helper()
	lines := readLines(t, filepath.Join("..", "..", "internal", "integrationtest", "images.go"))
	out := make(map[string]string)
	for _, line := range lines {
		if m := integrationImageLine.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	if len(out) == 0 {
		t.Fatal("internal/integrationtest/images.go declares no image constants")
	}
	return out
}

// integrationWorkflowImages returns the refs in ci.yml's target_images array.
func integrationWorkflowImages(t *testing.T) []string {
	t.Helper()
	lines := readLines(t, filepath.Join(workflowDir, "ci.yml"))
	start := -1
	for i, line := range lines {
		if strings.Contains(line, "target_images=(") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("ci.yml: no target_images array")
	}
	var out []string
	for _, line := range lines[start+1:] {
		trim := strings.TrimSpace(line)
		if trim == ")" {
			return out
		}
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		out = append(out, strings.Trim(trim, `"'`))
	}
	t.Fatal("ci.yml: unterminated target_images array")
	return nil
}

// releaseCheckPaths returns the pull_request.paths entries of release-check.yml.
func releaseCheckPaths(t *testing.T) []string {
	t.Helper()
	lines := readLines(t, filepath.Join(workflowDir, "release-check.yml"))
	inPullRequest := false
	for i, line := range lines {
		if strings.TrimSuffix(line, " ") == "  pull_request:" {
			inPullRequest = true
			continue
		}
		if !inPullRequest || strings.TrimSpace(line) != "paths:" {
			continue
		}
		keyIndent := leadingSpaces(line)
		var out []string
		for _, next := range lines[i+1:] {
			trim := strings.TrimSpace(next)
			if trim == "" || strings.HasPrefix(trim, "#") {
				continue
			}
			if leadingSpaces(next) <= keyIndent || !strings.HasPrefix(trim, "- ") {
				break
			}
			out = append(out, strings.Trim(strings.TrimPrefix(trim, "- "), `"'`))
		}
		return out
	}
	t.Fatal("release-check.yml: no pull_request.paths list")
	return nil
}

// deadcodeAllowlist splits one Makefile allowlist assignment into its names.
func deadcodeAllowlist(t *testing.T, makefile, name string) []string {
	t.Helper()
	for _, line := range strings.Split(makefile, "\n") {
		m := deadcodeAllowedLine.FindStringSubmatch(line)
		if m == nil || "DEADCODE_ALLOWED"+m[1] != name {
			continue
		}
		var out []string
		for _, entry := range strings.Split(m[2], "|") {
			if entry = strings.TrimSpace(entry); entry != "" {
				out = append(out, entry)
			}
		}
		return out
	}
	t.Fatalf("Makefile: no %s assignment", name)
	return nil
}

// declaredFunction reports whether dir declares a top-level func with name.
func declaredFunction(t *testing.T, dir, name string) bool {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`)
	found := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if pattern.Match(data) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s for func %s: %v", dir, name, err)
	}
	return found
}

// Dependabot configuration must track the /tools directory.
func TestDependabotTracksToolsModule(t *testing.T) {
	dependabotPath := filepath.Join("..", "..", ".github", "dependabot.yml")
	content, err := os.ReadFile(dependabotPath)
	if err != nil {
		t.Fatalf("read dependabot.yml: %v", err)
	}
	if !strings.Contains(string(content), `directory: "/tools"`) {
		t.Fatalf("%s must track directory: \"/tools\"", dependabotPath)
	}
}
