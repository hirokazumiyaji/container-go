package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cleanupErrRunner makes the failed-create cleanup fail in a chosen way, so
// the returned error can be inspected. sequence names the subcommand that
// fails; empty means every delete succeeds.
type cleanupErrRunner struct {
	*fakeRunner
	failDelete   bool
	failInspect  bool
	inspectErr   error
	deleteErr    error
	deletedCalls int
}

func (r *cleanupErrRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		// Record the creation label the way the shared fake does, so the
		// inspect below echoes a generation the ownership check accepts.
		recordCreation(r.fakeRunner, args)
		// A failed create: the container exists but never started.
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 125, Stderr: "start failed"}
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		inspectErr := r.inspectErr
		failInspect := r.failInspect
		r.mu.Unlock()
		if failInspect {
			if inspectErr != nil {
				return nil, nil, inspectErr
			}
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "inspect failed"}
		}
	case "rm", "delete":
		r.mu.Lock()
		r.deletedCalls++
		deleteErr := r.deleteErr
		failDelete := r.failDelete
		r.mu.Unlock()
		if failDelete {
			if deleteErr != nil {
				return nil, nil, deleteErr
			}
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "daemon down"}
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

// A cleanup failure must reach the caller. Before, the delete error was
// discarded and Run returned only the classified run error, so a leftover
// container was undetectable.
func TestReviewFailedCreateCleanupErrorReachesCaller(t *testing.T) {
	deleteErr := &cli.CLIError{Args: []string{"rm"}, ExitCode: 1, Stderr: "daemon down"}
	r := &cleanupErrRunner{fakeRunner: newTestRunner(), failDelete: true, deleteErr: deleteErr}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run returned nil")
	}

	// The original operation error is still recoverable.
	if !strings.Contains(err.Error(), "start failed") {
		t.Errorf("err = %v, want the original run error", err)
	}
	// The cleanup failure is recoverable as a *CleanupError.
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("err = %v (%T), want a *CleanupError", err, err)
	}
	if cleanupErr.Container != "myctr" {
		t.Errorf("Container = %q, want myctr", cleanupErr.Container)
	}
	// Both causes stay individually recoverable with errors.As.
	var runErr *cli.CLIError
	if !errors.As(err, &runErr) {
		t.Errorf("the original CLIError is no longer in the chain: %v", err)
	}
	var cleanupCLI *cli.CLIError
	if !errors.As(cleanupErr.Err, &cleanupCLI) {
		t.Errorf("the cleanup CLIError is no longer in the chain: %v", cleanupErr.Err)
	}
}

// An already-absent container is an idempotent success, not a failure: there
// is nothing left to clean up.
func TestReviewFailedCreateCleanupNotFoundIsSuccess(t *testing.T) {
	r := &cleanupErrRunner{
		fakeRunner:  newTestRunner(),
		failInspect: true,
		inspectErr:  &cli.CLIError{Args: []string{"inspect"}, ExitCode: 1, Stderr: "No such container: myctr"},
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run returned nil")
	}
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		t.Errorf("an absent container was reported as a cleanup failure: %v", err)
	}
}

// An inspect that succeeds but reports no container is proof of absence, so
// it must be an idempotent success. Reporting it as a leak is worse than the
// original silence: the caller is told about a container that is gone.
func TestReviewFailedCreateEmptyInspectOutputIsNotAReport(t *testing.T) {
	r := &emptyInspectRunner{fakeRunner: newTestRunner()}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run returned nil")
	}
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		t.Errorf("a provably absent container was reported as a leak: %v", err)
	}
	if r.deletedCalls != 0 {
		t.Errorf("deletedCalls = %d, want 0 for a container that is already gone", r.deletedCalls)
	}
}

// A container that cannot be inspected may still be running, so that is a
// failure rather than an assumption that it is gone.
func TestReviewFailedCreateUninspectableIsReported(t *testing.T) {
	r := &cleanupErrRunner{fakeRunner: newTestRunner(), failInspect: true}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("err = %v, want a *CleanupError for an uninspectable container", err)
	}
	if !strings.Contains(cleanupErr.Error(), "cleanup myctr: inspect") {
		t.Errorf("err = %v, want the inspect failure named", err)
	}
}

// A name conflict means a pre-existing container, not ours, so nothing is
// removed and nothing is reported.
func TestReviewFailedCreateNameConflictIsNotACleanupFailure(t *testing.T) {
	r := &conflictRunner{fakeRunner: newTestRunner()}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run returned nil")
	}
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		t.Errorf("a name conflict was reported as a cleanup failure: %v", err)
	}
	if r.deletedCalls != 0 {
		t.Errorf("deletedCalls = %d, want 0 for a pre-existing container", r.deletedCalls)
	}
}

