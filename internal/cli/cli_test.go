package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// Emit ~1MiB of stderr, far beyond the 64KiB cap, then finish with
	// the diagnostic that classification needs.
	r := &ExecRunner{Binary: writeStub(t, `printf 'FIRST_BYTES_SENTINEL\n' >&2; i=0; while [ $i -lt 16384 ]; do printf '%064d\n' "$i" >&2; i=$((i+1)); done; printf 'Error response from daemon: No such container: terminal\n' >&2; exit 1`)}

	stdout, stderr, err := r.Run(context.Background(), "run")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
	if !strings.Contains(cliErr.Stderr, "No such container: terminal") {
		t.Errorf("Stderr = %q, want terminal not-found diagnostic", cliErr.Stderr)
	}
	if strings.Contains(cliErr.Stderr, "FIRST_BYTES_SENTINEL") {
		t.Errorf("Stderr retained the first bytes instead of the terminal tail")
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
	r := &ExecRunner{Binary: writeStub(t, `sleep 5`)}

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

func TestExecRunnerCancellationDuringStartUsesPublicationBarrier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelObserved := make(chan struct{})
	terminateEntered := make(chan struct{})
	r := &ExecRunner{Binary: writeStub(t, `exec sleep 30`)}
	hooks := runHooks{
		start: func(cmd *exec.Cmd) error {
			if err := cmd.Start(); err != nil {
				return err
			}
			// The process exists, but Run has not published its ownership
			// barrier or started its lifecycle Wait yet.
			cancel()
			return nil
		},
		beforePublish: func() {
			select {
			case <-cancelObserved:
			case <-time.After(5 * time.Second):
				t.Error("context cancellation did not reach the publication barrier")
			}
			select {
			case <-terminateEntered:
				t.Error("termination ran before lifecycle publication")
			default:
			}
		},
		cancelObserved: func() { close(cancelObserved) },
		terminate: func(cmd *exec.Cmd) error {
			close(terminateEntered)
			return terminateProcessTree(cmd)
		},
	}

	_, _, err := r.run(ctx, hooks, "logs")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestCommandRunErrorCancellationRacePreservesExitAndContext(t *testing.T) {
	binary := writeStub(t, `echo cancel-race >&2; exit 7`)
	cmd := exec.Command(binary)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	exitErr := cmd.Run()
	if exitErr == nil {
		t.Fatal("stub command unexpectedly succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := commandRunError(ctx, binary, []string{"exec", "myctr", "query"}, stderr.Bytes(), exitErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want joined *CLIError", err)
	}
	if cliErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cliErr.ExitCode)
	}
	if cliErr.Binary != binary || !slices.Equal(cliErr.Args, []string{"exec", "myctr", "query"}) {
		t.Errorf("CLIError command = %q %v", cliErr.Binary, cliErr.Args)
	}
	if !strings.Contains(cliErr.Stderr, "cancel-race") {
		t.Errorf("Stderr = %q, want command diagnostic", cliErr.Stderr)
	}
}

