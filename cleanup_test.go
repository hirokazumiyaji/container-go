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

const (
	cleanupDockerIDOne = "1111111111111111111111111111111111111111111111111111111111111111"
	cleanupDockerIDTwo = "2222222222222222222222222222222222222222222222222222222222222222"
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
	if args[0] == "inspect" && l.lsJSON != "" {
		l.calls = append(l.calls, args)
		return []byte(l.lsJSON), nil, nil
	}
	return l.fakeRunner.Run(ctx, args...)
}

const pruneLsJSON = `[
  {"id":"managed-stopped","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"stopped","networks":[]}},
  {"id":"managed-running","configuration":{"labels":{"com.github.hirokazumiyaji.container-go":"true","com.github.hirokazumiyaji.container-go.creation":"0123456789abcdef"}},"status":{"state":"running","networks":[]}},
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
	output    string
	deleteErr error
}

func (d *dockerListRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "ps" {
		d.calls = append(d.calls, args)
		return []byte(d.output), nil, nil
	}
	if args[0] == "inspect" {
		d.calls = append(d.calls, args)
		id := args[len(args)-1]
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:"integration"}},"NetworkSettings":{}}]`, id, managedLabel, reuseLabel, creationLabel, reuseGroupLabel)), nil, nil
	}
	if args[0] == "rm" && d.deleteErr != nil {
		d.calls = append(d.calls, args)
		return nil, nil, d.deleteErr
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
			r := &dockerListRunner{fakeRunner: newTestRunner(), output: cleanupDockerIDOne + "\n" + cleanupDockerIDTwo + "\n"}
			removed, err := tc.run(context.Background(), r)
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if !slices.Equal(removed, []string{cleanupDockerIDOne, cleanupDockerIDTwo}) {
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
				want := []string{"rm", "--force", "--volumes", call[len(call)-1]}
				if !slices.Equal(call, want) {
					t.Errorf("rm call = %v, want %v", call, want)
				}
			}
		})
	}
}

func TestDockerPruneReportsVolumeDeleteFailure(t *testing.T) {
	r := &dockerListRunner{
		fakeRunner: newTestRunner(),
		output:     cleanupDockerIDOne + "\n",
		deleteErr: &cli.CLIError{
			Binary: "docker",
			Args:   []string{"rm", "--force", "--volumes", cleanupDockerIDOne},
			Stderr: "error removing volume: volume driver plugin not found",
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err == nil {
		t.Fatal("prune succeeded, want volume deletion failure")
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want no falsely successful deletions", removed)
	}
	if !strings.Contains(err.Error(), "prune "+cleanupDockerIDOne) {
		t.Fatalf("error = %v, want prune target context", err)
	}
}

func TestDockerPruneAcceptsPreciseContainerNotFound(t *testing.T) {
	id := cleanupDockerIDOne
	r := &dockerListRunner{
		fakeRunner: newTestRunner(),
		output:     id + "\n",
		deleteErr: &cli.CLIError{
			Binary: "docker",
			Args:   []string{"rm", "--force", "--volumes", id},
			Stderr: "Error response from daemon: No such container: " + id,
		},
	}
	removed, err := pruneWith(context.Background(), r, dockerEngine{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !slices.Equal(removed, []string{id}) {
		t.Fatalf("removed = %v, want [%s]", removed, id)
	}
}

func TestDockerPruneSelectsExitedAndDeadOnly(t *testing.T) {
	args := (dockerEngine{}).listArgs()
	for _, want := range []string{"status=exited", "status=dead"} {
		if !slices.Contains(args, want) {
			t.Errorf("listArgs = %v, missing %q", args, want)
		}
	}
	for _, unwanted := range []string{"status=created", "status=running"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("listArgs = %v, unexpectedly includes %q", args, unwanted)
		}
	}
}

type cleanupTBRecorder struct {
	testing.TB
	callbacks []func()
	logs      []string
	errors    []string
	helpers   int
}

func (r *cleanupTBRecorder) Helper() { r.helpers++ }

func (r *cleanupTBRecorder) Cleanup(fn func()) {
	r.callbacks = append(r.callbacks, fn)
}

func (r *cleanupTBRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *cleanupTBRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestCleanupFunctionsMarkThemselvesAsHelpers(t *testing.T) {
	tb := &cleanupTBRecorder{}
	Cleanup(tb, nil)
	if tb.helpers < 2 {
		t.Fatalf("Cleanup helper calls = %d, want exported and registration helpers", tb.helpers)
	}

	tb = &cleanupTBRecorder{}
	StrictCleanup(tb, nil)
	if tb.helpers < 2 {
		t.Fatalf("StrictCleanup helper calls = %d, want exported and registration helpers", tb.helpers)
	}
}

func TestStrictCleanupReportsCleanupFailure(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.failPrefix = "delete"
	tb := &cleanupTBRecorder{}
	registerCleanup(tb, ctr, true)
	if len(tb.callbacks) != 1 {
		t.Fatalf("callbacks = %d, want 1", len(tb.callbacks))
	}
	tb.callbacks[0]()
	if len(tb.errors) != 1 {
		t.Fatalf("errors = %v, want cleanup failure", tb.errors)
	}
	if len(tb.logs) != 0 {
		t.Fatalf("logs = %v, strict cleanup should report through Errorf", tb.logs)
	}
}

func TestSessionLabelValueIsValid(t *testing.T) {
	id := sessionID()
	if len(id) != 16 || strings.ToLower(id) != id {
		t.Errorf("sessionID = %q, want 16 lowercase hex chars", id)
	}
}

func TestRunWaitFailureHonorsKeepEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "default rolls back", keep: "", wantDelete: true},
		{name: "keep skips rollback", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				withRunner(f), withEngine(appleEngine{}),
				WithWaitStrategy(&recordingStrategy{err: errors.New("never ready")}))
			if err == nil {
				t.Fatal("Run succeeded; want readiness wait error")
			}
			didDelete := f.callWith("delete") != nil
			if didDelete != tc.wantDelete {
				t.Fatalf("delete issued = %v, want %v", didDelete, tc.wantDelete)
			}
		})
	}
}

func TestRunCopyFailureHonorsKeepEnv(t *testing.T) {
	src := filepath.Join(t.TempDir(), "testfile")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "default rolls back", keep: "", wantDelete: true},
		{name: "keep skips rollback", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
			f := newTestRunner()
			f.failPrefix = "cp"
			_, err := Run(context.Background(), "redis:7-alpine",
				withRunner(f), withEngine(appleEngine{}),
				WithFiles(File{HostPath: src, ContainerPath: "/testfile"}))
			if err == nil {
				t.Fatal("Run succeeded; want copy error")
			}
			didDelete := f.callWith("delete") != nil
			if didDelete != tc.wantDelete {
				t.Fatalf("delete issued = %v, want %v", didDelete, tc.wantDelete)
			}
		})
	}
}

func TestRunFailedCreateHonorsKeepEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keep       string
		wantDelete bool
	}{
		{name: "default cleans up", keep: "", wantDelete: true},
		{name: "keep skips cleanup", keep: "1", wantDelete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", tc.keep)
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
				t.Fatal("Run succeeded; want run error")
			}
			didDelete := len(r.deleted) > 0
			if didDelete != tc.wantDelete {
				t.Fatalf("delete issued = %v, want %v", didDelete, tc.wantDelete)
			}
		})
	}
}

func TestExplicitTerminateIgnoresKeepEnv(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if f.callWith("delete") == nil {
		t.Fatal("explicit Terminate did not issue delete when CONTAINERGO_KEEP=1")
	}
}
