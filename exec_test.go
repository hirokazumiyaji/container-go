package container

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// execRunner extends fakeRunner with canned exec results.
type execRunner struct {
	*fakeRunner
	execStdout string
	execErr    error
}

func (e *execRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		e.calls = append(e.calls, args)
		for i, a := range args {
			if a == "--env-file" && i+1 < len(args) {
				data, _ := os.ReadFile(args[i+1])
				e.envFiles = append(e.envFiles, string(data))
			}
		}
		stderr := []byte("stderr-part")
		var cliErr *cli.CLIError
		if errors.As(e.execErr, &cliErr) {
			stderr = []byte(cliErr.Stderr)
		}
		return []byte(e.execStdout), stderr, e.execErr
	}
	return e.fakeRunner.Run(ctx, args...)
}

func TestExecReturnsZeroExitAndOutput(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "hello\n"}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "hello\n") {
		t.Errorf("output = %q", data)
	}

	execCall := f.callWith("exec")
	i := slices.Index(execCall, "myctr")
	if i < 0 || !slices.Equal(execCall[i+1:], []string{"echo", "hello"}) {
		t.Errorf("exec args = %v", execCall)
	}
}

func TestExecReturnsCommandExitCodeWithoutError(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "process failed"},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"false"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	data, _ := io.ReadAll(out)
	if string(data) != "process failed" {
		t.Errorf("output = %q, want %q", string(data), "process failed")
	}
}

func TestExecReportsMissingContainerAsError(t *testing.T) {
	f := &execMissingRunner{
		execRunner: &execRunner{
			fakeRunner: newTestRunner(),
			execErr:    &cli.CLIError{Args: []string{"exec", "myctr", "true"}, ExitCode: 1, Stderr: `Error: get failed: container myctr not found`},
		},
	}
	ctr := runTestContainer(t, f)

	_, _, err := ctr.Exec(context.Background(), []string{"true"})
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, want ErrContainerNotFound", err)
	}
}

type execMissingRunner struct {
	*execRunner
}

func (m *execMissingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `Error: container not found: myctr`}
	}
	return m.execRunner.Run(ctx, args...)
}

func TestExecAppNotFoundStderrIsResult(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "container not found: myctr"},
	}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec: %v, want app result", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
}

type operationSpecificExecStderrRunner struct {
	inspectCalls int
}