// A reuse create that publishes a generation must not delete it on a later
// caller-local failure (file copy). Peers may already have adopted it; the
// original operational error is returned without a rollback CleanupError.
func TestReviewReusePublishedGenerationSurvivesCopyFailure(t *testing.T) {
	copyErr := &cli.CLIError{Args: []string{"cp"}, ExitCode: 1, Stderr: "reuse copy failed"}
	r := &reuseRollbackCleanupRunner{
		fakeRunner: newTestRunner(),
		copyErr:    copyErr,
		deleteErr:  &cli.CLIError{Args: []string{"rm"}, ExitCode: 1, Stderr: "daemon down"},
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("reuse-rollback"), WithReuse(), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: writeTempFile(t), ContainerPath: "/tmp/in.txt"}))
	if err == nil {
		t.Fatal("Run returned nil")
	}
	if !strings.Contains(err.Error(), "reuse copy failed") {
		t.Errorf("err = %v, want the original copy error", err)
	}
	var got *CleanupError
	if errors.As(err, &got) {
		t.Fatalf("err = %v, published reuse generation must not be rolled back", err)
	}
	if call := r.callWith("delete"); call != nil {
		t.Fatalf("delete ran after published reuse create: %v", call)
	}
}

// StrictCleanup must report a teardown failure as a test failure rather than
// logging it, so a leak cannot hide behind a green run.
func TestReviewStrictCleanupFailsOnTeardownError(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &cleanupErrRunner{fakeRunner: f, failDelete: true}

	rec := &fakeTB{TB: t}
	StrictCleanup(rec, ctr)
	rec.run()

	if !rec.errored {
		t.Error("StrictCleanup did not report the teardown failure")
	}
}

// Cleanup keeps its existing lenient behavior, so an unrelated backend
// problem does not turn an unrelated test red.
func TestReviewCleanupStaysLenient(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.runner = &cleanupErrRunner{fakeRunner: f, failDelete: true}

	rec := &fakeTB{TB: t}
	Cleanup(rec, ctr)
	rec.run()

	if rec.errored {
		t.Error("Cleanup must not fail the test; use StrictCleanup for that")
	}
	if rec.logs == 0 {
		t.Error("Cleanup must still log the failure")
	}
}

// fakeTB records whether a cleanup called Errorf and how often it logged.
type fakeTB struct {
	testing.TB
	errored bool
	logs    int
	fns     []func()
}

func (f *fakeTB) Errorf(format string, args ...any) { f.errored = true }
func (f *fakeTB) Logf(format string, args ...any)   { f.logs++ }
func (f *fakeTB) Helper()                           {}
func (f *fakeTB) Cleanup(fn func())                 { f.fns = append(f.fns, fn) }

// run executes the registered cleanups, as testing would at test end.
func (f *fakeTB) run() {
	for _, fn := range f.fns {
		fn()
	}
}

// conflictRunner fails `run` with a name conflict.
type conflictRunner struct {
	*fakeRunner
	deletedCalls int
}

func (r *conflictRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		return nil, nil, &cli.CLIError{
			Args:     args,
			ExitCode: 125,
			Stderr:   `container with id myctr already exists`,
		}
	case "rm", "delete":
		r.deletedCalls++
	}
	return r.fakeRunner.Run(ctx, args...)
}

// reuseRollbackCleanupRunner fails the reuse copy and the subsequent delete,
// and reports a container that carries the reuse labels the adoption path
// requires.
type reuseRollbackCleanupRunner struct {
	*fakeRunner
	copyErr   error
	deleteErr error
	inspectNo int
}

func (r *reuseRollbackCleanupRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspectNo++
		first := r.inspectNo == 1
		creation := r.creations[args[len(args)-1]]
		r.mu.Unlock()
		if first {
			// The adoption probe: not there yet, so a create follows.
			return nil, nil, &cli.CLIError{
				Args: args, ExitCode: 1, Stderr: "No such container: " + args[len(args)-1],
			}
		}
		name := args[len(args)-1]
		return []byte(`[{"id":` + quote(name) + `,"configuration":{"id":` + quote(name) +
			`,"image":{"reference":"redis:7-alpine"},"publishedPorts":[],"labels":{` +
			quote(managedLabel) + `:"true",` + quote(sessionLabel) + `:` + quote(sessionID()) + `,` +
			quote(creationLabel) + `:` + quote(creation) + `,` + quote(reuseLabel) + `:"true"}},` +
			`"status":{"state":"running","networks":[]}}]`), nil, nil
	case "cp":
		if r.copyErr != nil {
			return nil, nil, r.copyErr
		}
	case "rm", "delete":
		if r.deleteErr != nil {
			return nil, nil, r.deleteErr
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// emptyInspectRunner fails the create and then answers a successful inspect
// with no containers, which is authoritative proof of absence.
type emptyInspectRunner struct {
	*fakeRunner
	deletedCalls int
}

func (r *emptyInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		recordCreation(r.fakeRunner, args)
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 125, Stderr: "start failed"}
	case "inspect":
		// Exit 0 with an empty result: the daemon answered, and the answer
		// does not contain the container.
		return []byte("[]"), nil, nil
	case "rm", "delete":
		r.deletedCalls++
	}
	return r.fakeRunner.Run(ctx, args...)
}

// recordCreation captures the creation generation from a run invocation, as
// the shared fake does, so a later inspect reports a matching label.
func recordCreation(f *fakeRunner, args []string) {
	name := ""
	creation := ""
	for i, a := range args {
		switch {
		case a == "--name" && i+1 < len(args):
			name = args[i+1]
		case a == "--label" && i+1 < len(args):
			key, value, ok := strings.Cut(args[i+1], "=")
			if ok && key == creationLabel {
				creation = value
			}
		}
	}
	if name == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creations == nil {
		f.creations = map[string]string{}
	}
	f.creations[name] = creation
}

func writeTempFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(path, []byte("in"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
