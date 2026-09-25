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

func (r *issue104FollowupPruneRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && (args[0] == "ps" || args[0] == "ls") {
		return nil, nil, nil
	}
	if len(args) > 0 && (args[0] == "rm" || args[0] == "delete") {
		return nil, nil, r.deleteErr
	}
	return nil, nil, nil
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
