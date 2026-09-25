package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// failRunRunner fails the run call but serves an owned inspect payload
// so cleanupFailedCreate can verify ownership.
type failRunRunner struct {
	*fakeRunner
	runErr      error
	inspectJSON string
	inspectErr  error
	deleteErr   error
	probeErr    error
	deleted     []string
}

func (r *failRunRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "system":
		if r.probeErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.probeErr
		}
		return r.fakeRunner.Run(ctx, args...)
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		if r.inspectErr != nil {
			return nil, nil, r.inspectErr
		}
		return []byte(r.inspectJSON), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		if r.deleteErr != nil {
			return nil, nil, r.deleteErr
		}
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func ownedInspectJSON(name string) string {
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.session": %q
      }
    },
    "status": {"state": "created", "networks": []}
  }
]`, name, name, sessionID())
}

func foreignInspectJSON(name string) string {
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {"com.github.hirokazumiyaji.container-go": "true"}
    },
    "status": {"state": "created", "networks": []}
  }
]`, name, name)
}

func TestRunFailureCleansUpOwnedContainer(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error for failed run")
	}
	if len(r.deleted) != 1 || r.deleted[0] != "myctr" {
		t.Fatalf("deleted = %v, want [myctr]", r.deleted)
	}
}

func TestRunFailurePreservesCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "daemon unavailable"}
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedInspectJSON("myctr"),
		deleteErr:   cleanupErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
	if !strings.Contains(err.Error(), "cleanup") || !strings.Contains(err.Error(), "myctr") {
		t.Fatalf("error = %v, want cleanup context and container name", err)
	}
}

func TestRunFailurePreservesBothCLIErrorsWhenBackendIsDown(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "daemon unavailable"}
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedInspectJSON("myctr"),
		deleteErr:   cleanupErr,
		probeErr:    &cli.CLIError{Args: []string{"system", "status"}, ExitCode: 1, Stderr: "backend down"},
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
}

func TestRunFailurePreservesCleanupInspectCLIError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "cleanup inspect failed"}
	r := &failRunRunner{
		fakeRunner: base,
		runErr:     runErr,
		inspectErr: cleanupErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if got := cliErrorWithStderr(err, runErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup inspect CLIError", err)
	}
}

func TestRunFailureCleanupNotFoundIsSuccess(t *testing.T) {
	cases := []struct {
		name       string
		inspectErr error
		deleteErr  error
	}{
		{
			name:       "inspect",
			inspectErr: &cli.CLIError{Args: []string{"inspect"}, ExitCode: 1, Stderr: `not found: "myctr"`},
		},
		{
			name:      "delete",
			deleteErr: &cli.CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: `not found: "myctr"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestRunner()
			base.imagePresent = true
			r := &failRunRunner{
				fakeRunner:  base,
				runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"},
				inspectJSON: ownedInspectJSON("myctr"),
				inspectErr:  tc.inspectErr,
				deleteErr:   tc.deleteErr,
			}

			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
			var cleanupErr *CleanupError
			if errors.As(err, &cleanupErr) {
				t.Fatalf("error = %v, want not-found cleanup to be successful", err)
			}
			if got := cliErrorWithStderr(err, "start failed"); got == nil {
				t.Fatalf("error = %v, want original run CLIError", err)
			}
		})
	}
}

func TestRunFailurePreservesNameConflict(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner: base,
		runErr: &cli.CLIError{Args: []string{"run"}, ExitCode: 1,
			Stderr: `Error: already exists: container "myctr"`},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want conflict error")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no cleanup on name conflict", r.deleted)
	}
}

func TestRunFailurePreservesForeignContainer(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "port bind failed"},
		inspectJSON: foreignInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no cleanup for foreign container", r.deleted)
	}
}

func TestRunFailureCleansUpAfterCancel(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = appleEngine{}
	cfg.name = "myctr"
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	if err := cleanupFailedCreate(ctx, cfg, runErr, runErr); err != nil {
		t.Fatalf("cleanupFailedCreate: %v", err)
	}
	if len(r.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup even after cancel", r.deleted)
	}
}

func TestReuseCreateFailureCleansUpOwned(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	// reuseCreate inspects first: report not-found once, then owned after failed run.
	calls := 0
	inner := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: &calls}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("error = %v, want run failure", err)
	}
	if len(inner.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup", inner.deleted)
	}
}

type rollbackErrorRunner struct {
	*fakeRunner
	copyErr   error
	deleteErr error
}

func (r *rollbackErrorRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "cp":
		if r.copyErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.copyErr
		}
	case "delete", "rm":
		if r.deleteErr != nil {
			r.mu.Lock()
			r.calls = append(r.calls, args)
			r.mu.Unlock()
			return nil, nil, r.deleteErr
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestRunCopyFailurePreservesCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	copyErr := &cli.CLIError{Args: []string{"cp"}, ExitCode: 1, Stderr: "copy failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "delete failed"}
	r := &rollbackErrorRunner{fakeRunner: base, copyErr: copyErr, deleteErr: cleanupErr}
	hostPath := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(hostPath, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithFiles(File{HostPath: hostPath, ContainerPath: "/tmp/input.txt"}))
	if got := cliErrorWithStderr(err, copyErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original copy CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
}

func TestRunWaitFailurePreservesCleanupCLIError(t *testing.T) {
	base := newTestRunner()
	waitErr := &cli.CLIError{Args: []string{"wait"}, ExitCode: 1, Stderr: "wait failed"}
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "delete failed"}
	r := &rollbackErrorRunner{fakeRunner: base, deleteErr: cleanupErr}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithWaitStrategy(&recordingStrategy{err: waitErr}))
	if got := cliErrorWithStderr(err, waitErr.Stderr); got == nil {
		t.Fatalf("error = %v, want original wait CLIError", err)
	}
	if got := cliErrorWithStderr(err, cleanupErr.Stderr); got == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
}

type reuseFailWrapper struct {
	*failRunRunner
	calls *int
}

func (w *reuseFailWrapper) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		*w.calls++
		if *w.calls == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
		}
	}
	return w.failRunRunner.Run(ctx, args...)
}
