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

// writeStub creates an executable shell script and returns its path. It skips
// on Windows, which has no /bin/sh, so the stub is never executed there.
func writeStub(t *testing.T, script string) string {
	t.Helper()
	requirePOSIXShell(t)
	path := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIErrorRetainsUnkeyedFieldLayout(t *testing.T) {
	err := CLIError{"docker", []string{"version"}, 1, "failed"}
	if err.Binary != "docker" || err.ExitCode != 1 || err.Stderr != "failed" {
		t.Fatalf("CLIError = %#v, want the historical four-field layout", err)
	}
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

func TestExecRunnerPreservesFailureStdout(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `echo "probe diagnostic"; echo "probe stderr" >&2; exit 1`)}

	stdout, _, err := r.Run(context.Background(), "system", "status")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if got := string(stdout); got != "probe diagnostic\n" {
		t.Errorf("raw stdout = %q, want %q", got, "probe diagnostic\\n")
	}
	stdoutDiagnostic, _, ok := DiagnosticText(err)
	if !ok || stdoutDiagnostic != "probe diagnostic\n" {
		t.Errorf("stdout diagnostic = %q (ok=%t), want %q", stdoutDiagnostic, ok, "probe diagnostic\\n")
	}
	if !strings.Contains(err.Error(), "probe diagnostic") {
		t.Errorf("Error() = %q, want stdout diagnostic", err.Error())
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
	if stdoutDiagnostic, _, ok := DiagnosticText(err); !ok || len(stdoutDiagnostic) > maxStderr {
		t.Errorf("len(stdout diagnostic) = %d (ok=%t), want <= %d", len(stdoutDiagnostic), ok, maxStderr)
	}
	if cliErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cliErr.ExitCode)
	}
}

func TestExecRunnerPreservesLaunchErrorAndCancellation(t *testing.T) {
	r := &ExecRunner{Binary: filepath.Join(t.TempDir(), "missing-cli")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := r.Run(ctx, "version")
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "missing-cli") {
		t.Fatalf("error = %v, want original launch diagnostic", err)
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

func TestExecRunnerPreservesCLIErrorWhenCancellationRacesWithExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "process-exited")
	r := &ExecRunner{Binary: writeStub(t, `
parent=$$
(
  while kill -0 "$parent" 2>/dev/null; do sleep 0.01; done
  : > "$1"
  sleep 0.1
) &
echo "real exit" >&2
exit 3
`)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(marker); err == nil {
					cancel()
					return
				}
			}
		}
	}()

	_, stderr, err := r.Run(ctx, marker)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError after a real non-zero exit", err)
	}
	if cliErr.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "real exit") {
		t.Errorf("CLIError.Stderr = %q, want real command diagnostic", cliErr.Stderr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want concurrent context cancellation", err)
	}
	if got := string(stderr); !strings.Contains(got, "real exit") {
		t.Errorf("stderr = %q, want real command diagnostic", got)
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

func testProbeUnavailable(err error) bool {
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	s := strings.ToLower(cliErr.Stderr)
	return strings.Contains(s, "xpc") || strings.Contains(s, "cannot connect")
}

var appleProbe = Probe{
	Args:          []string{"system", "status"},
	Hint:          "run `container system start`",
	IsUnavailable: testProbeUnavailable,
}

func TestClassifyReturnsSystemNotRunningWhenStatusProbeFails(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}},
	}}

	err := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !strings.Contains(err.Error(), "container system start") {
		t.Errorf("Error() = %q, want hint to run 'container system start'", err.Error())
	}
}

func TestClassifyPreservesOriginalAndProbeErrorChains(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "permission denied"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}

	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, orig) {
		t.Errorf("classified error = %v, want original error", got)
	}
	if !errors.Is(got, probeErr) {
		t.Errorf("classified error = %v, want probe error", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Errorf("classified error = %v, permission failure must not be ErrSystemNotRunning", got)
	}
}

func TestClassifyUsesProbeSpecificHint(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "cannot connect"}
	probeArgs := []string{"version", "--format", "{{.Server.Version}}"}
	r := &fakeRunner{results: map[string]fakeResult{
		strings.Join(probeArgs, " "): {err: &CLIError{Args: probeArgs, ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}},
	}}

	err := Classify(context.Background(), r, orig, Probe{
		Args:          probeArgs,
		Hint:          "start the Docker daemon",
		IsUnavailable: testProbeUnavailable,
	})
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
	started         chan struct{}
	afterContextErr error
}

