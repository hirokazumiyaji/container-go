package container

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
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

func TestExecKeepsPositiveWorkloadPermissionTextAsResult(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "workload output\n",
		execErr: &cli.CLIError{
			Binary:   "container",
			Args:     []string{"exec", "myctr", "app"},
			ExitCode: 23,
			Stderr:   "permission denied: /var/lib/app/data\n",
		},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"app"})
	if err != nil {
		t.Fatalf("Exec: %v, want ordinary positive workload result", err)
	}
	if code != 23 {
		t.Errorf("exit code = %d, want 23", code)
	}
	data, _ := io.ReadAll(out)
	if got, want := string(data), "workload output\npermission denied: /var/lib/app/data\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestExecReturnsStructuredPermissionAndConfigFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{
			name: "permission sentinel",
			err: errors.Join(
				&cli.CLIError{
					Binary: "container", Args: []string{"exec", "myctr", "app"},
					ExitCode: 17, Stderr: "permission denied",
				},
				os.ErrPermission,
			),
			code: 17,
		},
		{
			name: "configuration sentinel",
			err: errors.Join(
				&cli.CLIError{
					Binary: "container", Args: []string{"exec", "myctr", "app"},
					ExitCode: 18, Stderr: "invalid configuration",
				},
				os.ErrInvalid,
			),
			code: 18,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{
				fakeRunner: newTestRunner(),
				execStdout: "diagnostic output\n",
				execErr:    tc.err,
			}
			ctr := runTestContainer(t, f)

			code, out, err := ctr.Exec(context.Background(), []string{"app"})
			if err == nil {
				t.Fatal("Exec returned nil for a structured client-side failure")
			}
			if code != tc.code {
				t.Errorf("exit code = %d, want %d", code, tc.code)
			}
			if out == nil {
				t.Fatal("Exec returned nil output for a structured client-side failure")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("error = %v, want original structured failure", err)
			}
		})
	}
}

type issue104CanceledExecRunner struct {
	*execRunner
}

func (r *issue104CanceledExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		return []byte("partial output"), []byte("process killed"), errors.Join(
			&cli.CLIError{Args: args, ExitCode: -1, Stderr: "process killed"},
			context.Canceled,
		)
	}
	return r.execRunner.Run(ctx, args...)
}

func TestExecReturnsCancellationWhenCLIExitIsSignal(t *testing.T) {
	f := &issue104CanceledExecRunner{execRunner: &execRunner{fakeRunner: newTestRunner()}}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"sleep"})
	if err == nil {
		t.Fatal("Exec returned nil error for a canceled CLI process")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != -1 {
		t.Fatalf("error = %v, want original signal CLIError in the chain", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 for an unobservable signal exit", code)
	}
	if out == nil {
		t.Fatal("Exec returned nil output for a canceled CLI process")
	}
}

func TestExecReportsMissingContainerAsError(t *testing.T) {
	f := &execMissingRunner{
		execRunner: &execRunner{
			fakeRunner: newTestRunner(),
			execErr:    &cli.CLIError{Args: []string{"exec", "myctr"}, ExitCode: 1, Stderr: `Error: get failed: container myctr not found`},
		},
	}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err == nil {
		t.Fatal("want error for missing container")
	}
}

type execMissingRunner struct {
	*execRunner
	inspectCalls int
}

func (m *execMissingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		m.inspectCalls++
		if m.inspectCalls > 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `Error: container not found: "myctr"`}
		}
	}
	return m.execRunner.Run(ctx, args...)
}

type execInspectFailureRunner struct {
	*execRunner
	inspectCalls  int
	inspectStdout []byte
	inspectErr    error
}

func (r *execInspectFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.inspectCalls++
		if r.inspectCalls > 1 {
			return r.inspectStdout, nil, r.inspectErr
		}
	}
	return r.execRunner.Run(ctx, args...)
}

