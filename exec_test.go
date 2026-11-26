package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

type canceledExecRunner struct {
	*execRunner
}

func (r *canceledExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		return []byte("partial output"), []byte("process killed"), errors.Join(
			&cli.CLIError{Args: args, ExitCode: -1, Stderr: "process killed"},
			context.Canceled,
		)
	}
	return r.execRunner.Run(ctx, args...)
}

func TestExecReturnsCancellationWhenCLIExitIsSignal(t *testing.T) {
	f := &canceledExecRunner{execRunner: &execRunner{fakeRunner: newTestRunner()}}
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
			// Exec errors cross the redaction boundary, so only the
			// explicit UnwrapRaw escape hatch exposes the structured
			// cause. That cause must still be the original parse error.
			raw := err
			if facade, ok := err.(interface{ UnwrapRaw() error }); ok {
				raw = facade.UnwrapRaw()
			}
			if got := errors.As(raw, &syntaxErr); got != tc.wantSyntaxError {
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

// execEchoFailureRunner fails every exec call. A CLI failure carries the
// configured stderr; plainErr fails exec with a non-CLI backend error.
type execEchoFailureRunner struct {
	*fakeRunner
	stderr   string
	plainErr error
}

func (r *execEchoFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		r.recordCall(args)
		if r.plainErr != nil {
			return nil, nil, r.plainErr
		}
		return nil, []byte(r.stderr), &cli.CLIError{
			Binary:   r.binaryName(),
			Args:     args,
			ExitCode: -1,
			Stderr:   r.stderr,
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecRedactsCallerValuesFromFailures(t *testing.T) {
	if envFileLocksSupported {
		isolateEnvFileRoot(t)
	}
	const (
		cmdSecret     = "exec-command-secret-329"
		workdirSecret = "/srv/exec-workdir-secret-329"
		envSecret     = "exec-env-secret-329"
	)
	execCall := func(ctr *Container, opts []ExecOption) (int, error) {
		code, _, err := ctr.Exec(context.Background(), []string{"sh", "-c", "echo " + cmdSecret}, opts...)
		return code, err
	}
	execToCall := func(ctr *Container, opts []ExecOption) (int, error) {
		code, _, err := ctr.ExecTo(context.Background(), []string{"sh", "-c", "echo " + cmdSecret}, io.Discard, opts...)
		return code, err
	}
	cliFailure := func(_ *testing.T, _ *Container, r *execEchoFailureRunner, values []string) {
		r.stderr = "exec failed for " + strings.Join(values, " ")
	}
	backendFailure := func(_ *testing.T, _ *Container, r *execEchoFailureRunner, values []string) {
		r.plainErr = errors.New("backend unavailable for " + strings.Join(values, " "))
	}
	// A handle without its creation generation fails operation-target
	// verification before any backend call.
	verifyFailure := func(_ *testing.T, ctr *Container, _ *execEchoFailureRunner, _ []string) {
		ctr.creation = ""
	}
	// A cache root that is a regular file named after the command makes
	// the env-file write fail with a path error that echoes a caller value.
	envFileFailure := func(t *testing.T, _ *Container, _ *execEchoFailureRunner, _ []string) {
		blocked := filepath.Join(t.TempDir(), cmdSecret)
		if err := os.WriteFile(blocked, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", blocked)
		t.Setenv("XDG_CACHE_HOME", blocked)
		t.Setenv("LocalAppData", blocked)
	}
	cases := []struct {
		name    string
		call    func(*Container, []ExecOption) (int, error)
		arm     func(*testing.T, *Container, *execEchoFailureRunner, []string)
		code    int
		wantCLI bool
		envOnly bool
	}{
		{name: "Exec CLI failure", call: execCall, arm: cliFailure, code: -1, wantCLI: true},
		{name: "ExecTo CLI failure", call: execToCall, arm: cliFailure, code: -1, wantCLI: true},
		{name: "Exec backend failure", call: execCall, arm: backendFailure},
		{name: "ExecTo backend failure", call: execToCall, arm: backendFailure},
		{name: "Exec verification failure", call: execCall, arm: verifyFailure},
		{name: "ExecTo verification failure", call: execToCall, arm: verifyFailure},
		{name: "Exec env-file failure", call: execCall, arm: envFileFailure, envOnly: true},
		{name: "ExecTo env-file failure", call: execToCall, arm: envFileFailure, envOnly: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envOnly && !envFileLocksSupported {
				t.Skip("secure environment files are unsupported on this platform")
			}
			values := []string{cmdSecret, workdirSecret}
			opts := []ExecOption{WithExecWorkDir(workdirSecret)}
			if envFileLocksSupported {
				opts = append(opts, WithExecEnv(map[string]string{"TOKEN": envSecret}))
				values = append(values, envSecret)
			}
			r := &execEchoFailureRunner{fakeRunner: newTestRunner()}
			ctr := runTestContainer(t, r)
			tc.arm(t, ctr, r, values)

			code, err := tc.call(ctr, opts)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			if err == nil {
				t.Fatal("Exec returned no error for a failed operation")
			}
			if _, ok := err.(interface{ UnwrapRaw() error }); !ok {
				t.Fatalf("error %T bypassed the redaction boundary: %v", err, err)
			}
			for _, value := range values {
				if strings.Contains(err.Error(), value) {
					t.Errorf("error = %q, leaks caller value %q", err, value)
				}
			}
			var cliErr *CLIError
			if !tc.wantCLI {
				if errors.As(err, &cliErr) {
					t.Fatalf("non-CLI failure exposed a CLIError: %v", err)
				}
				return
			}
			if !errors.As(err, &cliErr) {
				t.Fatalf("errors.As did not find *CLIError: %v", err)
			}
			for _, value := range values {
				if strings.Contains(cliErr.Stderr, value) ||
					strings.Contains(strings.Join(cliErr.Args, " "), value) ||
					strings.Contains(cliErr.Error(), value) {
					t.Errorf("redacted CLIError = %#v, leaks caller value %q", cliErr, value)
				}
			}
		})
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

type blockingExecRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
}

func (r *blockingExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
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

type deadlineExecRunner struct {
	*fakeRunner
	deadline time.Time
	has      bool
}

func (r *deadlineExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.deadline, r.has = ctx.Deadline()
		return []byte("ok"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type execResult struct {
	code int
	out  io.Reader
	err  error
}

func waitForExecStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("exec stub was not started")
	}
}

func waitForExecResult(t *testing.T, result <-chan execResult) execResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(time.Second):
		t.Fatal("Exec did not return")
		return execResult{}
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
	f := &deadlineExecRunner{fakeRunner: newTestRunner()}
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
	f := &deadlineExecRunner{fakeRunner: newTestRunner()}
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
	f := &deadlineExecRunner{fakeRunner: newTestRunner()}
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

	f := &blockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)
	result := make(chan execResult, 1)
	go func() {
		code, out, err := ctr.Exec(context.Background(), []string{"true"})
		result <- execResult{code: code, out: out, err: err}
	}()
	waitForExecStart(t, f.started)
	got := waitForExecResult(t, result)
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
	f := &blockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(f.release)

	result := make(chan execResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- execResult{code: code, out: out, err: err}
	}()
	waitForExecStart(t, f.started)
	cancel()

	got := waitForExecResult(t, result)
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
	f := &blockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)

	result := make(chan execResult, 1)
	go func() {
		code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
		result <- execResult{code: code, out: out, err: err}
	}()
	waitForExecStart(t, f.started)
	got := waitForExecResult(t, result)
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

type infraRunner struct {
	*fakeRunner
	execCalled bool
}

func (r *infraRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
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
	f := &infraRunner{fakeRunner: newTestRunner()}
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

type inspectDeadlineRunner struct {
	*fakeRunner
	execCalled bool
}

func (r *inspectDeadlineRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
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
	f := &inspectDeadlineRunner{fakeRunner: newTestRunner()}
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

type concurrentExitRunner struct {
	*fakeRunner
}

func (r *concurrentExitRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
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
	f := &concurrentExitRunner{fakeRunner: newTestRunner()}
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
type windowsKillExecRunner struct {
	*fakeRunner
	stderr     string
	execCalled bool
}

func (r *windowsKillExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
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

type windowsKillInfraRunner struct {
	*windowsKillExecRunner
}

func (r *windowsKillInfraRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if r.execCalled && len(args) > 0 && args[0] == "inspect" {
		return nil, nil, errors.New("inspect failed")
	}
	return r.windowsKillExecRunner.Run(ctx, args...)
}

func TestExecWindowsKillExitCodeStillReturnsTerminationError(t *testing.T) {
	f := &windowsKillExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	assertWindowsKillResult(t, execResult{code: code, out: out, err: err})
}

func TestExecWindowsKillInfrastructureStatusStillReturnsTerminationError(t *testing.T) {
	f := &windowsKillInfraRunner{windowsKillExecRunner: &windowsKillExecRunner{
		fakeRunner: newTestRunner(),
		stderr:     "daemon unavailable",
	}}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	assertWindowsKillResult(t, execResult{code: code, out: out, err: err})
}

func assertWindowsKillResult(t *testing.T, got execResult) {
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
	f := &deadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(0)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if f.has {
		t.Fatal("WithExecTimeout(0) unexpectedly added a deadline")
	}
}

func TestExecZeroTimeoutKeepsCallerDeadline(t *testing.T) {
	f := &deadlineExecRunner{fakeRunner: newTestRunner()}
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

type preCanceledRunner struct {
	*fakeRunner
	execCalls int
}

func (r *preCanceledRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.execCalls++
		return []byte("unexpected"), nil, &cli.CLIError{Args: args, ExitCode: 7, Stderr: "unexpected"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecPreCanceledContextDoesNotLaunchCLI(t *testing.T) {
	f := &preCanceledRunner{fakeRunner: newTestRunner()}
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

type plainCLIErrorRaceRunner struct {
	*fakeRunner
	started chan struct{}
}

func (r *plainCLIErrorRaceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
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
	f := &plainCLIErrorRaceRunner{fakeRunner: newTestRunner(), started: make(chan struct{})}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan execResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- execResult{code: code, out: out, err: err}
	}()
	waitForExecStart(t, f.started)
	cancel()
	got := waitForExecResult(t, result)

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

type testTimeoutError struct{}

func (testTimeoutError) Error() string   { return "structured operation timeout" }
func (testTimeoutError) Timeout() bool   { return true }
func (testTimeoutError) Temporary() bool { return true }

func TestExecRecognizesTimeoutAndSignalBeforeApplicationResult(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{
			name: "structured timeout",
			err:  errors.Join(&cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "client: i/o timeout"}, testTimeoutError{}),
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

// stdinExecRunner adds the optional cli.StdinRunner capability to the fake
// runner. It echoes the received reader back through stdout so a test can
// verify that the public option reached the runner and was actually consumed.
type stdinExecRunner struct {
	*fakeRunner
	stdin     []byte
	stdinArgs []string
	runErr    error
}

func (r *stdinExecRunner) RunWithStdin(_ context.Context, stdin io.Reader, stdout, _ io.Writer, args ...string) (cli.OutputStats, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return cli.OutputStats{}, err
	}
	r.mu.Lock()
	r.stdin = data
	r.stdinArgs = append([]string(nil), args...)
	r.calls = append(r.calls, args)
	r.mu.Unlock()
	n, writeErr := stdout.Write(data)
	if r.runErr != nil {
		return cli.OutputStats{StdoutBytes: int64(n)}, r.runErr
	}
	return cli.OutputStats{StdoutBytes: int64(n)}, writeErr
}

func (r *stdinExecRunner) capturedStdin() ([]byte, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.stdin...), append([]string(nil), r.stdinArgs...)
}

func TestExecWithStdinWiresReaderAndAddsInteractiveFlag(t *testing.T) {
	f := &stdinExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"psql", "-f", "-"},
		WithExecStdin(strings.NewReader("SELECT 1;\n")))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	data, _ := io.ReadAll(out)
	if got, want := string(data), "SELECT 1;\n"; got != want {
		t.Errorf("output = %q, want the stdin payload", got)
	}
	stdin, args := f.capturedStdin()
	if got, want := string(stdin), "SELECT 1;\n"; got != want {
		t.Errorf("stdin = %q, want %q", got, want)
	}
	interactive := slices.Index(args, "-i")
	target := slices.Index(args, "myctr")
	if interactive < 0 || target < 0 || interactive > target {
		t.Errorf("exec args = %v, want -i before the container target", args)
	}
	if !slices.Equal(args[target+1:], []string{"psql", "-f", "-"}) {
		t.Errorf("exec command suffix = %v", args[target+1:])
	}
}

func TestExecToWithStdinWiresReader(t *testing.T) {
	f := &stdinExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	var output strings.Builder
	code, stats, err := ctr.ExecTo(context.Background(), []string{"mysql"}, &output,
		WithExecStdin(strings.NewReader("INSERT 1;\n")))
	if err != nil {
		t.Fatalf("ExecTo: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got, want := output.String(), "INSERT 1;\n"; got != want {
		t.Errorf("streamed output = %q, want the stdin payload", got)
	}
	if got, want := stats.Bytes, int64(len("INSERT 1;\n")); got != want {
		t.Errorf("stats.Bytes = %d, want %d", got, want)
	}
	_, args := f.capturedStdin()
	if !slices.Contains(args, "-i") {
		t.Errorf("exec args = %v, want -i", args)
	}
}

func TestExecWithStdinHonorsMaxBytes(t *testing.T) {
	f := &stdinExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	payload := strings.Repeat("x", 64)

	code, out, err := ctr.Exec(context.Background(), []string{"cat"},
		WithExecStdin(strings.NewReader(payload)), WithExecMaxBytes(8))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	data, readErr := io.ReadAll(out)
	if len(data) != 8 {
		t.Errorf("retained output = %q, want the 8-byte prefix", data)
	}
	if !errors.Is(readErr, ErrOutputTruncated) {
		t.Errorf("read error = %v, want ErrOutputTruncated", readErr)
	}
	stdin, _ := f.capturedStdin()
	if len(stdin) != len(payload) {
		t.Errorf("stdin length = %d, want the full %d-byte dump", len(stdin), len(payload))
	}
}

func TestExecWithStdinRejectsTypedNilReader(t *testing.T) {
	f := &stdinExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	var reader *bytes.Reader

	code, out, err := ctr.Exec(context.Background(), []string{"cat"}, WithExecStdin(reader))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Option != "WithExecStdin" {
		t.Fatalf("error = %v, want *ValidationError for WithExecStdin", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Option validation returns before output capture starts.
	if out != nil {
		data, _ := io.ReadAll(out)
		if len(data) != 0 {
			t.Errorf("output = %q, want none", data)
		}
	}
	if call := f.callWith("exec"); call != nil {
		t.Errorf("typed-nil stdin still invoked the CLI: %v", call)
	}
}

func TestExecWithStdinReturnsReaderError(t *testing.T) {
	readErr := errors.New("stdin source failed")
	f := &stdinExecRunner{
		fakeRunner: newTestRunner(),
		runErr:     &cli.StdinReadError{Err: readErr},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"cat"},
		WithExecStdin(strings.NewReader("partial")))
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !errors.Is(err, readErr) || !errors.Is(err, cli.ErrStdinRead) {
		t.Fatalf("error = %v, want the reader failure and ErrStdinRead", err)
	}
	data, _ := io.ReadAll(out)
	if string(data) != "partial" {
		t.Errorf("output = %q, want the data read before the failure", data)
	}
}

func TestExecWithStdinPreservesReaderErrorWithCommandExit(t *testing.T) {
	readErr := errors.New("stdin source failed")
	cliErr := &cli.CLIError{Binary: "container", Args: []string{"exec", "myctr", "cat"}, ExitCode: 7, Stderr: "command failed"}
	f := &stdinExecRunner{
		fakeRunner: newTestRunner(),
		runErr:     errors.Join(cliErr, &cli.StdinReadError{Err: readErr}),
	}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.Exec(context.Background(), []string{"cat"},
		WithExecStdin(strings.NewReader("partial")))
	if code != 7 {
		t.Errorf("exit code = %d, want the command status 7", code)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want the reader failure alongside the command exit", err)
	}
	if !errors.Is(err, cli.ErrStdinRead) {
		t.Fatalf("error = %v, want ErrStdinRead alongside the command exit", err)
	}
}

func TestExecWithStdinRequiresStdinCapableRunner(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	before := len(f.calls)

	_, _, err := ctr.Exec(context.Background(), []string{"cat"}, WithExecStdin(strings.NewReader("x")))
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
	}
	if got := len(f.calls); got != before {
		t.Errorf("CLI calls grew from %d to %d; unsupported stdin must not invoke the backend", before, got)
	}
}
