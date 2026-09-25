package container

import (
	"context"
	"slices"
	"strings"
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
	lsJSON string
}

func (l *lsRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ls" {
		l.calls = append(l.calls, args)
		return []byte(l.lsJSON), nil, nil
	}
	return l.fakeRunner.Run(ctx, args...)
}

const pruneLsJSON = `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-running","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true"}},"status":{"state":"running","networks":[]}},
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

type dockerListRunner struct {
	*fakeRunner
	output string
}

func (d *dockerListRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		d.calls = append(d.calls, args)
		return []byte(d.output), nil, nil
	}
	return d.fakeRunner.Run(ctx, args...)
}

func TestDockerPruneAndReuseGroupUseVolumeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *dockerListRunner) ([]string, error)
	}{
		{
			name: "prune",
			run: func(ctx context.Context, r *dockerListRunner) ([]string, error) {
				return pruneWith(ctx, r, dockerEngine{})
			},
		},
		{
			name: "reuse group",
			run: func(ctx context.Context, r *dockerListRunner) ([]string, error) {
				return pruneReuseGroupWith(ctx, r, dockerEngine{}, "integration")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &dockerListRunner{fakeRunner: newTestRunner(), output: "managed-one\nmanaged-two\n"}
			removed, err := tc.run(context.Background(), r)
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if !slices.Equal(removed, []string{"managed-one", "managed-two"}) {
				t.Fatalf("removed = %v", removed)
			}

			var deletes [][]string
			for _, call := range r.calls {
				if call[0] == "rm" {
					deletes = append(deletes, call)
				}
			}
			if len(deletes) != 2 {
				t.Fatalf("rm calls = %v, want 2", deletes)
			}
			for _, call := range deletes {
				if len(call) < 3 || !slices.Equal(call[:3], []string{"rm", "--force", "--volumes"}) {
					t.Errorf("rm call = %v, want volume cleanup flag", call)
				}
			}
		})
	}
}

func TestSessionLabelValueIsValid(t *testing.T) {
	id := sessionID()
	if len(id) != 16 || strings.ToLower(id) != id {
		t.Errorf("sessionID = %q, want 16 lowercase hex chars", id)
	}
}