func (h *hangingProbeRunner) Run(ctx context.Context, _ ...string) ([]byte, []byte, error) {
	close(h.started)
	<-ctx.Done()
	if h.afterContextErr != nil {
		return nil, nil, h.afterContextErr
	}
	return nil, nil, ctx.Err()
}

type issue104RecordingProbeRunner struct {
	calls int
	err   error
}

func (r *issue104RecordingProbeRunner) Run(_ context.Context, _ ...string) ([]byte, []byte, error) {
	r.calls++
	return nil, nil, r.err
}

type issue104JoinedProbeRunner struct {
	err    error
	stdout string
}

func (r *issue104JoinedProbeRunner) Run(_ context.Context, _ ...string) ([]byte, []byte, error) {
	return []byte(r.stdout), nil, r.err
}

func TestClassifyDoesNotProbeKnownOperationTimeout(t *testing.T) {
	original := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command timed out"}
	operationErr := errors.Join(original, context.DeadlineExceeded)
	runner := &issue104RecordingProbeRunner{err: &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}}

	got := Classify(context.Background(), runner, operationErr, appleProbe)
	if runner.calls != 0 {
		t.Fatalf("probe calls = %d, want 0 after operation timeout", runner.calls)
	}
	if !errors.Is(got, original) || !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want original and operation timeout chains", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, operation timeout must not be classified as daemon down", got)
	}
}

type timeoutTestError struct{}

func (timeoutTestError) Error() string { return "typed timeout" }
func (timeoutTestError) Timeout() bool { return true }

func TestIsOperationTimeoutErrorUsesStructuredEvidence(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "context deadline", err: context.DeadlineExceeded, want: true},
		{name: "typed timeout", err: timeoutTestError{}, want: true},
		{name: "signal CLI exit", err: &CLIError{Args: []string{"exec"}, ExitCode: -1}, want: true},
		{
			name: "application stderr",
			err:  &CLIError{Args: []string{"exec", "myctr", "query"}, ExitCode: 7, Stderr: "i/o timeout"},
		},
		{
			name: "application argv",
			err:  &CLIError{Args: []string{"exec", "myctr", "command timed out"}, ExitCode: 7, Stderr: "application failed"},
		},
		{
			name: "ordinary non-zero CLI exit",
			err:  &CLIError{Args: []string{"exec", "myctr", "query"}, ExitCode: 7, Stderr: "application failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOperationTimeoutError(tc.err); got != tc.want {
				t.Fatalf("IsOperationTimeoutError(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyJoinedProbeConfigurationVetoesLiveness(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	liveness := &CLIError{
		Args: []string{"version"}, ExitCode: 1,
		Stderr: "cannot connect to the Docker daemon",
	}
	probeErr := errors.Join(liveness, errors.New("x509: certificate signed by unknown authority"))
	runner := &issue104JoinedProbeRunner{err: probeErr, stdout: "probe stdout"}
	probe := Probe{
		Args: []string{"version"}, Hint: "start the daemon",
		IsUnavailable: func(error) bool { return true },
	}

	got := Classify(context.Background(), runner, orig, probe)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, joined configuration branch must veto liveness", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, liveness) {
		t.Fatalf("error = %v, want original and probe error chains", got)
	}
}

func TestClassifyProbeTimeoutPreservesExplicitDeadline(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	r := &hangingProbeRunner{
		started:         make(chan struct{}),
		afterContextErr: errors.New("transport stopped"),
	}
	start := time.Now()
	err := Classify(context.Background(), r, orig, appleProbe)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original error", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want probe timeout", err)
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want probe cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Classify took %v, want fast return on parent cancel", elapsed)
	}
}

func TestClassifyDoesNotAddSystemDownAfterPredicateCancellation(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := Probe{
		Args: []string{"system", "status"},
		Hint: "run `container system start`",
		IsUnavailable: func(error) bool {
			cancel()
			return true
		},
	}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}

	got := Classify(ctx, r, orig, probe)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, must not classify after caller cancellation", got)
	}
	for _, want := range []error{orig, probeErr, context.Canceled} {
		if !errors.Is(got, want) {
			t.Errorf("error = %v, want chain to include %v", got, want)
		}
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