func (r *operationSpecificExecStderrRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "exec":
		return nil, nil, &cli.CLIError{
			Binary: "docker", Args: args, ExitCode: 7,
			Stderr: "Error: no such container: myctr",
		}
	case "inspect":
		r.inspectCalls++
		return nil, nil, errors.New("inspect endpoint is temporarily unavailable")
	case "version":
		return []byte("29.7"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestExecGenericErrorStderrIsNotDockerBackendEvidence(t *testing.T) {
	runner := &operationSpecificExecStderrRunner{}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}

	code, _, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec: %v, want application result", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
	if runner.inspectCalls != 0 {
		t.Errorf("inspect calls = %d, want no classification probe for application stderr", runner.inspectCalls)
	}
}

type execVerificationRunner struct {
	execErr       error
	inspectErr    error
	inspectStdout string
	probeErr      error
	inspectCalls  int
	probeCalls    int
}

func (r *execVerificationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "exec":
		var stderr []byte
		var cliErr *cli.CLIError
		if errors.As(r.execErr, &cliErr) {
			stderr = []byte(cliErr.Stderr)
		}
		return nil, stderr, r.execErr
	case "inspect":
		r.inspectCalls++
		if r.inspectErr != nil {
			return nil, nil, r.inspectErr
		}
		return []byte(r.inspectStdout), nil, nil
	case "system", "version":
		r.probeCalls++
		if r.probeErr != nil {
			return nil, nil, r.probeErr
		}
		return []byte("running"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestExecInspectFailureIsNotContainerAbsence(t *testing.T) {
	cases := []struct {
		name       string
		eng        engine
		execErr    *cli.CLIError
		inspectErr error
	}{
		{
			name: "docker TLS failure",
			eng:  dockerEngine{},
			execErr: &cli.CLIError{
				Binary: "docker", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
				Stderr: "Error response from daemon: No such container: myctr",
			},
			inspectErr: &cli.CLIError{
				Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
				Stderr: "error during connect: x509: certificate signed by unknown authority",
			},
		},
		{
			name: "Apple application inspect failure",
			eng:  appleEngine{},
			execErr: &cli.CLIError{
				Binary: "container", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
				Stderr: "Error: get failed: container myctr not found",
			},
			inspectErr: errors.New("inspect returned application-specific output"),
		},
		{
			name: "wrong inspect target",
			eng:  dockerEngine{},
			execErr: &cli.CLIError{
				Binary: "docker", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
				Stderr: "Error response from daemon: No such container: myctr",
			},
			inspectErr: &cli.CLIError{
				Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
				Stderr: "Error response from daemon: No such object: other",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &execVerificationRunner{execErr: tc.execErr, inspectErr: tc.inspectErr}
			ctr := &Container{id: "myctr", runner: runner, eng: tc.eng}

			_, _, err := ctr.Exec(context.Background(), []string{"query"})
			if err == nil {
				t.Fatal("Exec returned nil, want preserved verification failure")
			}
			if errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("Exec error = %v, inspect failure became ErrContainerNotFound", err)
			}
			if errors.Is(err, ErrSystemNotRunning) {
				t.Fatalf("Exec error = %v, inspect failure became daemon down", err)
			}
			if !errors.Is(err, tc.execErr) || !errors.Is(err, tc.inspectErr) {
				t.Fatalf("Exec error = %v, want exec and inspect errors", err)
			}
			if runner.inspectCalls != 1 || runner.probeCalls != 0 {
				t.Fatalf("inspect calls = %d, probe calls = %d; want one inspect and no liveness probe", runner.inspectCalls, runner.probeCalls)
			}
		})
	}
}

func TestExecInspectLivenessFailureStillClassifiesSystemDown(t *testing.T) {
	execErr := &cli.CLIError{
		Binary: "docker", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
		Stderr: "Cannot connect to the Docker daemon",
	}
	inspectErr := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Cannot connect to the Docker daemon",
	}
	probeErr := &cli.CLIError{
		Binary: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"},
		ExitCode: 1, Stderr: "Cannot connect to the Docker daemon",
	}
	runner := &execVerificationRunner{execErr: execErr, inspectErr: inspectErr, probeErr: probeErr}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}

	_, _, err := ctr.Exec(context.Background(), []string{"query"})
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("Exec error = %v, want ErrSystemNotRunning", err)
	}
	if !errors.Is(err, execErr) || !errors.Is(err, inspectErr) || !errors.Is(err, probeErr) {
		t.Fatalf("Exec error = %v, want exec, inspect, and probe chains", err)
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, liveness failure became ErrContainerNotFound", err)
	}
}

func TestExecMissingInspectOutputIsNotFound(t *testing.T) {
	execErr := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
		Stderr: "Error: get failed: container myctr not found",
	}
	runner := &execVerificationRunner{execErr: execErr, inspectStdout: "[]"}
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}

	_, _, err := ctr.Exec(context.Background(), []string{"query"})
	if !errors.Is(err, ErrContainerNotFound) || !errors.Is(err, execErr) {
		t.Fatalf("Exec error = %v, want ErrContainerNotFound with original chain", err)
	}
}

func TestExecStoppedTargetDoesNotBecomeNotFound(t *testing.T) {
	execErr := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
		Stderr: "Error: get failed: container myctr not found",
	}
	inspectStdout := strings.ReplaceAll(ownedInspectJSON("myctr"), `"state": "created"`, `"state": "stopped"`)
	runner := &execVerificationRunner{execErr: execErr, inspectStdout: inspectStdout}
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}

	_, _, err := ctr.Exec(context.Background(), []string{"query"})
	if !errors.Is(err, execErr) {
		t.Fatalf("Exec error = %v, want original command error", err)
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, stopped target became ErrContainerNotFound", err)
	}
	if runner.probeCalls != 0 {
		t.Fatalf("probe calls = %d, want none after successful inspect", runner.probeCalls)
	}
}

type joinedContextExecRunner struct {
	execErr      *cli.CLIError
	inspectCalls int
}

