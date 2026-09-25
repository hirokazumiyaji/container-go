package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// failRunRunner fails the run call but serves an owned inspect payload
// so cleanupFailedCreate can verify ownership.
type failRunRunner struct {
	*fakeRunner
	runErr          error
	inspectJSON     string
	inspectOwned    bool
	inspectReuse    bool
	inspectCreation string
	blockInspect    bool
	creation        string
	deleteErr       error
	deleted         []string
}

func (r *failRunRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		for _, arg := range args {
			if creation, ok := strings.CutPrefix(arg, creationLabel+"="); ok {
				r.creation = creation
			}
		}
		r.mu.Unlock()
		return nil, nil, r.runErr
	case "inspect":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		if r.blockInspect {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		if r.inspectOwned {
			creation := r.inspectCreation
			if creation == "" {
				creation = r.creation
			}
			return []byte(ownedInspectJSON(args[len(args)-1], creation, r.inspectReuse)), nil, nil
		}
		return []byte(r.inspectJSON), nil, nil
	case "delete", "rm":
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, r.deleteErr
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func ownedInspectJSON(name, creation string, reuse bool) string {
	reuseLabelJSON := ""
	if reuse {
		reuseLabelJSON = fmt.Sprintf(",%q: \"true\"", reuseLabel)
	}
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
        "com.github.hirokazumiyaji.container-go.creation": %q%s
      }
    },
    "status": {"state": "created", "networks": []}
  }
]`, name, name, sessionID(), creation, reuseLabelJSON)
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
		fakeRunner:   base,
		runErr:       &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectOwned: true,
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

func TestRunFailurePreservesReplacement(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:      base,
		runErr:          &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectOwned:    true,
		inspectCreation: "bbbbbbbbbbbbbbbb",
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil || strings.Contains(err.Error(), "cleanup ") {
		t.Fatalf("Run = %v, replacement should be a clean non-error", err)
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v, want no replacement delete", r.deleted)
	}
}

func TestRunFailureJoinsCleanupTimeout(t *testing.T) {
	oldTimeout := cleanupFailedCreateTimeout
	cleanupFailedCreateTimeout = 50 * time.Millisecond
	t.Cleanup(func() { cleanupFailedCreateTimeout = oldTimeout })

	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:   base,
		runErr:       &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		blockInspect: true,
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "cleanup myctr: inspect") {
		t.Fatalf("Run = %v, want joined cleanup timeout", err)
	}
	if len(r.deleted) != 0 {
		t.Fatalf("deleted = %v after cleanup timeout", r.deleted)
	}
}

func TestRunFailureJoinsCleanupDeleteError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:   base,
		runErr:       &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectOwned: true,
		deleteErr:    &cli.CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: "delete failed"},
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "entrypoint not found") ||
		!strings.Contains(err.Error(), "cleanup myctr: delete myctr") || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("Run = %v, want primary and cleanup errors", err)
	}
}

func TestCleanupFailedCreateRequiresGeneration(t *testing.T) {
	cfg := newConfig()
	cfg.name = "legacy"
	cfg.eng = appleEngine{}
	cfg.runner = newTestRunner()
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 1, Stderr: "failed"}
	if err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("cleanupFailedCreate = %v, want ErrGenerationReplaced", err)
	}
}

func TestRunFailureCleansUpAfterCancel(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	r := &failRunRunner{
		fakeRunner:   base,
		runErr:       &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectOwned: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := newConfig()
	cfg.runner = r
	cfg.eng = appleEngine{}
	cfg.name = "myctr"
	cfg.creation = "aaaaaaaaaaaaaaaa"
	r.creation = cfg.creation
	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	if err := cleanupFailedCreate(ctx, cfg, runErr, runErr); err != nil {
		t.Fatalf("cleanupFailedCreate: %v", err)
	}
	if len(r.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup even after cancel", r.deleted)
	}
}

type postCreateFailureRunner struct {
	*fakeRunner
	mu       sync.Mutex
	seenRun  bool
	creation string
}

func (r *postCreateFailureRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	seenRun := r.seenRun
	r.mu.Unlock()
	switch args[0] {
	case "inspect":
		if !seenRun {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "published-reuse"`}
		}
		r.mu.Lock()
		creation := r.creation
		r.mu.Unlock()
		return []byte(reuseInspectJSON(args[len(args)-1], "running", "redis:7-alpine", creation)), nil, nil
	case "run":
		r.mu.Lock()
		r.seenRun = true
		for _, arg := range args {
			if creation, ok := strings.CutPrefix(arg, creationLabel+"="); ok {
				r.creation = creation
			}
		}
		r.mu.Unlock()
		return []byte("published-reuse\n"), nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func TestReusePostCreateFileFailureDoesNotDeletePublishedGeneration(t *testing.T) {
	f := newReuseCreateRunner()
	f.failPrefix = "cp"
	r := &postCreateFailureRunner{fakeRunner: f.fakeRunner}
	host := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(host, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("published-reuse"), WithReuse(),
		WithFiles(File{HostPath: host, ContainerPath: "/input"}),
		withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "cp") {
		t.Fatalf("Run error = %v, want post-create copy failure", err)
	}
	if del := f.callWith("delete"); del != nil {
		t.Fatalf("post-create failure deleted published generation: %v", del)
	}
}

func TestReuseCreateFailureCleansUpOwned(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	// reuseCreate inspects first: report not-found once, then owned after failed run.
	calls := 0
	inner := &failRunRunner{
		fakeRunner:   base,
		runErr:       &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectOwned: true,
		inspectReuse: true,
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

func TestReuseCreateFailureJoinsCleanupError(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	calls := 0
	inner := &failRunRunner{
		fakeRunner:   base,
		runErr:       &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
		inspectOwned: true,
		inspectReuse: true,
		deleteErr:    &cli.CLIError{Args: []string{"delete"}, ExitCode: 1, Stderr: "delete failed"},
	}
	wrapper := &reuseFailWrapper{failRunRunner: inner, calls: &calls}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(wrapper), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "entrypoint not found") ||
		!strings.Contains(err.Error(), "cleanup myctr: delete myctr") {
		t.Fatalf("Run = %v, want joined reuse create and cleanup errors", err)
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
