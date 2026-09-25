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
			execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 1, Stderr: `not found: "myctr"`},
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
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
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

func TestExecReturnsOperationTimeoutAsInfrastructureError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{name: "i/o timeout", stderr: "client: i/o timeout"},
		{name: "command timed out", stderr: "command timed out"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{
				fakeRunner: newTestRunner(),
				execStdout: "partial stdout",
				execErr: &cli.CLIError{
					Args:     []string{"exec"},
					ExitCode: 7,
					Stderr:   tc.stderr,
				},
			}
			ctr := runTestContainer(t, f)

			code, out, err := ctr.Exec(context.Background(), []string{"query"})
			if err == nil {
				t.Fatal("Exec returned nil error for an operation timeout")
			}
			if code != 7 {
				t.Errorf("exit code = %d, want 7", code)
			}
			if out == nil {
				t.Fatal("Exec returned nil output for an operation timeout")
			}
			var cliErr *cli.CLIError
			if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
				t.Fatalf("error = %v, want the original timeout CLIError", err)
			}
			if f.callWith("version") != nil || f.callWith("system") != nil {
				t.Errorf("timeout triggered an infrastructure probe: %v", f.calls)
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