func TestExecPropagatesInspectVerificationErrors(t *testing.T) {
	cases := []struct {
		name            string
		inspectStdout   []byte
		inspectErr      error
		wantNotFound    bool
		wantSyntaxError bool
		wantSchema      string
	}{
		{
			name: "target missing",
			inspectErr: &cli.CLIError{
				Args:   []string{"inspect", "myctr"},
				Stderr: `Error: container not found: "myctr"`,
			},
			wantNotFound: true,
		},
		{
			name:            "malformed inspect",
			inspectStdout:   []byte(`{not json`),
			wantSyntaxError: true,
		},
		{
			name:          "null inspect",
			inspectStdout: []byte(`null`),
			wantSchema:    "expected a JSON array",
		},
		{
			// A null entry is unreadable output, not a missing
			// container: reporting it as not found would send a running
			// container's exec failure down the wrong path.
			name:          "null inspect entry",
			inspectStdout: []byte(`[null]`),
			wantSchema:    "got null",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execInspectFailureRunner{
				execRunner: &execRunner{
					fakeRunner: newTestRunner(),
					execErr: &cli.CLIError{
						Args:     []string{"exec"},
						ExitCode: 1,
						Stderr:   "daemon unavailable",
					},
				},
				inspectStdout: tc.inspectStdout,
				inspectErr:    tc.inspectErr,
			}
			ctr := runTestContainer(t, f)
			_, _, err := ctr.Exec(context.Background(), []string{"true"})
			if err == nil {
				t.Fatal("Exec returned nil error for failed inspect verification")
			}
			if got := errors.Is(err, ErrContainerNotFound); got != tc.wantNotFound {
				t.Errorf("errors.Is(ErrContainerNotFound) = %t, want %t: %v", got, tc.wantNotFound, err)
			}
			var syntaxErr *json.SyntaxError
			if got := errors.As(err, &syntaxErr); got != tc.wantSyntaxError {
				t.Errorf("errors.As(*json.SyntaxError) = %t, want %t: %v", got, tc.wantSyntaxError, err)
			}
			if tc.wantSchema != "" && (!strings.Contains(err.Error(), tc.wantSchema) || errors.Is(err, ErrContainerNotFound)) {
				t.Errorf("error = %v, want a schema error containing %q and distinct from ErrContainerNotFound", err, tc.wantSchema)
			}
		})
	}
}

func TestExecAppNotFoundStderrIsResult(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "record not found"},
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

func TestExecReturnsStructuredOperationTimeoutAsInfrastructureError(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "partial stdout",
		execErr: errors.Join(
			&cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "command failed"},
			context.DeadlineExceeded,
		),
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"query"})
	if err == nil {
		t.Fatal("Exec returned nil error for a structured operation timeout")
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if out == nil {
		t.Fatal("Exec returned nil output for a structured operation timeout")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want the original timeout CLIError", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if f.callWith("version") != nil || f.callWith("system") != nil {
		t.Errorf("timeout triggered an infrastructure probe: %v", f.calls)
	}
}

func TestExecKeepsTimeoutTextAsApplicationResult(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		stderr string
	}{
		{name: "application stderr", args: []string{"exec", "myctr", "query"}, stderr: "i/o timeout"},
		{name: "argv", args: []string{"exec", "myctr", "command timed out"}, stderr: "application failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{
				fakeRunner: newTestRunner(),
				execStdout: "application output",
				execErr: &cli.CLIError{
					Args:     tc.args,
					ExitCode: 7,
					Stderr:   tc.stderr,
				},
			}
			ctr := runTestContainer(t, f)

			code, out, err := ctr.Exec(context.Background(), []string{"query"})
			if err != nil {
				t.Fatalf("Exec: %v, want normal non-zero application result", err)
			}
			if code != 7 || out == nil {
				t.Fatalf("code/output = %d/%v, want code 7 and output", code, out)
			}
		})
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
	ctr := &Container{id: "myctr", sync: &lockPack{uid: dockerFixtureID}, runner: runner, eng: dockerEngine{}}

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
			ctr := &Container{id: "myctr", creation: "bbbbbbbbbbbbbbbb", runner: runner, eng: tc.eng}
			if _, ok := tc.eng.(dockerEngine); ok {
				ctr.sync = &lockPack{uid: dockerFixtureID}
			}

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
	ctr := &Container{id: "myctr", sync: &lockPack{uid: dockerFixtureID}, runner: runner, eng: dockerEngine{}}

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
	ctr := &Container{id: "myctr", creation: "bbbbbbbbbbbbbbbb", runner: runner, eng: appleEngine{}}

	_, _, err := ctr.Exec(context.Background(), []string{"query"})
	if !errors.Is(err, ErrContainerNotFound) || !errors.Is(err, execErr) {
		t.Fatalf("Exec error = %v, want ErrContainerNotFound with original chain", err)
	}
}

