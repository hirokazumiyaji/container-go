package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type issue104FreshDockerRunner struct {
	calls       [][]string
	inspectJSON []byte
	execErr     error
	execStdout  string
	execStderr  string
}

func (r *issue104FreshDockerRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "inspect":
		return append([]byte(nil), r.inspectJSON...), nil, nil
	case "exec":
		return []byte(r.execStdout), []byte(r.execStderr), r.execErr
	default:
		return nil, nil, nil
	}
}

func dockerInspectJSON(id, name, state string) []byte {
	return []byte(fmt.Sprintf(`[{"Id":%q,"Name":%q,"State":{"Status":%q},"Config":{},"NetworkSettings":{}}]`, id, name, state))
}

func TestNotFoundForOperationMatchesExecVerificationBranch(t *testing.T) {
	err := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr"}, ExitCode: 1,
		Stderr: "Error: get failed: container myctr not found",
	}
	if !isNotFoundForOperation(appleEngine{}, err, "exec", "myctr") {
		t.Fatalf("isNotFoundForOperation = false for %v", err)
	}
}

func TestDockerOperationsUseImmutableOperationTarget(t *testing.T) {
	uid := strings.Repeat("a", 64)
	r := &issue104FreshDockerRunner{inspectJSON: dockerInspectJSON(uid, "/myctr", "running")}
	ctr := &Container{id: "myctr", uid: uid, runner: r, eng: dockerEngine{}}

	if _, err := ctr.State(context.Background()); err != nil {
		t.Fatalf("State: %v", err)
	}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	logs, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_ = logs.Close()
	_ = ctr.logTail(context.Background())
	hostPath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(hostPath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctr.CopyToContainer(context.Background(), hostPath, "/file"); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if verification := ctr.verifyExecContainer(context.Background()); verification.err != nil {
		t.Fatalf("verifyExecContainer: %v", verification.err)
	}

	for _, call := range r.calls {
		if len(call) == 0 || !strings.Contains(strings.Join(call, " "), uid) {
			t.Errorf("operation %v did not use immutable UID %s", call, uid)
		}
	}
	for _, call := range r.calls {
		if call[0] == "inspect" && !slices.Contains(call, "--type=container") {
			t.Errorf("docker inspect args = %v, want --type=container", call)
		}
	}
}

func TestDockerRejectsSameNameReplacementByUID(t *testing.T) {
	uid := strings.Repeat("a", 64)
	replacementID := strings.Repeat("b", 64)
	r := &issue104FreshDockerRunner{inspectJSON: dockerInspectJSON(replacementID, "/myctr", "running")}
	ctr := &Container{id: "myctr", uid: uid, runner: r, eng: dockerEngine{}}

	if _, err := ctr.State(context.Background()); err == nil {
		t.Fatal("State accepted a same-name replacement for an immutable Docker handle")
	}
	if verification := ctr.verifyExecContainer(context.Background()); verification.err == nil {
		t.Fatal("Exec verification accepted a same-name replacement for an immutable Docker handle")
	}
}

func TestDockerParseInspectRejectsNullAndNonContainerState(t *testing.T) {
	uid := strings.Repeat("a", 64)
	cases := []struct {
		name      string
		data      []byte
		wantFound bool
		wantShape bool
	}{
		{name: "empty array", data: []byte("[]"), wantShape: false},
		{name: "top-level null", data: []byte("null"), wantShape: true},
		{name: "null entry", data: []byte("[null]"), wantShape: true},
		{name: "missing state", data: []byte(`[{"Id":"` + uid + `","Name":"/myctr"}]`), wantShape: true},
		{name: "null state", data: []byte(`[{"Id":"` + uid + `","Name":"/myctr","State":null}]`), wantShape: true},
		{name: "empty state", data: []byte(`[{"Id":"` + uid + `","Name":"/myctr","State":{}}]`), wantShape: true},
		{name: "valid", data: dockerInspectJSON(uid, "/myctr", "running"), wantFound: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := (dockerEngine{}).parseInspect(tc.data, uid)
			if tc.wantShape {
				if err == nil {
					t.Fatal("parseInspect accepted malformed container output")
				}
				if errors.Is(err, errInspectTargetNotFound) {
					t.Fatalf("shape error was classified as target absence: %v", err)
				}
				return
			}
			if tc.wantFound {
				if err != nil || info == nil {
					t.Fatalf("parseInspect valid output = %v/%v", info, err)
				}
				return
			}
			if err == nil || !errors.Is(err, errInspectTargetNotFound) {
				t.Fatalf("parseInspect([]) error = %v, want target absence", err)
			}
		})
	}
}

