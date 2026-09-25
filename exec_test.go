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
			execErr:    &cli.CLIError{Binary: "container", Args: []string{"exec"}, ExitCode: 1, Stderr: `Error: get failed: container myctr not found`},
		},
	}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err == nil {
		t.Fatal("want error for missing container")
	}
}

type execMissingRunner struct {
	*execRunner
}

func (m *execMissingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: `Error: container not found: myctr`}
	}
	return m.execRunner.Run(ctx, args...)
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
