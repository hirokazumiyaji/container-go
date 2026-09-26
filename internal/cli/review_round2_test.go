package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// The backends report failures on stdout as well as stderr, so a stream's
// terminal error must keep the stdout tail reachable through DiagnosticText.
func TestReviewStreamTerminalErrorKeepsStdoutDiagnostic(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'No such container: myctr\n'; exit 1`)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(stream)

	status, ok := stream.(interface{ TerminalError() error })
	if !ok {
		t.Fatalf("stream %T does not expose TerminalError", stream)
	}
	terminal := status.TerminalError()
	if terminal == nil {
		t.Fatal("expected a terminal error")
	}
	stdout, stderr, hasDiag := DiagnosticText(terminal)
	if !hasDiag {
		t.Fatal("no diagnostic attached to the terminal error")
	}
	if !strings.Contains(stdout+stderr, "No such container") {
		t.Errorf("stdout diagnostic missing from terminal error: stdout=%q stderr=%q", stdout, stderr)
	}
}

// A probe timeout must be a pure timeout: the context expired AND the error
// is that expiry. A genuine failure that merely arrived after the deadline
// is not liveness evidence.
// A liveness probe failure must stay out of the original error's whole-chain
// analysis, or a probe that failed for an unrelated reason (a permission or
// certificate problem) suppresses not-found detection for the original.
func TestReviewProbeDiagnosticStaysOutOfChainAnalysis(t *testing.T) {
	orig := &CLIError{Binary: "docker", Args: []string{"logs", "web"}, ExitCode: 1, Stderr: "No such container: web"}
	probeErr := &CLIError{
		Binary:   "docker",
		Args:     []string{"version"},
		ExitCode: 1,
		Stderr:   "permission denied while trying to connect to the Docker daemon socket",
	}
	r := &fakeRunner{results: map[string]fakeResult{
		"version": {err: probeErr},
	}}
	classified := Classify(context.Background(), r, orig, Probe{Args: []string{"version"}, Hint: "start the Docker daemon"})

	// The probe cause must stay reachable for diagnostics.
	if !strings.Contains(classified.Error(), "permission denied") {
		t.Errorf("probe cause lost from the message: %v", classified)
	}
	// But it must not be visible to a chain walk over the original error.
	var found *CLIError
	if errors.As(classified, &found) && found == probeErr {
		t.Error("probe CLIError is reachable through a chain walk")
	}
	// The original must remain the only CLIError a walk can see.
	if !errors.Is(classified, orig) {
		t.Errorf("original error lost: %v", classified)
	}
}

func TestReviewProbeTimeoutRequiresErrorIdentity(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if expired.Err() != context.DeadlineExceeded {
		t.Fatalf("test setup: got %v", expired.Err())
	}
	if isProbeTimeoutError(expired, errors.New("some other failure")) {
		t.Error("non-timeout error promoted to probe timeout")
	}
	if !isProbeTimeoutError(expired, context.DeadlineExceeded) {
		t.Error("genuine timeout not recognized")
	}
	if isProbeTimeoutError(context.Background(), context.DeadlineExceeded) {
		t.Error("Background context reported a probe timeout")
	}
	// Run joins the context error onto any exit status, so a real non-zero
	// probe exit is not timeout evidence.
	realExit := errors.Join(&CLIError{Binary: "docker", Args: []string{"info"}, ExitCode: 1}, context.DeadlineExceeded)
	if isProbeTimeoutError(expired, realExit) {
		t.Error("a real probe exit status was read as a timeout")
	}
	signalled := errors.Join(&CLIError{Binary: "docker", Args: []string{"info"}, ExitCode: -1}, context.DeadlineExceeded)
	if !isProbeTimeoutError(expired, signalled) {
		t.Error("a signalled child was not read as a timeout")
	}
}

// A child that exits on its own reports its own outcome. A cancellation
// observed afterwards must not rewrite a clean exit into an error, or a wait
// that actually succeeded reads as failed.
func TestReviewStreamCleanExitBeatsLateCancellation(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf out; exit 0`)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(stream)

	status, ok := stream.(interface{ TerminalError() error })
	if !ok {
		t.Fatalf("stream %T does not expose TerminalError", stream)
	}
	if terminal := status.TerminalError(); terminal != nil {
		t.Errorf("clean exit reported as %v", terminal)
	}
}