func TestExecStoppedTargetDoesNotBecomeNotFound(t *testing.T) {
	const creation = "bbbbbbbbbbbbbbbb"
	execErr := &cli.CLIError{
		Binary: "container", Args: []string{"exec", "myctr", "query"}, ExitCode: 1,
		Stderr: "Error: get failed: container myctr not found",
	}
	inspectStdout := strings.ReplaceAll(ownedInspectJSON("myctr"), `"state": "created"`, `"state": "stopped"`)
	inspectStdout = strings.ReplaceAll(inspectStdout, "__CONTAINER_CREATION__", creation)
	runner := &execVerificationRunner{execErr: execErr, inspectStdout: inspectStdout}
	ctr := &Container{id: "myctr", creation: creation, runner: runner, eng: appleEngine{}}

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
	ctr := &Container{id: "myctr", sync: &lockPack{uid: dockerFixtureID}, runner: runner, eng: dockerEngine{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, _, err := ctr.Exec(ctx, []string{"query"})
	if code != 0 {
		t.Errorf("code = %d, want error result", code)
	}
	// An already-finished caller context must not launch. Keep the
	// cancellation, and do not invent a command exit that never ran.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec error = %v, want caller context", err)
	}
	if errors.Is(err, execErr) {
		t.Fatalf("Exec error = %v, pre-canceled Exec must not launch a command", err)
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
	ctr := &Container{id: "myctr", sync: &lockPack{uid: dockerFixtureID}, runner: runner, eng: dockerEngine{}}

	code, _, err := ctr.Exec(context.Background(), []string{"query"})
	if code != 7 {
		t.Errorf("code = %d, want preserved command exit 7", code)
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
	ctr := &Container{id: "myctr", sync: &lockPack{uid: dockerFixtureID}, runner: runner, eng: dockerEngine{}}

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
	isolateEnvFileRoot(t)
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

func TestExecRejectsEnvValueBeyondStreamSafetyLimit(t *testing.T) {
	secret := strings.Repeat("s", diagnostic.MaxStreamOverlap+1)
	ctr := runTestContainer(t, &execRunner{fakeRunner: newTestRunner()})
	_, _, err := ctr.Exec(context.Background(), []string{"true"}, WithExecEnv(map[string]string{"TOKEN": secret}))
	if err == nil || !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("validation error echoes oversized value")
	}
}

func TestExecRejectsEmptyCommand(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), nil); err == nil {
		t.Fatal("want error for empty command")
	}
}

type issue116BlockingExecRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
}

func (r *issue116BlockingExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "exec" {
		return r.fakeRunner.Run(ctx, args...)
	}
	close(r.started)
	select {
	case <-ctx.Done():
		return []byte("partial stdout"), []byte("partial stderr"), ctx.Err()
	case <-r.release:
		return []byte("partial stdout"), []byte("partial stderr"), errors.New("backend released")
	}
}

type issue116DeadlineExecRunner struct {
	*fakeRunner
	deadline time.Time
	has      bool
}

func (r *issue116DeadlineExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.deadline, r.has = ctx.Deadline()
		return []byte("ok"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type issue116ExecResult struct {
	code int
	out  io.Reader
	err  error
}

func waitForIssue116ExecStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("exec stub was not started")
	}
}

func waitForIssue116ExecResult(t *testing.T, result <-chan issue116ExecResult) issue116ExecResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(time.Second):
		t.Fatal("Exec did not return")
		return issue116ExecResult{}
	}
}

func assertExecTerminationError(t *testing.T, err error) {
	t.Helper()
	var terminationErr *ExecTerminationError
	if !errors.As(err, &terminationErr) {
		t.Fatalf("error = %v, want *ExecTerminationError", err)
	}
	if !errors.Is(err, ErrExecTerminationUnsupported) {
		t.Fatalf("error = %v, want ErrExecTerminationUnsupported", err)
	}
}

func TestExecAddsDefaultDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !f.has {
		t.Fatal("Exec passed a context without a default deadline")
	}
	remaining := time.Until(f.deadline)
	if remaining <= 0 || remaining > defaultExecTimeout+time.Second {
		t.Fatalf("default deadline = %v away, want within %v", remaining, defaultExecTimeout)
	}
}

