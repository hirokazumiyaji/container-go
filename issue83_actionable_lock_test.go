//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func configureUnavailableOptionalNameLocks(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldOverride := nameLockStateRootOverride
	nameLockStateRootOverride = notDirectory
	t.Cleanup(func() { nameLockStateRootOverride = oldOverride })
}

func TestNameLockFailsClosedWhenTransitionalNamespaceUnavailable(t *testing.T) {
	configureUnavailableOptionalNameLocks(t)
	_, err := lockName(context.Background(), "missing-transitional-"+newContainerName())
	if !errors.Is(err, ErrNameLockCompatibility) || !strings.Contains(err.Error(), "transitional") {
		t.Fatalf("lockName error = %v, fail-closed transitional compatibility error", err)
	}
}

func TestReaperFailsClosedWhenTransitionalNamespaceUnavailable(t *testing.T) {
	configureUnavailableOptionalNameLocks(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	err := r.register("missing-transitional-reaper", "0123456789abcdef")
	if err == nil || !strings.Contains(err.Error(), "transitional") {
		t.Fatalf("register error = %v, fail-closed transitional compatibility error", err)
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("reaper issued backend calls without every lock barrier: %s", data)
	}
}

func TestFollowLogsKeepsWriterBarrierWhileStreamIsOpen(t *testing.T) {
	runner := &followLockRunner{
		fakeRunner: newTestRunner(),
		stream:     &blockingFollowStream{done: make(chan struct{})},
	}
	ctr := runTestContainer(t, runner)
	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	defer stream.Close()

	readCtx, readCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	readerUnlock, err := lockNameShared(readCtx, "myctr")
	readCancel()
	if err != nil {
		t.Fatalf("shared reader was blocked by log stream: %v", err)
	}
	readerUnlock()

	writeCtx, writeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = lockName(writeCtx, "myctr")
	writeCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement writer was not excluded by log stream: %v", err)
	}
}

type blockingAppleCopyRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingAppleCopyRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "cp" {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *blockingAppleCopyRunner) unblock() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

func TestAppleCopyUsesSharedGenerationPin(t *testing.T) {
	runner := &blockingAppleCopyRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	t.Cleanup(runner.unblock)
	ctr := runTestContainer(t, runner)

	copyDone := make(chan error, 1)
	go func() {
		copyDone <- ctr.CopyToContainer(context.Background(), writeFreshReviewFile(t), "/tmp/input")
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("copy did not reach the backend")
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := ctr.State(readCtx)
	readCancel()
	if err != nil {
		t.Fatalf("shared reader blocked by copy: %v", err)
	}
	writerCtx, writerCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, writerErr := lockName(writerCtx, "myctr")
	writerCancel()
	if !errors.Is(writerErr, context.DeadlineExceeded) {
		t.Fatalf("writer was not excluded by copy: %v", writerErr)
	}

	runner.unblock()
	if err := <-copyDone; err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
}

type blockingPostCreateRunner struct {
	*fakeRunner
	mu      sync.Mutex
	created bool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingPostCreateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "cp" {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if args[0] == "run" {
		r.mu.Lock()
		r.created = true
		r.mu.Unlock()
	}
	if args[0] == "inspect" {
		r.mu.Lock()
		created := r.created
		r.mu.Unlock()
		if !created {
			return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: `not found: "` + args[len(args)-1] + `"`}
		}
		creation := r.creations[args[len(args)-1]]
		return []byte(`[{"id":"` + args[len(args)-1] + `","configuration":{"id":"` + args[len(args)-1] + `","image":{"reference":"redis:7-alpine"},"labels":{"` + managedLabel + `":"true","` + reuseLabel + `":"true","` + creationLabel + `":"` + creation + `"},"publishedPorts":[]},"status":{"state":"running","networks":[]}}]`), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *blockingPostCreateRunner) unblock() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

func TestReusePostCreateUsesSharedGenerationPin(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	runner := &blockingPostCreateRunner{
		fakeRunner: base,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	t.Cleanup(runner.unblock)
	name := "post-create-pin-" + newContainerName()
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName(name), WithReuse(), WithFiles(File{HostPath: writeFreshReviewFile(t), ContainerPath: "/tmp/input"}),
			withRunner(runner), withEngine(appleEngine{}))
		done <- err
	}()
	select {
	case <-runner.started:
	case err := <-done:
		t.Fatalf("post-create returned before copy: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("post-create copy did not reach the backend")
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	readerUnlock, err := lockNameShared(readCtx, name)
	readCancel()
	if err != nil {
		t.Fatalf("shared reader blocked by post-create copy: %v", err)
	}
	readerUnlock()
	writerCtx, writerCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, writerErr := lockName(writerCtx, name)
	writerCancel()
	if !errors.Is(writerErr, context.DeadlineExceeded) {
		t.Fatalf("writer was not excluded by post-create copy: %v", writerErr)
	}

	runner.unblock()
	if err := <-done; err != nil {
		t.Fatalf("reuse Run: %v", err)
	}
}

func TestAppleDeleteClassificationPreservesWrongUppercaseTarget(t *testing.T) {
	err := &cli.CLIError{
		Binary: "container",
		Args:   []string{"delete", "--force", "MyCtr"},
		Stderr: "Container Not Found: OTHER",
	}
	if isDeleteNotFound(appleEngine{}, "MyCtr", err) {
		t.Fatal("case-insensitive matching accepted a different target")
	}
	if !strings.Contains(err.Stderr, "OTHER") {
		t.Fatal("test fixture lost its different target")
	}
}