func (r *joinedContextExecRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "exec":
		return []byte("application output"), []byte(r.execErr.Stderr), errors.Join(r.execErr, context.Canceled)
	case "inspect":
		r.inspectCalls++
		return nil, nil, errors.New("inspect should not run after caller cancellation")
	case "version":
		return []byte("29.7"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestExecPrioritizesCallerContextOverCommandExit(t *testing.T) {
	execErr := &cli.CLIError{
		Binary: "docker", Args: []string{"exec", "myctr", "query"}, ExitCode: 7,
		Stderr: "Error response from daemon: No such container: myctr",
	}
	runner := &joinedContextExecRunner{execErr: execErr}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, _, err := ctr.Exec(ctx, []string{"query"})
	if code != 0 {
		t.Errorf("code = %d, want error result", code)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, execErr) {
		t.Fatalf("Exec error = %v, want caller context and command exit", err)
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, canceled command was classified as missing", err)
	}
	if runner.inspectCalls != 0 {
		t.Errorf("inspect calls = %d, want no verification after cancellation", runner.inspectCalls)
	}
}

func TestExecReturnedContextCausePrioritizesCommandExit(t *testing.T) {
	execErr := &cli.CLIError{
		Binary: "docker", Args: []string{"exec", "myctr", "query"}, ExitCode: 7,
		Stderr: "Error response from daemon: No such container: myctr",
	}
	runner := &joinedContextExecRunner{execErr: execErr}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}

	code, _, err := ctr.Exec(context.Background(), []string{"query"})
	if code != 0 {
		t.Errorf("code = %d, want error result", code)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, execErr) {
		t.Fatalf("Exec error = %v, want returned context and command exit", err)
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, context was classified as missing", err)
	}
	if runner.inspectCalls != 0 {
		t.Errorf("inspect calls = %d, want no verification after context cause", runner.inspectCalls)
	}
}

type cancelDuringInspectRunner struct {
	execErr    *cli.CLIError
	inspectErr *cli.CLIError
	cancel     context.CancelFunc
}

func (r *cancelDuringInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "exec":
		return nil, []byte(r.execErr.Stderr), r.execErr
	case "inspect":
		r.cancel()
		return nil, nil, r.inspectErr
	default:
		return nil, nil, nil
	}
}

func TestExecCancellationDuringInspectOverridesConfirmedNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execErr := &cli.CLIError{
		Binary: "docker", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
		Stderr: "Error response from daemon: No such container: myctr",
	}
	inspectErr := &cli.CLIError{
		Binary: "docker", Args: []string{"inspect", "myctr"}, ExitCode: 1,
		Stderr: "Error response from daemon: No such object: myctr",
	}
	runner := &cancelDuringInspectRunner{execErr: execErr, inspectErr: inspectErr, cancel: cancel}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}

	_, _, err := ctr.Exec(ctx, []string{"query"})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, execErr) || !errors.Is(err, inspectErr) {
		t.Fatalf("Exec error = %v, want cancellation and command/inspect chains", err)
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Exec error = %v, cancellation lost to inspect result", err)
	}
}

func TestExecSuccessAddsNoProbe(t *testing.T) {
	inner := &execRunner{fakeRunner: newTestRunner(), execStdout: "ok\n"}
	r := newCountingRunner(inner)
	ctr := runTestContainer(t, r)
	before := r.count()
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := r.count() - before; got != 1 {
		t.Fatalf("exec success calls = %d, want 1", got)
	}
}

func TestExecPassesOptionsAndEnvFile(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	_, _, err := ctr.Exec(context.Background(), []string{"id"},
		WithExecUser("nobody"), WithExecWorkDir("/tmp"),
		WithExecEnv(map[string]string{"TOKEN": "xyz"}))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	joined := strings.Join(f.callWith("exec"), " ")
	if !strings.Contains(joined, "--user nobody") || !strings.Contains(joined, "--workdir /tmp") {
		t.Errorf("exec args = %s", joined)
	}
	if strings.Contains(joined, "--env ") {
		t.Errorf("exec env passed via argv: %s", joined)
	}
	if len(f.envFiles) != 1 || !strings.Contains(f.envFiles[0], "TOKEN=xyz\n") {
		t.Errorf("env-file captures = %q", f.envFiles)
	}
}

func TestExecRejectsEmptyCommand(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), nil); err == nil {
		t.Fatal("want error for empty command")
	}
}
