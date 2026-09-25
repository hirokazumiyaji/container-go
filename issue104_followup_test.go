package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type issue104FollowupProbeRunner struct {
	probeErr    error
	probeStdout string
}

func (r *issue104FollowupProbeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && (args[0] == "version" || args[0] == "system") {
		return []byte(r.probeStdout), nil, r.probeErr
	}
	return nil, nil, nil
}

type issue104FollowupPruneRunner struct {
	deleteErr error
}

type issue104ReplacementDockerRunner struct {
	inspects int
	deleted  []string
}

func (r *issue104ReplacementDockerRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.inspects++
		return []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"running"},"Config":{"Labels":{%q:%q}},"NetworkSettings":{}}]`, strings.Repeat("b", 64), creationLabel, strings.Repeat("b", 16))), nil, nil
	case "rm":
		r.deleted = append(r.deleted, args[len(args)-1])
		return nil, nil, nil
	case "version":
		return []byte("version"), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *issue104FollowupPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && (args[0] == "ps" || args[0] == "ls") {
		return nil, nil, nil
	}
	if len(args) > 0 && (args[0] == "rm" || args[0] == "delete") {
		return nil, nil, r.deleteErr
	}
	return nil, nil, nil
}

func TestExec126ShortcutUsesSelectedCurrentBranch(t *testing.T) {
	unrelatedStatus := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "other", "app"}, ExitCode: 126,
		Stderr: "permission denied: unrelated workload",
	}
	selected := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "app"}, ExitCode: 23,
		Stderr: "permission denied: current workload",
	}
	f := &issue104JoinedExecRunner{
		fakeRunner: newTestRunner(),
		err:        errors.Join(unrelatedStatus, selected),
		stderr:     selected.Stderr,
	}
	ctr := runTestContainer(t, f)
	ctr.runner = f
	code, _, err := ctr.Exec(context.Background(), []string{"app"})
	if err != nil || code != 23 {
		t.Fatalf("Exec = %d/%v, want selected current workload result", code, err)
	}
}

func TestFailedDockerTerminateDoesNotPromoteReplacementUID(t *testing.T) {
	runner := &issue104ReplacementDockerRunner{}
	ctr := &Container{
		id: "myctr", creation: strings.Repeat("a", 16), runner: runner, eng: dockerEngine{},
	}
	for i := range 2 {
		err := ctr.Terminate(context.Background())
		if err == nil || !strings.Contains(err.Error(), "recreated") {
			t.Fatalf("Terminate call %d error = %v, want replacement refusal", i+1, err)
		}
	}
	if ctr.immutableUID() != "" {
		t.Fatalf("replacement UID was promoted: %s", ctr.immutableUID())
	}
	if len(runner.deleted) != 0 {
		t.Fatalf("replacement was deleted: %v", runner.deleted)
	}
	if runner.inspects != 2 {
		t.Fatalf("inspect calls = %d, want one generation check per Terminate", runner.inspects)
	}
}

func TestExecVerificationDoesNotPromoteUnprovenDockerUID(t *testing.T) {
	runner := &issue104ReplacementDockerRunner{}
	ctr := &Container{
		id: "myctr", creation: strings.Repeat("a", 16), runner: runner, eng: dockerEngine{},
	}
	verification := ctr.verifyExecContainer(context.Background())
	if verification.err != nil || verification.state != StateRunning {
		t.Fatalf("verification = %+v, want running replacement observation", verification)
	}
	if ctr.immutableUID() != "" {
		t.Fatalf("unverified replacement UID was promoted: %s", ctr.immutableUID())
	}
}

func TestInspectDetailJoinedWithSentinelIsOperationAbsence(t *testing.T) {
	detail := newInspectTargetNotFound("myctr", "inspect output did not contain the requested target")
	if !isNotFoundForOperation(dockerEngine{}, errors.Join(ErrContainerNotFound, detail), "inspect", "myctr") {
		t.Fatal("matching inspect detail was not treated as corroborating absence")
	}
	other := newInspectTargetNotFound("other", "inspect output did not contain the requested target")
	if isNotFoundForOperation(dockerEngine{}, errors.Join(ErrContainerNotFound, other), "inspect", "myctr") {
		t.Fatal("nonmatching inspect detail suppressed the requested target")
	}
	unrelated := errors.Join(ErrContainerNotFound, detail, errors.New("permission denied"))
	if isNotFoundForOperation(dockerEngine{}, unrelated, "inspect", "myctr") {
		t.Fatal("unrelated joined failure overrode inspect absence")
	}
}

func TestTerminateTreatsEmptyAndMismatchedInspectAsAbsent(t *testing.T) {
	otherUID := strings.Repeat("d", 64)
	cases := []struct {
		name string
		data string
	}{
		{name: "empty array", data: "[]"},
		{name: "nonmatching inspect", data: fmt.Sprintf(`[{"Id":%q,"Name":"/other","State":{"Status":"running"},"Config":{},"NetworkSettings":{}}]`, otherUID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &issue104FreshDockerRunner{inspectJSON: []byte(tc.data)}
			ctr := &Container{id: "myctr", creation: strings.Repeat("a", 16), runner: runner, eng: dockerEngine{}}
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Fatalf("Terminate with absent inspect result = %v, want nil", err)
			}
			if ctr.immutableUID() != "" || len(runner.calls) != 1 {
				t.Fatalf("absence promoted UID or issued extra calls: uid=%q calls=%v", ctr.immutableUID(), runner.calls)
			}
		})
	}
}

func TestPruneDeleteNotFoundIsOperationAndTargetSpecific(t *testing.T) {
	permission := &cli.CLIError{
		Binary: "docker", Args: []string{"rm", "--force", "target"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	inspectAbsence := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "target"}, ExitCode: 1,
		Stderr: "Error: no such object: target",
	}
	r := &issue104FollowupPruneRunner{deleteErr: errors.Join(inspectAbsence, permission)}
	removed, err := pruneListed(
		context.Background(), r, dockerEngine{}, []string{"ps"},
		func([]byte) ([]string, error) { return []string{"target"}, nil }, "prune",
	)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("prune error = %v, want rm permission failure", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want no removals", removed)
	}
}

func TestPruneDeleteHonorsSentinelWithoutCLIError(t *testing.T) {
	r := &issue104FollowupPruneRunner{deleteErr: ErrContainerNotFound}
	removed, err := pruneListed(
		context.Background(), r, dockerEngine{}, []string{"ps"},
		func([]byte) ([]string, error) { return []string{"target"}, nil }, "prune",
	)
	if err != nil || len(removed) != 1 || removed[0] != "target" {
		t.Fatalf("prune = %v/%v, want target treated as already absent", removed, err)
	}
}

func TestExecWorkloadStatuses126And127AreResults(t *testing.T) {
	for _, code := range []int{126, 127} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := &execRunner{
				fakeRunner: newTestRunner(),
				execStdout: "workload output\n",
				execErr: &cli.CLIError{
					Binary: "container", Args: []string{"exec", "myctr", "app"},
					ExitCode: code, Stderr: "permission denied",
				},
			}
			ctr := runTestContainer(t, f)
			gotCode, out, err := ctr.Exec(context.Background(), []string{"app"})
			if err != nil {
				t.Fatalf("Exec = %v, want normal workload result", err)
			}
			if gotCode != code || out == nil {
				t.Fatalf("code/output = %d/%v, want %d/output", gotCode, out, code)
			}
		})
	}
}

func TestBranchSelectionPrefersExactTargetOverTargetless(t *testing.T) {
	targetless := &cli.CLIError{
		Binary: "container", Args: []string{"rm", "--force"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	exact := &cli.CLIError{
		Binary: "container", Args: []string{"rm", "--force", "target"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	branch, ok := matchingCLIErrorBranch(errors.Join(targetless, exact), "container", "rm", "target")
	if !ok || branch.ctx.err != exact {
		t.Fatalf("selected branch = %#v, want exact target branch", branch.ctx.err)
	}
}

func TestOperationAbsencePrefersExactTargetBranches(t *testing.T) {
	targetlessAbsence := &cli.CLIError{
		Binary: "container", Args: []string{"delete", "--force"}, ExitCode: 1,
		Stderr: "Error: container with ID target not found",
	}
	exactPermission := &cli.CLIError{
		Binary: "container", Args: []string{"delete", "--force", "target"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	if isNotFoundForOperation(appleEngine{}, errors.Join(targetlessAbsence, exactPermission), "delete", "target") {
		t.Fatal("targetless absence suppressed exact permission branch")
	}
}

func TestOperationSpecificAbsenceHonorsSentinelWithoutCLIError(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", ErrContainerNotFound)
	if !isNotFoundForOperation(dockerEngine{}, err, "rm", "target") {
		t.Fatal("operation-specific absence ignored ErrContainerNotFound sentinel")
	}
}

func TestContainerNotFoundSentinelDoesNotOverrideConflictingBranches(t *testing.T) {
	conflicting := errors.Join(ErrContainerNotFound, errors.New("application failed"))
	if isNotFoundForOperation(dockerEngine{}, conflicting, "rm", "target") ||
		isNotFoundFor(dockerEngine{}, conflicting) {
		t.Fatal("bare sentinel fast path overrode a conflicting joined failure")
	}
	unrelatedAbsence := errors.Join(ErrContainerNotFound, &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "other"}, ExitCode: 1,
		Stderr: "Error: no such object: other",
	})
	if isNotFoundForOperation(dockerEngine{}, unrelatedAbsence, "rm", "target") {
		t.Fatal("sentinel plus unrelated inspect absence was treated as rm absence")
	}
	matchingAbsence := errors.Join(ErrContainerNotFound, &cli.CLIError{
		Binary: "docker", Args: []string{"rm", "--force", "target"}, ExitCode: 1,
		Stderr: "Error response from daemon: No such container: target",
	})
	if !isNotFoundForOperation(dockerEngine{}, matchingAbsence, "rm", "target") {
		t.Fatal("matching rm absence evidence was not honored with sentinel")
	}
}

func TestProbeStdoutAttachesToMatchingBranch(t *testing.T) {
	wrongBranch := &cli.CLIError{
		Binary: "container", Args: []string{"system", "status"}, ExitCode: 1,
		Stderr: "container backend is not running",
	}
	matchingBranch := &cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}, ExitCode: 1,
		Stderr: "docker version failed",
	}
	original := &cli.CLIError{
		Binary: "docker", Args: []string{"run", "--name", "myctr"}, ExitCode: 1,
		Stderr: "run failed",
	}
	probe := dockerEngine{}.probe()
	sawMatchingStdout := false
	probe.IsUnavailable = func(probeClassification error) bool {
		for _, branch := range backendCLIErrorBranches(probeClassification, "docker") {
			if branch.ctx.operation == "version" && strings.Contains(branch.stdout, "Cannot connect") {
				sawMatchingStdout = true
			}
		}
		return false
	}
	runner := &issue104FollowupProbeRunner{
		probeErr:    errors.Join(wrongBranch, matchingBranch),
		probeStdout: "Cannot connect to the Docker daemon\n",
	}
	got := cli.Classify(context.Background(), runner, original, probe)
	if got == nil {
		t.Fatal("Classify returned nil for a failed probe")
	}
	if !sawMatchingStdout {
		t.Fatalf("probe stdout was not attached to the matching Docker version branch: %v", got)
	}
}

func TestAppleParseInspectRejectsNullAndInvalidContainerEntries(t *testing.T) {
	cases := [][]byte{
		[]byte("null"),
		[]byte("[null]"),
		[]byte(`[{"configuration":{"id":"myctr"},"status":{"state":"running"}}]`),
		[]byte(`[{"id":"myctr","configuration":{"id":"myctr"}}]`),
	}
	for _, data := range cases {
		_, err := (appleEngine{}).parseInspect(data, "myctr")
		if err == nil || errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("parseInspect(%s) error = %v, want schema error", data, err)
		}
	}
}

func TestAppleEmptyArrayStillMeansTargetAbsence(t *testing.T) {
	_, err := (appleEngine{}).parseInspect([]byte("[]"), "myctr")
	if err == nil || !errors.Is(err, errInspectTargetNotFound) {
		t.Fatalf("parseInspect([]) error = %v, want target absence", err)
	}
}

func TestDockerParseInspectRejectsNonCanonicalID(t *testing.T) {
	for _, id := range []string{"myctr", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		data := []byte(fmt.Sprintf(`[{"Id":%q,"Name":"/myctr","State":{"Status":"running"},"Config":{},"NetworkSettings":{}}]`, id))
		_, err := (dockerEngine{}).parseInspect(data, "myctr")
		if err == nil || errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("parseInspect(%q) error = %v, want schema error", id, err)
		}
	}
}
