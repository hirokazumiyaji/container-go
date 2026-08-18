package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeStub creates an executable shell script and returns its path.
func writeStub(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRunnerReturnsStdout(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `echo "out $1"; echo "err" >&2`)}

	stdout, stderr, err := r.Run(context.Background(), "ls")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(stdout); got != "out ls\n" {
		t.Errorf("stdout = %q, want %q", got, "out ls\n")
	}
	if got := string(stderr); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
}

func TestExecRunnerNonZeroExitReturnsCLIError(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `echo "boom" >&2; exit 3`)}

	_, _, err := r.Run(context.Background(), "inspect", "missing")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if cliErr.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "boom") {
		t.Errorf("Stderr = %q, want to contain %q", cliErr.Stderr, "boom")
	}
	if !strings.Contains(cliErr.Error(), "inspect") {
		t.Errorf("Error() = %q, want to contain subcommand %q", cliErr.Error(), "inspect")
	}
}

func TestExecRunnerCapsStderr(t *testing.T) {
	// Emit ~1MiB of stderr, far beyond the 64KiB cap.
	r := &ExecRunner{Binary: writeStub(t, `i=0; while [ $i -lt 16384 ]; do printf '%064d\n' "$i" >&2; i=$((i+1)); done; exit 1`)}

	_, _, err := r.Run(context.Background(), "run")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
}

func TestExecRunnerHonorsContextCancellation(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `sleep 30`)}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := r.Run(ctx, "logs", "--follow", "x")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %v, want prompt return after cancellation", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestExecRunnerDefaultsToContainerBinary(t *testing.T) {
	r := &ExecRunner{}
	if got := r.binary(); got != "container" {
		t.Errorf("binary() = %q, want %q", got, "container")
	}
}

type fakeRunner struct {
	results map[string]fakeResult
}

type fakeResult struct {
	stdout string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	res, ok := f.results[strings.Join(args, " ")]
	if !ok {
		return nil, nil, &CLIError{Args: args, ExitCode: 1, Stderr: "unexpected command"}
	}
	return []byte(res.stdout), nil, res.err
}

func TestClassifyReturnsSystemNotRunningWhenStatusProbeFails(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: &CLIError{Args: []string{"system", "status"}, ExitCode: 1}},
	}}

	err := Classify(context.Background(), r, orig)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !strings.Contains(err.Error(), "container system start") {
		t.Errorf("Error() = %q, want hint to run 'container system start'", err.Error())
	}
}

func TestClassifyKeepsOriginalErrorWhenSystemIsRunning(t *testing.T) {
	orig := &CLIError{Args: []string{"inspect", "x"}, ExitCode: 1, Stderr: "not found"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {stdout: "apiserver is running"},
	}}

	err := Classify(context.Background(), r, orig)
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original error preserved", err)
	}
	if errors.Is(err, ErrSystemNotRunning) {
		t.Error("error wrongly classified as ErrSystemNotRunning")
	}
}

func TestClassifyPassesThroughNil(t *testing.T) {
	if err := Classify(context.Background(), &fakeRunner{}, nil); err != nil {
		t.Fatalf("Classify(nil) = %v, want nil", err)
	}
}
