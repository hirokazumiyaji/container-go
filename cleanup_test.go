package container

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
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
	lsJSON      string
	inspectJSON func(string) []byte
}

func (l *lsRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ls" {
		l.calls = append(l.calls, args)
		return []byte(l.lsJSON), nil, nil
	}
	if args[0] == "inspect" && l.inspectJSON != nil {
		return l.inspectJSON(args[len(args)-1]), nil, nil
	}
	return l.fakeRunner.Run(ctx, args...)
}

const pruneLsJSON = `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-running","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"running","networks":[]}},
  {"id":"unmanaged-stopped","configuration":{"labels":{}},"status":{"state":"stopped","networks":[]}}
]`

func stoppedPruneInspect(id string) []byte {
	return []byte(strings.Replace(
		reuseInspectJSONWithCreation(id, "running", "redis:7-alpine", "0123456789abcdef"),
		`"state": "running"`, `"state": "stopped"`, 1))
}

func TestPruneRemovesOnlyManagedStoppedContainers(t *testing.T) {
	f := &lsRunner{
		fakeRunner:  newTestRunner(),
		lsJSON:      pruneLsJSON,
		inspectJSON: stoppedPruneInspect,
	}

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

type pruneRaceRunner struct {
	mu           sync.Mutex
	id           string
	inspectCalls int
	deleted      []string
}

func (r *pruneRaceRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ls":
		return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"aaaaaaaaaaaaaaaa"}},"status":{"state":"stopped","networks":[]}}]`, r.id)), nil, nil
	case "inspect":
		r.mu.Lock()
		r.inspectCalls++
		n := r.inspectCalls
		r.mu.Unlock()
		_ = n
		return []byte(strings.Replace(reuseInspectJSONWithCreation(r.id, "running", "redis:7-alpine", "bbbbbbbbbbbbbbbb"), `"state": "running"`, `"state": "stopped"`, 1)), nil, nil
	case "delete":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.mu.Unlock()
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestPruneRevalidatesAppleCandidateUnderStableLock(t *testing.T) {
	r := &pruneRaceRunner{id: "prune-race-" + newContainerName()}
	removed, err := pruneWith(context.Background(), r, appleEngine{})
	if err != nil {
		t.Fatalf("pruneWith: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want stale candidate skipped", removed)
	}
	r.mu.Lock()
	deleted := append([]string(nil), r.deleted...)
	r.mu.Unlock()
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want no delete after generation replacement", deleted)
	}
}

func TestSessionLabelValueIsValid(t *testing.T) {
	id := sessionID()
	if len(id) != 16 || strings.ToLower(id) != id {
		t.Errorf("sessionID = %q, want 16 lowercase hex chars", id)
	}
}