func TestExecCustomTimeoutUsesEarlierCallerDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	callerDeadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	if _, _, err := ctr.Exec(ctx, []string{"true"}, WithExecTimeout(5*time.Second)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if f.deadline.After(callerDeadline) {
		t.Fatalf("deadline = %v, want no later than caller deadline %v", f.deadline, callerDeadline)
	}
}

func TestExecCustomTimeoutBoundsLaterCallerDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	callerDeadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	if _, _, err := ctr.Exec(ctx, []string{"true"}, WithExecTimeout(20*time.Millisecond)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	remaining := time.Until(f.deadline)
	if remaining <= 0 || remaining > 200*time.Millisecond {
		t.Fatalf("custom deadline = %v away, want it to bound the later caller deadline", remaining)
	}
}

func TestExecDefaultTimeoutStopsBlockingBackend(t *testing.T) {
	original := defaultExecTimeout
	defaultExecTimeout = 20 * time.Millisecond
	defer func() { defaultExecTimeout = original }()

	f := &issue116BlockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)
	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(context.Background(), []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	got := waitForIssue116ExecResult(t, result)
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on default timeout")
	}
	data, _ := io.ReadAll(got.out)
	if !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func TestExecCancellationPreservesPartialOutput(t *testing.T) {
	f := &issue116BlockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(f.release)

	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	cancel()

	got := waitForIssue116ExecResult(t, result)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on cancellation")
	}
	data, err := io.ReadAll(got.out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if got.code != 0 || !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("code/output = %d/%q, want partial output", got.code, data)
	}
}

func TestExecTimeoutPreservesPartialOutput(t *testing.T) {
	f := &issue116BlockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)

	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	got := waitForIssue116ExecResult(t, result)
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on timeout")
	}
	data, _ := io.ReadAll(got.out)
	if !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func TestExecInfrastructureErrorPreservesOutput(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "diagnostic stdout",
		execErr:    errors.New("backend unavailable"),
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"})
	if err == nil {
		t.Fatal("want infrastructure error")
	}
	if code != 0 || out == nil {
		t.Fatalf("code/output = %d/%v, want code 0 and output", code, out)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "diagnostic stdout") || !strings.Contains(string(data), "stderr-part") {
		t.Fatalf("output = %q, want diagnostic output", data)
	}
}

type issue116InfraRunner struct {
	*fakeRunner
	execCalled bool
}

