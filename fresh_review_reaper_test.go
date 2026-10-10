//go:build !windows

package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type blockingExternalRunner struct {
	bin      string
	started  chan struct{}
	release  chan struct{}
	startOne sync.Once
	releaseO sync.Once
}

func (r *blockingExternalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
	case "run":
		r.startOne.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return []byte("pre-shared\n"), nil, nil
	case "inspect":
		return []byte(fmt.Sprintf(`[{"id":"pre-shared","configuration":{"id":"pre-shared","image":{"reference":"redis:7-alpine"},"labels":{%q:"true",%q:%q,%q:"true",%q:"pre-group",%q:"0123456789abcdef"}},"status":{"state":"running","networks":[]}}]`, managedLabel, sessionLabel, sessionID(), reuseLabel, reuseGroupLabel, creationLabel)), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *blockingExternalRunner) External() bool         { return true }
func (r *blockingExternalRunner) ExternalBinary() string { return r.bin }
func (r *blockingExternalRunner) unblock() {
	r.releaseO.Do(func() { close(r.release) })
}

type uncertainExternalRunner struct {
	bin string
}

func (r *uncertainExternalRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
	case "run":
		return []byte("uncertain\n"), nil, nil
	case "inspect":
		return nil, nil, fmt.Errorf("temporary ownership inspect failure")
	default:
		return nil, nil, nil
	}
}

func (r *uncertainExternalRunner) External() bool         { return true }
func (r *uncertainExternalRunner) ExternalBinary() string { return r.bin }

type deleteExternalRunner struct {
	bin string
}

func (r *deleteExternalRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "rm" || args[0] == "delete" {
		return nil, nil, nil
	}
	return nil, nil, nil
}
func (r *deleteExternalRunner) External() bool         { return true }
func (r *deleteExternalRunner) ExternalBinary() string { return r.bin }

func TestFreshReviewConfirmedDeleteUnregistersReaperRecords(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	runner := &deleteExternalRunner{bin: bin}
	watchdog := newReaper(bin, "rm", dockerDeleteVolumesFlag)
	if err := watchdog.register("delete-identity", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	uid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := watchdog.register(uid, ""); err != nil {
		t.Fatal(err)
	}
	globalReapersMu.Lock()
	globalReapers[bin] = watchdog
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		watchdog.closeStdin()
	})

	ctr := &Container{id: "delete-identity", uid: uid, creation: "0123456789abcdef", runner: runner, eng: dockerEngine{}}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	watchdog.mu.Lock()
	entries := len(watchdog.entries)
	watchdog.mu.Unlock()
	if entries != 0 {
		t.Fatalf("reaper retained %d records after confirmed delete", entries)
	}
}

func TestFreshReviewUncertainReuseOwnershipStaysShared(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &uncertainExternalRunner{bin: bin}
	_, err := reuseCreate(context.Background(), "redis:7-alpine", &config{
		runner:   runner,
		eng:      appleEngine{},
		name:     "uncertain",
		reuse:    true,
		creation: "0123456789abcdef",
	})
	if err == nil {
		t.Fatal("reuseCreate succeeded despite uncertain ownership")
	}
	globalReapersMu.Lock()
	r := globalReapers[bin]
	globalReapersMu.Unlock()
	if r == nil {
		t.Fatal("uncertain reuse did not retain a reaper record")
	}
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		r.closeStdin()
	})
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || !entries[0].shared || entries[0].pending {
		t.Fatalf("uncertain reuse entries = %+v, want one shared entry", entries)
	}
}

func TestFreshReviewReuseRecordIsSharedBeforeCreatePublication(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &blockingExternalRunner{bin: bin, started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(runner.unblock)

	result := make(chan error, 1)
	go func() {
		_, err := reuseCreate(context.Background(), "redis:7-alpine", &config{
			runner:     runner,
			eng:        appleEngine{},
			name:       "pre-shared",
			reuse:      true,
			reuseGroup: "pre-group",
			creation:   "0123456789abcdef",
		})
		result <- err
	}()

	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("reuse create did not reach backend run")
	}
	globalReapersMu.Lock()
	r := globalReapers[bin]
	globalReapersMu.Unlock()
	if r == nil {
		t.Fatal("reuse create did not register a reaper")
	}
	t.Cleanup(func() {
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
		r.closeStdin()
	})
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || !entries[0].shared || entries[0].pending {
		t.Fatalf("reuse record during create = %+v, want one shared non-pending entry", entries)
	}

	runner.unblock()
	if err := <-result; err != nil {
		t.Fatalf("reuseCreate: %v", err)
	}
}
