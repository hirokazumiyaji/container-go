package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	deleted     []string
	creation    string
}

func (r *failRunRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = value
				}
			}
		}
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		if r.inspectErr != nil {
			return nil, nil, r.inspectErr
		}
		return []byte(strings.ReplaceAll(r.inspectJSON, "__CONTAINER_CREATION__", r.creation)), nil, nil
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
        "com.github.hirokazumiyaji.container-go.session": %q,
        "com.github.hirokazumiyaji.container-go.creation": "__CONTAINER_CREATION__"
      }
    },
    "status": {"state": "created", "networks": []}
  }
]`, name, name, sessionID())
}

func ownedReuseInspectJSON(name string) string {
	return fmt.Sprintf(`[
  {
    "id": %q,
    "configuration": {
      "id": %q,
      "image": {"reference": "redis:7-alpine"},
      "publishedPorts": [],
      "labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.session": %q,
        "com.github.hirokazumiyaji.container-go.reuse": "true",
        "com.github.hirokazumiyaji.container-go.creation": "__CONTAINER_CREATION__"
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

func unverifiableOwnedInspectJSON(name string) string {
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
func ownedDockerInspectJSON(name, id string) string {
	return fmt.Sprintf(`[
  {
    "Id": %q,
    "Name": %q,
    "State": {"Status": "created"},
    "Config": {
      "Image": "redis:7-alpine",
      "Labels": {
        "com.github.hirokazumiyaji.container-go": "true",
        "com.github.hirokazumiyaji.container-go.session": %q,
        "com.github.hirokazumiyaji.container-go.creation": "__CONTAINER_CREATION__"
      }
    }
  }
]`, id, "/"+name, sessionID())
}

func cliErrorWithStderr(err error, text string) *cli.CLIError {
	return findCLIErrorWithStderr(err, text)
}

func findCLIErrorWithStderr(err error, text string) *cli.CLIError {
	if err == nil {
		return nil
	}
	if cliErr, ok := err.(*cli.CLIError); ok {
		if strings.Contains(cliErr.Stderr, text) {
			return cliErr
		}
		return nil
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, u := range e.Unwrap() {
			if found := findCLIErrorWithStderr(u, text); found != nil {
				return found
			}
		}
	case interface{ Unwrap() error }:
		return findCLIErrorWithStderr(e.Unwrap(), text)
	}
	return nil
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

func TestDockerRunFailureCleansUpOwnedContainerWithVolumePolicy(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	id := strings.Repeat("ab", 32)
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectJSON: ownedDockerInspectJSON("myctr", id),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("want error for failed run")
	}
	if want := []string{"rm", "--force", "--volumes", id}; !slices.Equal(r.callWith("rm"), want) {
		t.Fatalf("rm = %v, want %v", r.callWith("rm"), want)
	}
}

func TestDockerRunFailurePreservesVolumeCleanupError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	id := strings.Repeat("ab", 32)
	runErr := &cli.CLIError{Binary: "docker", Args: []string{"run"}, ExitCode: 125, Stderr: "start failed"}
	cleanupErr := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"rm", "--force", "--volumes", id},
		ExitCode: 1,
		Stderr:   "error removing volume: volume driver plugin not found",
	}
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedDockerInspectJSON("myctr", id),
		deleteErr:   cleanupErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}))
	var wrapped *CleanupError
	if !errors.As(err, &wrapped) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if cliErrorWithStderr(err, runErr.Stderr) == nil {
		t.Fatalf("error = %v, want original run CLIError", err)
	}
	if cliErrorWithStderr(err, cleanupErr.Stderr) == nil {
		t.Fatalf("error = %v, want cleanup CLIError", err)
	}
	if !strings.Contains(err.Error(), "cleanup") || !strings.Contains(err.Error(), "myctr") {
		t.Fatalf("error = %v, want cleanup context", err)
	}
}

func TestRunFailurePreservesInspectCleanupError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Binary: "docker", Args: []string{"run"}, ExitCode: 125, Stderr: "create failed"}
	inspectErr := &cli.CLIError{
		Binary: "docker",
		Args:   []string{"inspect", "myctr"},
		Stderr: "volume driver plugin not found",
	}
	r := &failRunRunner{
		fakeRunner: base,
		runErr:     runErr,
		inspectErr: inspectErr,
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}))
	var wrapped *CleanupError
	if !errors.As(err, &wrapped) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if cliErrorWithStderr(err, runErr.Stderr) == nil || cliErrorWithStderr(err, inspectErr.Stderr) == nil {
		t.Fatalf("error = %v, want original run and inspect errors", err)
	}
}

type rollbackErrorRunner struct {
	*fakeRunner
	copyErr   error
	deleteErr error
	runID     string
}

func (r *rollbackErrorRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return []byte(r.runID + "\n"), nil, nil
	case "cp":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.copyErr
	case "rm", "delete":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, r.deleteErr
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestRunCopyFailurePreservesDockerCleanupError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	id := strings.Repeat("cd", 32)
	copyErr := &cli.CLIError{Binary: "docker", Args: []string{"cp"}, ExitCode: 1, Stderr: "copy failed"}
	cleanupErr := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"rm", "--force", "--volumes", id},
		ExitCode: 1,
		Stderr:   "volume driver unavailable",
	}
	r := &rollbackErrorRunner{fakeRunner: base, runID: id, copyErr: copyErr, deleteErr: cleanupErr}
	hostPath := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(hostPath, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}),
		WithFiles(File{HostPath: hostPath, ContainerPath: "/tmp/input.txt"}))
	var wrapped *CleanupError
	if !errors.As(err, &wrapped) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if cliErrorWithStderr(err, copyErr.Stderr) == nil || cliErrorWithStderr(err, cleanupErr.Stderr) == nil {
		t.Fatalf("error = %v, want both copy and cleanup errors", err)
	}
}

func TestRunWaitFailurePreservesDockerCleanupError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	id := strings.Repeat("ef", 32)
	waitErr := errors.New("not ready")
	cleanupErr := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"rm", "--force", "--volumes", id},
		ExitCode: 1,
		Stderr:   "volume driver unavailable",
	}
	r := &rollbackErrorRunner{fakeRunner: base, runID: id, deleteErr: cleanupErr}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}),
		WithWaitStrategy(&recordingStrategy{err: waitErr}))
	var wrapped *CleanupError
	if !errors.As(err, &wrapped) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if !strings.Contains(err.Error(), waitErr.Error()) || !strings.Contains(err.Error(), cleanupErr.Stderr) {
		t.Fatalf("error = %v, want wait and cleanup errors", err)
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

func TestRunFailurePreservesContainerWithMissingGeneration(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "port bind failed"},
		inspectJSON: unverifiableOwnedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want error")
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no cleanup without a verified generation", r.deleted)
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
	cfg.creation = "0123456789abcdef"
	r.creation = cfg.creation
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
		inspectJSON: ownedReuseInspectJSON("myctr"),
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

type reuseFailWrapper struct {
	*failRunRunner
	calls *int
}

func (w *reuseFailWrapper) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		*w.calls++
		if *w.calls == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `Error: container not found: "myctr"`}
		}
	}
	return w.failRunRunner.Run(ctx, args...)
}