type issue104JoinedExecRunner struct {
	*fakeRunner
	err    error
	stdout string
	stderr string
}

func (r *issue104JoinedExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		return []byte(r.stdout), []byte(r.stderr), r.err
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecJoinedErrorSelectsCurrentBranch(t *testing.T) {
	wrong := &cli.CLIError{
		Binary: "docker", Args: []string{"version"}, ExitCode: -1,
		Stderr: "unrelated signal branch",
	}
	selected := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "app"}, ExitCode: 23,
		Stderr: "permission denied: workload path",
	}
	f := &issue104JoinedExecRunner{
		fakeRunner: newTestRunner(),
		err:        errors.Join(wrong, selected),
		stdout:     "workload output\n",
		stderr:     selected.Stderr,
	}
	ctr := runTestContainer(t, f)
	ctr.runner = f

	code, out, err := ctr.Exec(context.Background(), []string{"app"})
	if err != nil {
		t.Fatalf("Exec: %v, want selected ordinary workload result", err)
	}
	if code != 23 {
		t.Errorf("exit code = %d, want selected branch code 23", code)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "workload output") || !strings.Contains(string(data), "permission denied") {
		t.Errorf("output = %q, want selected branch output", data)
	}
}

func TestExecIgnoresUnrelatedJoinedAbsenceBranch(t *testing.T) {
	unrelated := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Error: no such object: myctr",
	}
	selected := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "app"}, ExitCode: 23,
		Stderr: "permission denied: workload path",
	}
	f := &issue104JoinedExecRunner{
		fakeRunner: newTestRunner(),
		err:        errors.Join(unrelated, selected),
		stderr:     selected.Stderr,
	}
	ctr := runTestContainer(t, f)
	ctr.runner = f
	before := 0
	for _, call := range f.calls {
		if len(call) > 0 && call[0] == "inspect" {
			before++
		}
	}
	code, _, err := ctr.Exec(context.Background(), []string{"app"})
	if err != nil || code != 23 {
		t.Fatalf("Exec = %d/%v, want ordinary selected result 23", code, err)
	}
	after := 0
	for _, call := range f.calls {
		if len(call) > 0 && call[0] == "inspect" {
			after++
		}
	}
	if after != before {
		t.Fatalf("unrelated absence branch triggered %d inspect probes", after-before)
	}
}

func TestNotFoundSelectsMatchingJoinedBranch(t *testing.T) {
	absence := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Error: no such object: myctr",
	}
	unrelated := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "other", "app"}, ExitCode: 1,
		Stderr: "permission denied: unrelated workload",
	}
	for _, err := range []error{errors.Join(unrelated, absence), errors.Join(absence, unrelated)} {
		if !isNotFoundFor(dockerEngine{}, err) {
			t.Fatalf("matching Docker inspect branch was not selected from %v", err)
		}
	}
}

func TestNotFoundDefinitiveBranchMustMatchOperationAndTarget(t *testing.T) {
	absence := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Error: no such object: myctr",
	}
	permission := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	if isNotFoundFor(dockerEngine{}, errors.Join(absence, permission)) {
		t.Fatal("same-operation permission branch did not veto absence")
	}
}

func TestNotFoundIgnoresAmbiguousBranchForAnotherTarget(t *testing.T) {
	absence := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Error: no such object: myctr",
	}
	unrelated := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "other"}, ExitCode: 1,
		Stderr: "Error: application: no such object: other",
	}
	if !isNotFoundFor(dockerEngine{}, errors.Join(unrelated, absence)) {
		t.Fatal("ambiguous diagnostic for another target suppressed the matching absence")
	}
}

func TestProbeClassificationSelectsMatchingJoinedBranch(t *testing.T) {
	liveness := &cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}, ExitCode: 1,
		Stderr: "Cannot connect to the Docker daemon",
	}
	unrelated := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "app"}, ExitCode: 1,
		Stderr: "invalid configuration",
	}
	for _, err := range []error{errors.Join(unrelated, liveness), errors.Join(liveness, unrelated)} {
		if !dockerProbeUnavailable(err) {
			t.Fatalf("matching Docker probe branch was not selected from %v", err)
		}
	}
}

