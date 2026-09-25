package container

import (
	"context"
	"fmt"
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
		creation := r.creation
		r.mu.Unlock()
		return []byte(strings.ReplaceAll(r.inspectJSON, "__CREATION__", creation)), nil, nil
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
        "com.github.hirokazumiyaji.container-go.reuse": "true",
        "com.github.hirokazumiyaji.container-go.creation": "__CREATION__"
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
	cleanupFailedCreate(ctx, cfg, runErr, runErr)
	if len(r.deleted) != 1 {
		t.Fatalf("deleted = %v, want cleanup even after cancel", r.deleted)
	}
}

func TestReuseCreateFailureRetainsAmbiguousGeneration(t *testing.T) {
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
	if len(inner.deleted) != 0 {
		t.Fatalf("deleted = %v, reused ambiguous generation must be retained", inner.deleted)
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
