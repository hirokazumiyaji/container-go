package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type issue84ReuseReviewRunner struct {
	*fakeRunner
	existing   bool
	created    bool
	postState  string
	generation string
	failCopy   bool
}

func issue84ReuseContainerJSON(id, creation, state string) []byte {
	return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine","descriptor":{"digest":"sha256:%s"}},"platform":{"os":"linux","architecture":"amd64"},"labels":{%q:"true",%q:%q,%q:"true",%q:%q}},"status":{"state":%q,"networks":[]}}]`, id, id, strings.Repeat("b", 64), managedLabel, sessionLabel, sessionID(), reuseLabel, creationLabel, creation, state))
}

func (r *issue84ReuseReviewRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, nil
	}
	switch args[0] {
	case "run":
		r.created = true
		return r.fakeRunner.Run(ctx, args...)
	case "inspect":
		id := args[len(args)-1]
		if !r.existing && !r.created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "` + id + `"`}
		}
		creation := r.generation
		if creation == "" {
			creation = r.creations["shared"]
		}
		if creation == "" {
			creation = r.creations[id]
		}
		if creation == "" {
			creation = r.lastCreation
		}
		if creation == "" {
			creation = "0123456789abcdef"
		}
		state := "running"
		if r.postState != "" && r.created {
			state = r.postState
		}
		return issue84ReuseContainerJSON(id, creation, state), nil, nil
	case "cp":
		if r.failCopy {
			return nil, nil, errors.New("injected copy failure")
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

type issue84TerminalExternalRunner struct {
	binary string
}

func (r *issue84TerminalExternalRunner) External() bool         { return true }
func (r *issue84TerminalExternalRunner) ExternalBinary() string { return r.binary }
func (r *issue84TerminalExternalRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return issue84AppleImageFixture(), nil, nil
	}
	if len(args) > 0 && args[0] == "run" {
		return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: `Error: already exists: container "terminal-reaper"`}
	}
	return nil, nil, nil
}

func TestIssue84TerminalNameConflictRetiresReaperEntry(t *testing.T) {
	binary := t.TempDir() + "/container"
	runner := &issue84TerminalExternalRunner{binary: binary}
	globalReapersMu.Lock()
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	_, err := Run(context.Background(), "team/demo:stable",
		WithName("terminal-reaper"), withRunner(runner), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("terminal name conflict unexpectedly succeeded")
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	entries := len(r.entries)
	r.mu.Unlock()
	if entries != 0 {
		t.Fatalf("terminal create retained %d reaper entries", entries)
	}
}

func TestIssue84DockerCleanupSkipsNameLocks(t *testing.T) {
	unlock, err := lockNameForBackend(context.Background(), dockerEngine{}, "shared")
	if err != nil {
		t.Fatalf("Docker name-lock gate: %v", err)
	}
	unlock()
}

func TestIssue84ReuseValidationFailureCleansCreatedGeneration(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue84ReuseReviewRunner{fakeRunner: base, postState: "created"}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithPublishedPort("127.0.0.1:16379:6379"),
		withRunner(runner), withEngine(appleEngine{}))
	if err == nil || ctr != nil || !strings.Contains(err.Error(), "published port") {
		t.Fatalf("Run = (%v, %v), want port validation failure", ctr, err)
	}
	if !runner.created {
		t.Fatal("test did not create a container")
	}
	foundDelete := false
	for _, call := range base.calls {
		if len(call) > 0 && call[0] == "delete" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatal("exact created generation was not cleaned up")
	}
}

func TestIssue84KeepReturnsVerifiedPartialReuseHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue84ReuseReviewRunner{fakeRunner: base, postState: "created"}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithPublishedPort("127.0.0.1:16379:6379"),
		withRunner(runner), withEngine(appleEngine{}))
	if err == nil || ctr == nil {
		t.Fatalf("Run = (%v, %v), want verified KEEP handle and validation error", ctr, err)
	}
	if !ctr.reused || ctr.creation == "" || ctr.uid != "" {
		t.Fatalf("partial handle = %+v, want verified Apple reuse identity", ctr)
	}
	for _, call := range base.calls {
		if len(call) > 0 && call[0] == "delete" {
			t.Fatalf("KEEP deleted the partial generation: %v", call)
		}
	}
}

func TestIssue84KeepReturnsSharedHandleOnCopyError(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue84ReuseReviewRunner{fakeRunner: base, existing: true, generation: "0123456789abcdef", failCopy: true}
	file := t.TempDir() + "/input"
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithFiles(File{HostPath: file, ContainerPath: "/tmp/input"}),
		withRunner(runner), withEngine(appleEngine{}))
	if err == nil || ctr == nil {
		t.Fatalf("Run = (%v, %v), want verified shared handle and copy error", ctr, err)
	}
	if !ctr.reused {
		t.Fatal("copy failure dropped the verified shared handle")
	}
}

func TestIssue84KeepReturnsSharedHandleOnWaitError(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue84ReuseReviewRunner{fakeRunner: base, existing: true, generation: "0123456789abcdef"}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), WithWaitStrategy(waitErrorStrategy{err: errors.New("injected wait failure")}),
		withRunner(runner), withEngine(appleEngine{}))
	if err == nil || ctr == nil {
		t.Fatalf("Run = (%v, %v), want verified shared handle and wait error", ctr, err)
	}
	if !ctr.reused {
		t.Fatal("wait failure dropped the verified shared handle")
	}
}
