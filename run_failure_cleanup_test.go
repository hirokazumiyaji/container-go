package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	creation    string
	deleted     []string
}

func (r *failRunRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if creation, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = creation
				}
			}
		}
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		data := r.inspectJSON
		if r.creation != "" {
			data = strings.ReplaceAll(data, "__CONTAINERGO_CREATION__", r.creation)
		}
		r.mu.Unlock()
		return []byte(data), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
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
        "com.github.hirokazumiyaji.container-go.creation": "__CONTAINERGO_CREATION__"
      }
    },
    "status": {"state": "created", "networks": []}
  }
]`, name, name, sessionID())
}

func ownedWithoutGenerationJSON(name string) string {
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
		creation:    "aaaaaaaaaaaaaaaa",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = appleEngine{}
	cfg.name = "myctr"
	cfg.creation = "aaaaaaaaaaaaaaaa"
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	if err := cleanupFailedCreate(ctx, cfg, runErr, runErr); err != nil {
		t.Fatalf("cleanupFailedCreate: %v", err)
	}
	if len(r.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup even after cancel", r.deleted)
	}
}

func TestRollbackPreservesCleanupError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Apple name locks are unavailable on Windows")
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	cause := errors.New("copy failed")
	ctr := &Container{
		id: "myctr", runner: &generationRunner{}, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa",
	}
	err := ctr.rollback(context.Background(), cause)
	if !errors.Is(err, cause) || !errors.Is(err, ErrNameLockCompatibility) {
		t.Fatalf("rollback error = %v, want primary and cleanup errors", err)
	}
}

func TestCleanupFailedCreateSkipsMissingGeneration(t *testing.T) {
	r := &failRunRunner{
		fakeRunner:  newTestRunner(),
		inspectJSON: ownedWithoutGenerationJSON("myctr"),
	}
	cfg := &config{
		name:     "myctr",
		creation: "aaaaaaaaaaaaaaaa",
		runner:   r,
		eng:      appleEngine{},
	}
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	if err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr); err != nil {
		t.Fatalf("cleanupFailedCreate: %v", err)
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no delete without a generation", r.deleted)
	}
}

func TestRunSurfacesCreateLockFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Apple name locks are unavailable on Windows")
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	base := newTestRunner()
	base.imagePresent = true
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	r := &failRunRunner{
		fakeRunner:  base,
		runErr:      runErr,
		inspectJSON: ownedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !errors.Is(err, ErrNameLockCompatibility) ||
		!strings.Contains(err.Error(), "create myctr: lock name") ||
		!strings.Contains(err.Error(), "transitional user cache directory") {
		t.Fatalf("Run error = %v, want surfaced create lock failure", err)
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no delete without compatibility lock", r.deleted)
	}
}

func TestCleanupFailedCreateDockerSkipsNameLock(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "not-a-state-directory")
	if err := os.WriteFile(stateFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateFile)
	const id = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	inspectJSON := fmt.Sprintf(`[{
		"Id": %q,
		"Name": "/myctr",
		"Config": {"Labels": {
			%q: "true",
			%q: %q,
			%q: "aaaaaaaaaaaaaaaa"
		}},
		"State": {"Status": "exited"},
		"NetworkSettings": {}
	}]`, id, managedLabel, sessionLabel, sessionID(), creationLabel)
	r := &failRunRunner{
		fakeRunner:  newTestRunner(),
		inspectJSON: inspectJSON,
		runErr:      fmt.Errorf("create failed"),
	}
	cfg := &config{
		name:     "myctr",
		creation: "aaaaaaaaaaaaaaaa",
		runner:   r,
		eng:      dockerEngine{},
	}
	if err := cleanupFailedCreate(context.Background(), cfg, r.runErr, r.runErr); err != nil {
		t.Fatalf("cleanupFailedCreate: %v", err)
	}
	if len(r.deleted) != 1 || r.deleted[0] != id {
		t.Fatalf("deleted = %v, want immutable ID [%s]", r.deleted, id)
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
