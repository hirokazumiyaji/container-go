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
	if cliErr.Binary == "" || !strings.Contains(cliErr.Error(), cliErr.Binary) {
		t.Errorf("Binary = %q, Error() = %q", cliErr.Binary, cliErr.Error())
	}
}

func TestExecRunnerCapsStderr(t *testing.T) {
	// Emit ~1MiB of stderr, far beyond the 64KiB cap.
	r := &ExecRunner{Binary: writeStub(t, `i=0; while [ $i -lt 16384 ]; do printf '%064d\n' "$i" >&2; i=$((i+1)); done; exit 1`)}

	stdout, stderr, err := r.Run(context.Background(), "run")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
	// The returned output buffers stay whole for exec/log results.
	if len(stderr) <= maxStderr {
		t.Errorf("len(returned stderr) = %d, want > %d", len(stderr), maxStderr)
	}
	_ = stdout
}

func TestExecRunnerPreservesLargeSuccessOutput(t *testing.T) {
	// 128 KiB on each stream with exit 0 must come back whole.
	r := &ExecRunner{Binary: writeStub(t, `head -c 131072 /dev/zero; head -c 131072 /dev/zero >&2`)}
	stdout, stderr, err := r.Run(context.Background(), "exec")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stdout) != 131072 {
		t.Errorf("len(stdout) = %d, want 131072", len(stdout))
	}
	if len(stderr) != 131072 {
		t.Errorf("len(stderr) = %d, want 131072", len(stderr))
	}
}

func TestExecRunnerPreservesLargeFailureOutput(t *testing.T) {
	// Non-zero exit still returns whole output; only CLIError is capped.
	r := &ExecRunner{Binary: writeStub(t, `head -c 131072 /dev/zero; head -c 131072 /dev/zero >&2; exit 7`)}
	stdout, stderr, err := r.Run(context.Background(), "exec")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if len(stdout) != 131072 {
		t.Errorf("len(stdout) = %d, want 131072", len(stdout))
	}
	if len(stderr) != 131072 {
		t.Errorf("len(returned stderr) = %d, want 131072", len(stderr))
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(CLIError.Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
	if cliErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cliErr.ExitCode)
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

func TestExecRunnerPreservesOutputOnContextCancellation(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'partial stdout'; printf 'partial stderr' >&2; sleep 30`)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	stdout, stderr, err := r.Run(ctx, "exec", "ctr", "true")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if string(stdout) != "partial stdout" || string(stderr) != "partial stderr" {
		t.Fatalf("output = %q/%q, want partial output", stdout, stderr)
	}
}

func TestCLIErrorIncludesBinaryName(t *testing.T) {
	err := &CLIError{Binary: "docker", Args: []string{"run", "--detach"}, ExitCode: 125, Stderr: "conflict"}
	got := err.Error()
	if !strings.HasPrefix(got, "docker run --detach:") {
		t.Errorf("Error() = %q, want docker prefix", got)
	}
}

func TestCLIErrorDefaultsBinaryToContainer(t *testing.T) {
	err := &CLIError{Args: []string{"inspect", "x"}, ExitCode: 1}
	if !strings.HasPrefix(err.Error(), "container inspect x:") {
		t.Errorf("Error() = %q", err.Error())
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

var appleProbe = Probe{Args: []string{"system", "status"}, Hint: "run `container system start`"}

func TestClassifyReturnsSystemNotRunningWhenStatusProbeFails(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: &CLIError{Args: []string{"system", "status"}, ExitCode: 1}},
	}}

	err := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !strings.Contains(err.Error(), "container system start") {
		t.Errorf("Error() = %q, want hint to run 'container system start'", err.Error())
	}
}

func TestClassifyUsesProbeSpecificHint(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "cannot connect"}
	probeArgs := []string{"version", "--format", "{{.Server.Version}}"}
	r := &fakeRunner{results: map[string]fakeResult{
		strings.Join(probeArgs, " "): {err: &CLIError{Args: probeArgs, ExitCode: 1}},
	}}

	err := Classify(context.Background(), r, orig, Probe{Args: probeArgs, Hint: "start the Docker daemon"})
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !strings.Contains(err.Error(), "start the Docker daemon") {
		t.Errorf("Error() = %q, want docker hint", err.Error())
	}
}

func TestClassifyKeepsOriginalErrorWhenSystemIsRunning(t *testing.T) {
	orig := &CLIError{Args: []string{"inspect", "x"}, ExitCode: 1, Stderr: "not found"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {stdout: "apiserver is running"},
	}}

	err := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original error preserved", err)
	}
	if errors.Is(err, ErrSystemNotRunning) {
		t.Error("error wrongly classified as ErrSystemNotRunning")
	}
}

func TestClassifyPassesThroughNil(t *testing.T) {
	if err := Classify(context.Background(), &fakeRunner{}, nil, appleProbe); err != nil {
		t.Fatalf("Classify(nil) = %v, want nil", err)
	}
}

type hangingProbeRunner struct {
	started chan struct{}
}

func (h *hangingProbeRunner) Run(ctx context.Context, _ ...string) ([]byte, []byte, error) {
	close(h.started)
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func TestClassifyProbeTimesOut(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{started: make(chan struct{})}
	start := time.Now()
	err := Classify(context.Background(), r, orig, appleProbe)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Classify took %v, want finite probe timeout", elapsed)
	}
	select {
	case <-r.started:
	default:
		t.Error("probe was not invoked")
	}
}

func TestClassifyRespectsCallerCancel(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Classify(ctx, r, orig, appleProbe)
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original preserved on cancel", err)
	}
}

func TestClassifyPreservesOriginalWhenParentCancelsDuringProbe(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-r.started
		cancel()
	}()
	start := time.Now()
	err := Classify(ctx, r, orig, appleProbe)
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original when parent cancels mid-probe", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Classify took %v, want fast return on parent cancel", elapsed)
	}
}

func TestIsCommandExit(t *testing.T) {
	if !IsCommandExit(&CLIError{Args: []string{"exec"}, ExitCode: 1}) {
		t.Error("CLIError should be a command exit")
	}
	if IsCommandExit(errors.New("executable file not found")) {
		t.Error("launch failure must not count as command exit")
	}
	if IsCommandExit(nil) {
		t.Error("nil must not count as command exit")
	}
}
