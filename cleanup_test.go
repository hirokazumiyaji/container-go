package container

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestTerminateContainerIsNilSafe(t *testing.T) {
	if err := TerminateContainer(nil); err != nil {
		t.Fatalf("TerminateContainer(nil) = %v, want nil", err)
	}
}

func TestTerminateContainerDeletes(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	if err := TerminateContainer(ctr); err != nil {
		t.Fatalf("TerminateContainer: %v", err)
	}
	if f.callWith("delete") == nil {
		t.Error("delete not issued")
	}
}

func TestTerminateContainerHonorsKeepEnv(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	t.Setenv("CONTAINERGO_KEEP", "1")
	if err := TerminateContainer(ctr); err != nil {
		t.Fatalf("TerminateContainer: %v", err)
	}
	if f.callWith("delete") != nil {
		t.Error("delete issued despite CONTAINERGO_KEEP=1")
	}
}

type cleanupRecorder struct {
	cleanups []func()
	errors   []string
	logs     []string
}

func (r *cleanupRecorder) Helper() {}

func (r *cleanupRecorder) Cleanup(fn func()) {
	r.cleanups = append(r.cleanups, fn)
}

func (r *cleanupRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *cleanupRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *cleanupRecorder) run() {
	for _, fn := range r.cleanups {
		fn()
	}
}

func TestCleanupStrictReportsFailure(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	cleanupErr := &cli.CLIError{Args: []string{"delete", "myctr"}, ExitCode: 1, Stderr: "strict cleanup failed"}
	ctr.runner = &notFoundRunner{inner: f, err: cleanupErr}
	recorder := &cleanupRecorder{}

	registerCleanup(recorder, ctr, true)
	recorder.run()

	if len(recorder.errors) != 1 || !strings.Contains(recorder.errors[0], cleanupErr.Stderr) {
		t.Fatalf("errors = %v, want strict cleanup failure", recorder.errors)
	}
	if len(recorder.logs) != 0 {
		t.Fatalf("logs = %v, want strict cleanup reported as failure", recorder.logs)
	}
}

func TestCleanupRunsAtTestEnd(t *testing.T) {
	f := newTestRunner()
	t.Run("inner", func(t *testing.T) {
		ctr := runTestContainer(t, f)
		Cleanup(t, ctr)
		if f.callWith("delete") != nil {
			t.Error("delete ran before test end")
		}
	})
	if f.callWith("delete") == nil {
		t.Error("delete did not run at test end")
	}
}

func TestCleanupIsNilSafe(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		Cleanup(t, nil) // must not panic at cleanup time
	})
}

// lsRunner serves a canned `ls` listing and records deletes.
type lsRunner struct {
	*fakeRunner
	lsJSON string
}

func (l *lsRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ls" {
		l.calls = append(l.calls, args)
		return []byte(l.lsJSON), nil, nil
	}
	if args[0] == "inspect" {
		var containers []struct {
			ID     string `json:"id"`
			Config struct {
				Labels map[string]string `json:"labels"`
			} `json:"configuration"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(l.lsJSON), &containers); err != nil {
			return nil, nil, err
		}
		for _, container := range containers {
			if container.ID == args[len(args)-1] {
				data, err := json.Marshal([]any{container})
				if err != nil {
					return nil, nil, err
				}
				return data, nil, nil
			}
		}
	}
	return l.fakeRunner.Run(ctx, args...)
}

const pruneLsJSON = `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"aaaaaaaaaaaaaaaa"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-running","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"bbbbbbbbbbbbbbbb"}},"status":{"state":"running","networks":[]}},
  {"id":"unmanaged-stopped","configuration":{"labels":{}},"status":{"state":"stopped","networks":[]}}
]`

func TestPruneRemovesOnlyManagedStoppedContainers(t *testing.T) {
	f := &lsRunner{fakeRunner: newTestRunner(), lsJSON: pruneLsJSON}

	removed, err := pruneWith(context.Background(), f, appleEngine{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !slices.Equal(removed, []string{"managed-stopped"}) {
		t.Errorf("removed = %v, want [managed-stopped]", removed)
	}

	lsCall := f.callWith("ls")
	if !slices.Contains(lsCall, "--all") {
		t.Errorf("ls call missing --all: %v", lsCall)
	}
	var deleted []string
	for _, call := range f.calls {
		if call[0] == "delete" {
			deleted = append(deleted, call[len(call)-1])
		}
	}
	if !slices.Equal(deleted, []string{"managed-stopped"}) {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestSessionLabelValueIsValid(t *testing.T) {
	id := sessionID()
	if len(id) != 16 || strings.ToLower(id) != id {
		t.Errorf("sessionID = %q, want 16 lowercase hex chars", id)
	}
}