func TestExecRunnerPreservesOutputOnContextCancellation(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'partial stdout'; printf 'partial stderr' >&2; sleep 5`)}

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

func TestCommandErrorPreservesExitStatusWhenContextExpires(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	rawErr := cmd.Run()
	if rawErr == nil {
		t.Fatal("stub command unexpectedly succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := commandError(ctx, "stub", []string{"exec"}, nil, nil, rawErr)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want CLIError exit code 7", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestExecRunnerReportsWhetherLocalProcessStarted(t *testing.T) {
	missing := &ExecRunner{Binary: filepath.Join(t.TempDir(), "missing")}
	_, _, err := missing.Run(context.Background(), "exec")
	started, reported := StartedStatus(err)
	if !reported || started {
		t.Fatalf("missing binary start status = %t/%t, want false/true", started, reported)
	}

	failed := &ExecRunner{Binary: writeStub(t, `exit 7`)}
	_, _, err = failed.Run(context.Background(), "exec")
	started, reported = StartedStatus(err)
	if !reported || !started {
		t.Fatalf("started command status = %t/%t, want true/true", started, reported)
	}
}

type inactiveTerminationTree struct{}

func (inactiveTerminationTree) terminate(cmd *exec.Cmd) terminationResult {
	_ = cmd.Process.Kill()
	return terminationResult{active: false}
}
func (inactiveTerminationTree) close() {}

func TestLifecycleDoesNotMarkEmptyTerminationAsActive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command(writeStub(t, `sleep 5`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lifecycle := newCommandLifecycle(ctx, cmd)
	lifecycle.publishStart(inactiveTerminationTree{})
	if err := lifecycle.terminate(true); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	lifecycle.wait()
	status := lifecycle.status()
	if status.TerminatedByCancellation {
		t.Fatalf("status = %+v, empty/inactive termination must not claim active child", status)
	}
	if status.TerminationProvable {
		t.Fatalf("status = %+v, a best-effort tree must not report a provable termination", status)
	}
}

type blockingTerminationTree struct {
	entered chan struct{}
	release chan struct{}
}

func (t *blockingTerminationTree) terminate(cmd *exec.Cmd) terminationResult {
	close(t.entered)
	<-t.release
	_ = cmd.Process.Kill()
	return terminationResult{active: true}
}
func (t *blockingTerminationTree) close() {}

func (*blockingTerminationTree) provesActiveTermination() bool { return true }

func TestLifecycleSerializesTerminationWithWait(t *testing.T) {
	ctx := context.Background()
	cmd := exec.Command(writeStub(t, `sleep 5`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree := &blockingTerminationTree{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-tree.release:
		default:
			close(tree.release)
		}
	}()
	lifecycle := newCommandLifecycle(ctx, cmd)
	lifecycle.publishStart(tree)

	waitDone := make(chan struct{})
	go func() {
		lifecycle.wait()
		close(waitDone)
	}()
	terminateDone := make(chan struct{})
	go func() {
		_ = lifecycle.terminate(true)
		close(terminateDone)
	}()
	<-tree.entered
	select {
	case <-waitDone:
		t.Fatal("wait completed before termination released the lifecycle")
	default:
	}
	close(tree.release)
	<-terminateDone
	<-waitDone
	status := lifecycle.status()
	if !status.TerminationProvable || !status.TerminatedByCancellation {
		t.Fatalf("status = %+v, want a provable active cancellation termination", status)
	}
}

func TestExecRunnerStatusTracksContextTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	_, _, err := (&ExecRunner{Binary: writeStub(t, `sleep 5`)}).Run(ctx, "exec")
	status, reported := RunStatusOf(err)
	if !reported || !status.Started || !status.Reaped {
		t.Fatalf("run status = %+v, reported=%t; want started/reaped lifecycle status", status, reported)
	}
	// The stub sleeps far past the cancellation, so a prompt return is itself
	// the evidence that cancellation terminated the child rather than letting
	// it run to completion.
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("run took %v, want a prompt return after cancellation", elapsed)
	}
	// The platform probe alone cannot decide this run: attaching the pidfd
	// fails under transient resource pressure, and the retained fallback
	// cannot prove the direct child was active, so assert the capability the
	// run actually published rather than the platform's.
	//
	// Only this direction is an invariant. The converse does not hold: even a
	// run that attached the identity withholds the claim when the stopped-state
	// barrier goes unobserved (processStoppedObservationWindow) or the child
	// exits before the cancellation lands. Both are deliberate conservative
	// outcomes, not proof that termination failed.
	if status.TerminatedByCancellation && !status.TerminationProvable {
		t.Fatalf("run status = %+v, conservative fallback must not claim active termination", status)
	}
	if !status.TerminationProvable && processGroupTerminationSupported() {
		t.Logf("run status = %+v: platform identity was not attached, conservative result expected", status)
	}
}

// TestExecRunnerStatusReportsBestEffortTreeAsUnprovable pins the path a
// failed platform-identity attach takes: the run keeps the best-effort tree,
// so its status must not report a provable termination even though the tree
// terminated the child.
func TestExecRunnerStatusReportsBestEffortTreeAsUnprovable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	hooks := runHooks{terminate: func(cmd *exec.Cmd) error { return terminateProcessTree(cmd) }}
	// exec keeps the sleeper as the direct child: the stub's shell would
	// otherwise outlive the direct kill and hold the output pipe open until
	// the sleep expired, which only adds wall-clock to the race job.
	_, _, err := (&ExecRunner{Binary: writeStub(t, `exec sleep 5`)}).run(ctx, hooks, "exec")
	status, reported := RunStatusOf(err)
	if !reported || !status.Started || !status.Reaped {
		t.Fatalf("run status = %+v, reported=%t; want started/reaped lifecycle status", status, reported)
	}
	if status.TerminationProvable {
		t.Fatalf("run status = %+v, a best-effort process tree must not be provable", status)
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
	if !errors.Is(err, orig) {
		t.Fatalf("error = %v, want original CLI failure", err)
	}
	var got *CLIError
	if !errors.As(err, &got) || got != orig {
		t.Fatalf("error = %v, want original CLIError %v", err, orig)
	}
	if !strings.Contains(err.Error(), "container system start") {
		t.Errorf("Error() = %q, want hint to run 'container system start'", err.Error())
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr != orig {
		t.Fatalf("error = %v, want original CLIError in chain", err)
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

func TestClassifyPreservesCLIAndDeliveryErrors(t *testing.T) {
	original := &CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "XPC connection error"}
	writerErr := errors.New("sink failed")
	joined := errors.Join(original, &OutputError{Stream: "stdout", Err: writerErr})
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}

	err := Classify(context.Background(), r, joined, appleProbe)
	if !errors.Is(err, ErrSystemNotRunning) || !errors.Is(err, ErrOutputDelivery) || !errors.Is(err, writerErr) {
		t.Fatalf("error = %v, want system, delivery, and writer errors", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want original CLI exit 7", err)
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

func TestClassifyPreservesNonLivenessOriginalAndProbeErrors(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "permission denied: image config rejected"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}

	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and probe errors", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatal("permission failure was classified as daemon down")
	}
	var gotCLI *CLIError
	if !errors.As(got, &gotCLI) || gotCLI != orig {
		t.Fatalf("errors.As(*CLIError) = %v, want original CLIError", gotCLI)
	}
}

func TestClassifyDoesNotCallAmbiguousMissingDaemonDown(t *testing.T) {
	orig := &CLIError{Args: []string{"run", "--name", "ctr"}, ExitCode: 1, Stderr: "container not found: ctr"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	// Use the default matcher: an explicit IsUnavailable predicate is
	// authoritative for non-definitive original text, but ambiguous
	// object-absence wording must still veto daemon-down when only the
	// conservative default matcher is available.
	probe := Probe{Args: appleProbe.Args, Hint: appleProbe.Hint}

	got := Classify(context.Background(), r, orig, probe)
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want both errors", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatal("ambiguous missing message was classified as daemon down")
	}
}

func TestClassifyUsesProbePredicateAndPreservesChains(t *testing.T) {
	orig := &CLIError{Args: []string{"inspect", "ctr"}, ExitCode: 1, Stderr: "command failed"}
	probeErr := &CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: "transport unavailable"}
	r := &fakeRunner{results: map[string]fakeResult{
		"version": {err: probeErr},
	}}
	probe := Probe{
		Args: []string{"version"}, Hint: "start the daemon",
		IsUnavailable: func(error) bool { return true },
	}

	got := Classify(context.Background(), r, orig, probe)
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want both errors", got)
	}
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, want sentinel from explicit probe predicate", got)
	}
}

func TestClassifyExplicitProbePredicateOverridesBroadOriginalConfigurationText(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "configuration changed while the command was running"}
	probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	probe := appleProbe
	probe.IsUnavailable = func(error) bool { return true }

	got := Classify(context.Background(), r, orig, probe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, want authoritative probe sentinel", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and probe errors", got)
	}
}

// CLIError.Error() embeds the argv the caller supplied, so a container or
// image named "tls" would otherwise veto ErrSystemNotRunning. The
// diagnostics that drive that decision must come from the stderr the CLI
// actually wrote.
func TestProbeClassificationIgnoresCallerSuppliedArgv(t *testing.T) {
	for _, name := range []string{"tls", "proxy", "certificate", "x509", "ssh", "config", "credential"} {
		t.Run(name, func(t *testing.T) {
			orig := &CLIError{
				Args: []string{"logs", "-n", "1000", name}, ExitCode: 1,
				Stderr: "XPC connection error: service is not registered",
			}
			if IsProbeConfigurationError(orig) {
				t.Fatalf("target %q was read as a configuration diagnostic", name)
			}
			probeErr := &CLIError{
				Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error",
			}
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}

			got := Classify(context.Background(), r, orig, appleProbe)
			if !errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, want sentinel for target %q", got, name)
			}
			if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
				t.Fatalf("classified error = %v, want original and probe chains", got)
			}
		})
	}
}

// Excluding argv must not exclude real transport diagnostics that reach the
// same predicates through stderr, joined branches, or captured probe output.
func TestProbeConfigurationDetectionSurvivesArgvExclusion(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "stderr",
			err: &CLIError{
				Args: []string{"version"}, ExitCode: 1,
				Stderr: "x509: certificate signed by unknown authority",
			},
		},
		{
			name: "joined branch",
			err: errors.Join(
				&CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: "connect failed"},
				errors.New("tls: failed to verify certificate"),
			),
		},
		{
			name: "wrapped non-CLI cause",
			err:  fmt.Errorf("probe instrumentation: %w", errors.New("proxyconnect tcp: connection refused")),
		},
		{
			name: "captured probe output",
			err: withProbeOutput(
				&CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: "connect failed"},
				nil, []byte("x509: certificate signed by unknown authority"),
			),
		},
		{
			name: "wrapped CLIError",
			err: fmt.Errorf("runner: %w", &CLIError{
				Args: []string{"version"}, ExitCode: 1,
				Stderr: "error during connect: invalid configuration for current context",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsProbeConfigurationError(tc.err) {
				t.Fatalf("IsProbeConfigurationError(%v) = false, want true", tc.err)
			}
			if !isNonLivenessError(tc.err) {
				t.Fatalf("isNonLivenessError(%v) = false, want true", tc.err)
			}
		})
	}
}

// Wrapper-authored text is library or instrumentation output, not caller
// argv, so it stays in the collected diagnostics.
func TestDiagnosticTextKeepsWrapperAndJoinedWording(t *testing.T) {
	cliErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "status failed"}
	text := diagnosticText(fmt.Errorf("runner: %w", errors.Join(
		fmt.Errorf("probe instrumentation: %w", errors.New("cannot connect to backend")),
		cliErr,
	)))
	if !strings.Contains(text, "cannot connect to backend") {
		t.Fatalf("diagnosticText() = %q, want joined liveness wording", text)
	}
	if strings.Contains(text, "system status") {
		t.Fatalf("diagnosticText() = %q, want CLIError argv excluded", text)
	}
}

func TestDefaultProbeUnavailableInspectsCompleteJoinedError(t *testing.T) {
	cliErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "status failed"}
	joined := errors.Join(
		fmt.Errorf("probe instrumentation: %w", errors.New("cannot connect to the container backend")),
		cliErr,
	)
	wrapped := fmt.Errorf("runner: %w", joined)

	if !defaultProbeUnavailable(wrapped) {
		t.Fatal("wrapped joined liveness evidence was not classified")
	}
	if defaultProbeUnavailable(errors.New("cannot connect to the container backend")) {
		t.Fatal("plain application error without CLI evidence was classified")
	}
}

func TestClassifyRejectsProbeConfigurationBeforeExplicitPredicate(t *testing.T) {
	diagnostics := []string{
		"error during connect: x509: certificate signed by unknown authority",
		"error during connect: tls: failed to verify certificate",
		"error during connect: proxyconnect tcp: connection refused",
		"error during connect: ssh: handshake failed",
		"error during connect: invalid configuration for current context",
		"error during connect: authentication required",
	}
	for _, diagnostic := range diagnostics {
		t.Run(diagnostic, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
			cliProbeErr := &CLIError{Args: []string{"version"}, ExitCode: 1, Stderr: diagnostic}
			probeErr := errors.Join(cliProbeErr, errors.New("runner attached context"))
			r := &fakeRunner{results: map[string]fakeResult{
				"version": {err: probeErr},
			}}
			probe := Probe{
				Args: []string{"version"}, Hint: "start the daemon",
				IsUnavailable: func(error) bool { return true },
			}

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, configuration failure marked daemon down", got)
			}
			if !errors.Is(got, orig) || !errors.Is(got, cliProbeErr) {
				t.Fatalf("classified error = %v, want original and probe chains", got)
			}
		})
	}
}

func TestClassifyJoinedProbeConfigurationVetoesJoinedLivenessText(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	cliProbeErr := &CLIError{
		Args: []string{"version"}, ExitCode: 1,
		Stderr: "cannot connect to the docker daemon",
	}
	probeErr := errors.Join(cliProbeErr, errors.New("x509: certificate signed by unknown authority"))
	r := &fakeRunner{results: map[string]fakeResult{
		"version": {err: probeErr},
	}}
	probe := Probe{
		Args: []string{"version"}, Hint: "start the daemon",
		IsUnavailable: func(error) bool { return true },
	}

	got := Classify(context.Background(), r, orig, probe)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, joined configuration branch was ignored", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, cliProbeErr) {
		t.Fatalf("classified error = %v, want original and probe chains", got)
	}
}

func TestClassifyExplicitProbePredicateDoesNotOverrideDefinitiveOriginalFailure(t *testing.T) {
	cases := []string{
		"permission denied while opening the backend endpoint",
		"invalid context: production does not exist",
		"operation canceled by the caller",
	}
	for _, stderr := range cases {
		t.Run(stderr, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: stderr}
			probeErr := &CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, definitive original failure marked daemon down", got)
			}
			if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
				t.Fatalf("classified error = %v, want original and probe errors", got)
			}
		})
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

type definiteErrorAtProbeDeadlineRunner struct {
	err *CLIError
}

func (r *definiteErrorAtProbeDeadlineRunner) Run(ctx context.Context, _ ...string) ([]byte, []byte, error) {
	<-ctx.Done()
	return nil, nil, r.err
}

func TestClassifyDoesNotTreatExpiredProbeContextAsReturnedTimeout(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	probeErr := &CLIError{
		Args: []string{"system", "status"}, ExitCode: 1,
		Stderr: "permission denied: configuration cannot be loaded",
	}
	start := time.Now()
	got := Classify(context.Background(), &definiteErrorAtProbeDeadlineRunner{err: probeErr}, orig, appleProbe)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Classify took %v, want finite probe timeout", elapsed)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, probe context state raced a definite failure", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and returned probe error", got)
	}
}

func TestClassifyReturnedTimeoutRequiresOnlyTimeoutOrCommandExitCauses(t *testing.T) {
	causes := []struct {
		name  string
		cause error
	}{
		{name: "permission", cause: os.ErrPermission},
		{name: "cancellation", cause: context.Canceled},
		{name: "configuration", cause: errors.New("x509: certificate signed by unknown authority")},
	}
	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
			probeErr := errors.Join(context.DeadlineExceeded, tc.cause)
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, joined non-timeout cause was ignored", got)
			}
			if !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, tc.cause) {
				t.Fatalf("classified error = %v, want timeout and joined cause", got)
			}
		})
	}

	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	probeErr := errors.Join(
		context.DeadlineExceeded,
		fmt.Errorf("wrapped probe timeout: %w", context.DeadlineExceeded),
	)
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, pure joined timeout should be liveness", got)
	}
}

func TestClassifyJoinedCLIErrorAndDeadlineUsesTimeoutLiveness(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
	probeErr := &CLIError{
		Args: []string{"system", "status"}, ExitCode: 1, Stderr: "probe command timed out",
	}
	returned := errors.Join(probeErr, context.DeadlineExceeded)
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: returned},
	}}

	got := Classify(context.Background(), r, orig, appleProbe)
	if !errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, want ErrSystemNotRunning from definitive timeout", got)
	}
	for _, cause := range []error{orig, probeErr, context.DeadlineExceeded} {
		if !errors.Is(got, cause) {
			t.Fatalf("classified error = %v, want chain %v", got, cause)
		}
	}
}

func TestClassifyJoinedCLIErrorAndDeadlineStillVetoesNonLiveness(t *testing.T) {
	diagnostics := []string{
		"permission denied while opening the endpoint",
		"invalid configuration for current context",
		"tls: failed to verify certificate",
		"context canceled",
	}
	for _, diagnostic := range diagnostics {
		t.Run(diagnostic, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "command failed"}
			probeErr := &CLIError{
				Args: []string{"system", "status"}, ExitCode: 1, Stderr: diagnostic,
			}
			returned := errors.Join(probeErr, context.DeadlineExceeded)
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: returned},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, non-liveness diagnostic marked daemon down", got)
			}
			if !errors.Is(got, orig) || !errors.Is(got, probeErr) || !errors.Is(got, context.DeadlineExceeded) {
				t.Fatalf("classified error = %v, want original, probe, and timeout chains", got)
			}
		})
	}
}

func TestClassifyTimeoutTextVetoesConfigurationAndPermission(t *testing.T) {
	diagnostics := []string{
		"failed to load config",
		"permission denied",
		"proxyconnect tcp: connection refused",
		"tls: failed to verify certificate",
	}
	for _, diagnostic := range diagnostics {
		t.Run(diagnostic, func(t *testing.T) {
			orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
			probeErr := fmt.Errorf("%s: %w", diagnostic, context.DeadlineExceeded)
			r := &fakeRunner{results: map[string]fakeResult{
				"system status": {err: probeErr},
			}}
			probe := appleProbe
			probe.IsUnavailable = func(error) bool { return true }

			got := Classify(context.Background(), r, orig, probe)
			if errors.Is(got, ErrSystemNotRunning) {
				t.Fatalf("classified error = %v, timeout text was ignored", got)
			}
			if !errors.Is(got, context.DeadlineExceeded) || !strings.Contains(got.Error(), diagnostic) {
				t.Fatalf("classified error = %v, want timeout and diagnostic", got)
			}
		})
	}
}

func TestClassifyPreservesCallerCancellationDuringPredicate(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	probeErr := &CLIError{
		Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error",
	}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probeErr},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	probe := appleProbe
	probe.IsUnavailable = func(error) bool {
		cancel()
		return true
	}

	got := Classify(ctx, r, orig, probe)
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("classified error = %v, cancellation during predicate was ignored", got)
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("classified error = %v, want caller cancellation", got)
	}
	if !errors.Is(got, orig) || !errors.Is(got, probeErr) {
		t.Fatalf("classified error = %v, want original and probe errors", got)
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want caller cancellation preserved", err)
	}
	select {
	case <-r.started:
		t.Fatal("probe ran for an already-canceled caller")
	default:
	}
}

type successfulCancelingProbeRunner struct {
	cancel context.CancelFunc
}

func (r *successfulCancelingProbeRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	r.cancel()
	return nil, nil, nil
}

func TestClassifyCallerCancellationAfterSuccessfulProbeWins(t *testing.T) {
	orig := &CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "boom"}
	ctx, cancel := context.WithCancel(context.Background())
	got := Classify(ctx, &successfulCancelingProbeRunner{cancel: cancel}, orig, appleProbe)
	if !errors.Is(got, orig) || !errors.Is(got, context.Canceled) {
		t.Fatalf("error = %v, want original and caller cancellation", got)
	}
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("error = %v, successful probe was relabeled as daemon down", got)
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
	if !errors.Is(err, orig) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want original and caller cancellation", err)
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