type issue104ClassifyProbeRunner struct {
	err error
}

func (r *issue104ClassifyProbeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "system" {
		return nil, nil, r.err
	}
	return nil, nil, nil
}

func TestClassifyIgnoresUnrelatedJoinedProbeConfigBranch(t *testing.T) {
	original := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"}, ExitCode: 1,
		Stderr: "command failed",
	}
	liveness := &cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}, ExitCode: 1,
		Stderr: "Cannot connect to the Docker daemon",
	}
	unrelated := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "app"}, ExitCode: 1,
		Stderr: "invalid configuration",
	}
	for _, probeErr := range []error{errors.Join(unrelated, liveness), errors.Join(liveness, unrelated)} {
		got := cli.Classify(context.Background(), &latestProbeRunner{probeErr: probeErr}, original, dockerEngine{}.probe())
		if !errors.Is(got, cli.ErrSystemNotRunning) {
			t.Fatalf("unrelated probe configuration suppressed liveness classification: %v", got)
		}
		if !errors.Is(got, original) || !errors.Is(got, probeErr) {
			t.Fatalf("classified error = %v, want original chains", got)
		}
	}
}

func TestClassifySameOperationDefinitiveBranchVetoesLiveness(t *testing.T) {
	absence := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Error: no such object: myctr",
	}
	permission := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "permission denied",
	}
	probeErr := &cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}, ExitCode: 1,
		Stderr: "Cannot connect to the Docker daemon",
	}
	original := errors.Join(absence, permission)
	got := classifyErrorFor(context.Background(), &latestProbeRunner{probeErr: probeErr}, original, dockerEngine{}, "inspect", "myctr")
	if errors.Is(got, cli.ErrSystemNotRunning) {
		t.Fatalf("same-operation permission branch was classified as liveness: %v", got)
	}
	if !errors.Is(got, original) {
		t.Fatalf("classified error = %v, want original chain", got)
	}
}

func TestClassifyErrorSelectsCurrentOperationBranch(t *testing.T) {
	wrong := &cli.CLIError{
		Binary: "docker", Args: []string{"version"}, ExitCode: -1,
		Stderr: "unrelated signal branch",
	}
	wrongTarget := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "other"}, ExitCode: -1,
		Stderr: "unrelated target branch",
	}
	selected := &cli.CLIError{
		Binary: "container", Args: []string{"run", "--name", "myctr"}, ExitCode: 1,
		Stderr: "XPC connection error",
	}
	original := errors.Join(wrong, wrongTarget, selected)
	probeErr := &cli.CLIError{
		Binary: "container", Args: []string{"system", "status"}, ExitCode: 1,
		Stderr: "XPC connection error",
	}
	got := classifyErrorFor(context.Background(), &issue104ClassifyProbeRunner{err: probeErr}, original, appleEngine{}, "run", "myctr")
	if !errors.Is(got, cli.ErrSystemNotRunning) {
		t.Fatalf("Classify error = %v, want current run branch liveness classification", got)
	}
	for _, want := range []error{wrong, wrongTarget, selected} {
		if !errors.Is(got, want) {
			t.Fatalf("Classify error = %v, want original branch %v", got, want)
		}
	}
}

func TestProbeStdoutRemainsBranchLocalRegardlessOfJoinOrder(t *testing.T) {
	otherOperation := cli.WithStdout(&cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
	}, "Cannot connect to the Docker daemon\n")
	ordinaryVersion := &cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}, ExitCode: 1,
		Stderr: "version request failed",
	}
	for _, err := range []error{
		errors.Join(otherOperation, ordinaryVersion),
		errors.Join(ordinaryVersion, otherOperation),
		fmt.Errorf("probe: %w", errors.Join(ordinaryVersion, otherOperation)),
	} {
		if dockerProbeUnavailable(err) {
			t.Fatalf("probe stdout from another operation leaked into version branch: %v", err)
		}
	}
}

func TestDockerProbeStdoutPreservesDecisiveTail(t *testing.T) {
	tail := "Cannot connect to the Docker daemon"
	long := strings.Repeat("diagnostic prefix\n", 7000) + tail
	err := cli.WithStdout(&cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}, ExitCode: 1,
	}, long)
	if !dockerProbeUnavailable(err) {
		t.Fatal("truncated probe stdout lost its decisive tail")
	}
}