func (r *issue116InfraRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.execCalled = true
		return []byte("partial stdout"), []byte("partial stderr"), &cli.CLIError{
			Args:     args,
			ExitCode: 125,
			Stderr:   "daemon unavailable",
		}
	}
	if r.execCalled && len(args) > 0 && args[0] == "inspect" {
		return nil, nil, errors.New("inspect failed")
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecClassifiedInfrastructureErrorPreservesCodeAndOutput(t *testing.T) {
	f := &issue116InfraRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"})
	if err == nil {
		t.Fatal("want classified infrastructure error")
	}
	if code != 125 || out == nil {
		t.Fatalf("code/output = %d/%v, want code 125 and output", code, out)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 125 {
		t.Fatalf("error = %v, want classified CLI error with exit code 125", err)
	}
}

type issue116InspectDeadlineRunner struct {
	*fakeRunner
	execCalled bool
}

func (r *issue116InspectDeadlineRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.execCalled = true
		return []byte("command output"), []byte("daemon unavailable"), &cli.CLIError{Args: args, ExitCode: 125, Stderr: "daemon unavailable"}
	}
	if r.execCalled && len(args) > 0 && args[0] == "inspect" {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecInspectDeadlineDoesNotClaimRemoteTermination(t *testing.T) {
	f := &issue116InspectDeadlineRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	if code != 125 || out == nil {
		t.Fatalf("code/output = %d/%v, want status 125 and output", code, out)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want inspect deadline", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 125 {
		t.Fatalf("error = %v, want original CLIError", err)
	}
	if errors.As(err, new(*ExecTerminationError)) {
		t.Fatalf("error = %v, inspect deadline must not claim remote termination", err)
	}
}

type issue116ConcurrentExitRunner struct {
	*fakeRunner
}

func (r *issue116ConcurrentExitRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		<-ctx.Done()
		return []byte("exit stdout"), []byte("exit stderr"), errors.Join(
			&cli.CLIError{Args: args, ExitCode: 7, Stderr: "command failed"},
			ctx.Err(),
		)
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecContextTerminationPreservesExitStatus(t *testing.T) {
	f := &issue116ConcurrentExitRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"false"}, WithExecTimeout(20*time.Millisecond))
	if code != 7 || out == nil {
		t.Fatalf("code/output = %d/%v, want code 7 and output", code, out)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want exit status 7", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	assertExecTerminationError(t, err)
}

// Windows Process.Kill terminates the local CLI with exit code 1 rather
// than the negative status used for a Unix signal.
type issue116WindowsKillExecRunner struct {
	*fakeRunner
	stderr     string
	execCalled bool
}

func (r *issue116WindowsKillExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.execCalled = true
		<-ctx.Done()
		return []byte("windows stdout"), []byte("windows stderr"), errors.Join(
			&cli.CLIError{Args: args, ExitCode: 1, Stderr: r.stderr},
			ctx.Err(),
		)
	}
	return r.fakeRunner.Run(ctx, args...)
}

type issue116WindowsKillInfraRunner struct {
	*issue116WindowsKillExecRunner
}

func (r *issue116WindowsKillInfraRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if r.execCalled && len(args) > 0 && args[0] == "inspect" {
		return nil, nil, errors.New("inspect failed")
	}
	return r.issue116WindowsKillExecRunner.Run(ctx, args...)
}

func TestExecWindowsKillExitCodeStillReturnsTerminationError(t *testing.T) {
	f := &issue116WindowsKillExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	assertIssue116WindowsKillResult(t, issue116ExecResult{code: code, out: out, err: err})
}

func TestExecWindowsKillInfrastructureStatusStillReturnsTerminationError(t *testing.T) {
	f := &issue116WindowsKillInfraRunner{issue116WindowsKillExecRunner: &issue116WindowsKillExecRunner{
		fakeRunner: newTestRunner(),
		stderr:     "daemon unavailable",
	}}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	assertIssue116WindowsKillResult(t, issue116ExecResult{code: code, out: out, err: err})
}

func assertIssue116WindowsKillResult(t *testing.T, got issue116ExecResult) {
	t.Helper()
	if got.code != 1 || got.out == nil {
		t.Fatalf("code/output = %d/%v, want code 1 and output", got.code, got.out)
	}
	var cliErr *CLIError
	if !errors.As(got.err, &cliErr) || cliErr.ExitCode != 1 {
		t.Fatalf("error = %v, want local CLI exit status 1", got.err)
	}
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", got.err)
	}
	assertExecTerminationError(t, got.err)
	data, err := io.ReadAll(got.out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "windows stdout") || !strings.Contains(string(data), "windows stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func TestExecZeroTimeoutDisablesDefaultDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(0)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if f.has {
		t.Fatal("WithExecTimeout(0) unexpectedly added a deadline")
	}
}

func TestExecZeroTimeoutKeepsCallerDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	callerDeadline := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	if _, _, err := ctr.Exec(ctx, []string{"true"}, WithExecTimeout(0)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !f.has || f.deadline.After(callerDeadline) {
		t.Fatalf("deadline = %v (has=%t), want caller deadline %v", f.deadline, f.has, callerDeadline)
	}
}

type issue116PreCanceledRunner struct {
	*fakeRunner
	execCalls int
}

func (r *issue116PreCanceledRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.execCalls++
		return []byte("unexpected"), nil, &cli.CLIError{Args: args, ExitCode: 7, Stderr: "unexpected"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecPreCanceledContextDoesNotLaunchCLI(t *testing.T) {
	f := &issue116PreCanceledRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, out, err := ctr.Exec(ctx, []string{"true"})
	if code != 0 || out == nil {
		t.Fatalf("code/output = %d/%v, want zero and an output reader", code, out)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.As(err, new(*ExecTerminationError)) {
		t.Fatalf("error = %v, pre-canceled context must not claim remote termination", err)
	}
	if f.execCalls != 0 {
		t.Fatalf("exec calls = %d, want no launch", f.execCalls)
	}
}

type issue116PlainCLIErrorRaceRunner struct {
	*fakeRunner
	started chan struct{}
}

func (r *issue116PlainCLIErrorRaceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		close(r.started)
		<-ctx.Done()
		// Deliberately omit ctx.Err from the returned error. Exec must
		// normalize the effective context from the runner's context.
		return []byte("race stdout"), []byte("race stderr"), &cli.CLIError{Args: args, ExitCode: 7, Stderr: "race"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecNormalizesPlainCLIErrorCancellationRace(t *testing.T) {
	f := &issue116PlainCLIErrorRaceRunner{fakeRunner: newTestRunner(), started: make(chan struct{})}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	cancel()
	got := waitForIssue116ExecResult(t, result)

	if got.code != 7 || got.out == nil {
		t.Fatalf("code/output = %d/%v, want status 7 and output", got.code, got.out)
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want normalized context.Canceled", got.err)
	}
	var cliErr *CLIError
	if !errors.As(got.err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want original CLIError status 7", got.err)
	}
	assertExecTerminationError(t, got.err)
}

type issue116TimeoutError struct{}

func (issue116TimeoutError) Error() string   { return "structured operation timeout" }
func (issue116TimeoutError) Timeout() bool   { return true }
func (issue116TimeoutError) Temporary() bool { return true }

func TestExecRecognizesTimeoutAndSignalBeforeApplicationResult(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{
			name: "structured timeout",
			err:  errors.Join(&cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "client: i/o timeout"}, issue116TimeoutError{}),
			code: 7,
		},
		{
			name: "os deadline exceeded",
			err:  errors.Join(&cli.CLIError{Args: []string{"exec"}, ExitCode: 8, Stderr: "command timed out"}, os.ErrDeadlineExceeded),
			code: 8,
		},
		{
			name: "context deadline exceeded",
			err:  errors.Join(&cli.CLIError{Args: []string{"exec"}, ExitCode: 9, Stderr: "operation timed out"}, context.DeadlineExceeded),
			code: 9,
		},
		{
			name: "negative signal",
			err:  &cli.CLIError{Args: []string{"exec"}, ExitCode: -1, Stderr: "signal"},
			code: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{fakeRunner: newTestRunner(), execStdout: "partial stdout", execErr: tc.err}
			ctr := runTestContainer(t, f)
			f.calls = nil
			code, out, err := ctr.Exec(context.Background(), []string{"true"})
			if err == nil {
				t.Fatal("Exec returned nil error for an operation timeout/signal")
			}
			if code != tc.code || out == nil {
				t.Fatalf("code/output = %d/%v, want code %d and output", code, out, tc.code)
			}
			var cliErr *CLIError
			if !errors.As(err, &cliErr) || cliErr.ExitCode != tc.code {
				t.Fatalf("error = %v, want CLIError status %d", err, tc.code)
			}
			if f.callWith("inspect") != nil {
				t.Fatalf("timeout/signal triggered a verification probe: %v", f.calls)
			}
		})
	}
}

func TestExecDoesNotClassifyTimeoutTextFromStderr(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "application output",
		execErr: &cli.CLIError{
			Args:     []string{"exec", "myctr"},
			ExitCode: 7,
			Stderr:   "application failed: i/o timeout",
		},
	}
	ctr := runTestContainer(t, f)
	f.calls = nil

	code, out, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec returned infrastructure error for application stderr: %v", err)
	}
	if code != 7 || out == nil {
		t.Fatalf("code/output = %d/%v, want application status 7 and output", code, out)
	}
	if f.callWith("inspect") != nil {
		t.Fatalf("application stderr triggered a verification probe: %v", f.calls)
	}
}

func TestExecDoesNotClassifyTimeoutTextFromArgs(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "application output",
		execErr: &cli.CLIError{
			Args:     []string{"exec", "myctr", "command timed out"},
			ExitCode: 7,
			Stderr:   "application failed",
		},
	}
	ctr := runTestContainer(t, f)
	f.calls = nil

	code, out, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec returned infrastructure error for application argv: %v", err)
	}
	if code != 7 || out == nil {
		t.Fatalf("code/output = %d/%v, want application status 7 and output", code, out)
	}
	if f.callWith("inspect") != nil {
		t.Fatalf("application argv triggered a verification probe: %v", f.calls)
	}
}

func TestExecNonCLIErrorReturnsError(t *testing.T) {
	nonCLIErr := errors.New("spawn failed: binary not found")
	f := &execRunner{fakeRunner: newTestRunner(), execErr: nonCLIErr}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.Exec(context.Background(), []string{"echo", "hello"})
	if err == nil {
		t.Fatal("Exec succeeded, want non-CLI error")
	}
	if !errors.Is(err, nonCLIErr) {
		t.Fatalf("err = %v, want %v", err, nonCLIErr)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}
