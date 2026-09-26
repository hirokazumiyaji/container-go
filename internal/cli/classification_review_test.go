package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type reviewProbeRunner struct {
	results map[string]fakeResult
	calls   int
}

func (r *reviewProbeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.calls++
	res, ok := r.results[strings.Join(args, " ")]
	if !ok {
		return nil, nil, errors.New("unexpected probe")
	}
	return []byte(res.stdout), nil, res.err
}

func TestReviewExecRunnerRetainsStdoutDiagnosticsWithoutChangingCLIError(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf diagnostic; printf problem >&2; exit 9`)}
	_, _, err := r.Run(context.Background(), "inspect", "missing")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want CLIError", err)
	}
	stdout, stderr, ok := DiagnosticText(err)
	if !ok || stdout != "diagnostic" || stderr != "problem" {
		t.Fatalf("diagnostic text = %q/%q/%v", stdout, stderr, ok)
	}
}

func TestReviewClassifyUsesProbeStdoutForLivenessEvidence(t *testing.T) {
	original := &CLIError{Binary: "container", Args: []string{"run"}, ExitCode: 1, Stderr: "operation failed"}
	probe := &CLIError{Binary: "container", Args: []string{"system", "status"}, ExitCode: 1}
	runner := &reviewProbeRunner{results: map[string]fakeResult{"system status": {stdout: "XPC connection error", err: probe}}}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"system", "status"}, Hint: "start"})
	if !errors.Is(got, ErrSystemNotRunning) || !errors.Is(got, probe) {
		t.Fatalf("classification = %v, want liveness sentinel and probe error", got)
	}
}

func TestReviewClassifyPreservesGenericProbeFailure(t *testing.T) {
	original := &CLIError{Binary: "container", Args: []string{"run"}, ExitCode: 1, Stderr: "operation failed"}
	probe := &CLIError{Binary: "container", Args: []string{"system", "status"}, ExitCode: 1, Stderr: "unexpected status failure"}
	runner := &reviewProbeRunner{results: map[string]fakeResult{"system status": {err: probe}}}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"system", "status"}, Hint: "start"})
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("generic probe failure was classified as daemon down: %v", got)
	}
	if !errors.Is(got, original) || !errors.Is(got, probe) {
		t.Fatalf("classification lost an error branch: %v", got)
	}
}

func TestReviewClassifyVetoesOriginalPermissionFailure(t *testing.T) {
	original := &CLIError{Binary: "container", Args: []string{"run"}, ExitCode: 1, Stderr: "permission denied"}
	probe := &CLIError{Binary: "container", Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	runner := &reviewProbeRunner{results: map[string]fakeResult{"system status": {err: probe}}}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"system", "status"}, Hint: "start"})
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("permission failure was classified as daemon down: %v", got)
	}
}

func TestReviewClassifyVetoesAuthenticationProbeFailure(t *testing.T) {
	original := &CLIError{Binary: "docker", Args: []string{"run"}, ExitCode: 1, Stderr: "cannot connect"}
	probe := &CLIError{Binary: "docker", Args: []string{"version"}, ExitCode: 1, Stderr: "authentication required"}
	runner := &reviewProbeRunner{results: map[string]fakeResult{"version": {err: probe}}}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"version"}})
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("authentication failure was classified as daemon down: %v", got)
	}
}

func TestReviewClassifyVetoesProbePermissionFailure(t *testing.T) {
	original := &CLIError{Binary: "container", Args: []string{"run"}, ExitCode: 1, Stderr: "XPC connection error"}
	probe := &CLIError{Binary: "container", Args: []string{"system", "status"}, ExitCode: 1, Stderr: "permission denied"}
	runner := &reviewProbeRunner{results: map[string]fakeResult{"system status": {err: probe}}}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"system", "status"}, Hint: "start"})
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("probe permission failure was classified as daemon down: %v", got)
	}
}

func TestReviewClassifyVetoesAmbiguousApplicationNotFound(t *testing.T) {
	original := &CLIError{Binary: "container", Args: []string{"run", "--name", "myctr"}, ExitCode: 1, Stderr: "application dependency not found"}
	probe := &CLIError{Binary: "container", Args: []string{"system", "status"}, ExitCode: 1, Stderr: "XPC connection error"}
	runner := &reviewProbeRunner{results: map[string]fakeResult{"system status": {err: probe}}}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"system", "status"}, Hint: "start"})
	if errors.Is(got, ErrSystemNotRunning) {
		t.Fatalf("ambiguous application not-found was classified as daemon down: %v", got)
	}
	if !errors.Is(got, original) || !errors.Is(got, probe) {
		t.Fatalf("classification lost an error branch: %v", got)
	}
}

func TestReviewClassifyDoesNotProbeOperationTimeout(t *testing.T) {
	commandErr := &CLIError{Binary: "container", Args: []string{"exec"}, ExitCode: 7, Stderr: "application timeout"}
	original := errors.Join(commandErr, context.DeadlineExceeded)
	runner := &reviewProbeRunner{}
	got := Classify(context.Background(), runner, original, Probe{Args: []string{"system", "status"}})
	if runner.calls != 0 || !errors.Is(got, commandErr) || !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("timeout classification probed or replaced error: calls=%d err=%v", runner.calls, got)
	}
}
