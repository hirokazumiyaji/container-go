//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockNameSerializesHolders(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock while held: err = %v, want deadline exceeded", err)
	}

	unlock()
	unlock2, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestNameLockPathIsStableAcrossTempDirs(t *testing.T) {
	name := "lock-" + newContainerName()
	first, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}

	t.Setenv("TMPDIR", t.TempDir())
	second, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath after TMPDIR change: %v", err)
	}
	if first != second {
		t.Fatalf("nameLockPath changed with TMPDIR: %q != %q", first, second)
	}
	if strings.Contains(filepath.Base(first), name) {
		t.Fatalf("lock filename %q contains the un-hashed name", filepath.Base(first))
	}
}

func TestNameLockRejectsSymlink(t *testing.T) {
	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do not lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := lockName(context.Background(), name); err == nil {
		t.Fatal("lockName followed a pre-existing symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do not lock" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestNameLockRejectsWrongPermissions(t *testing.T) {
	name := "lock-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatalf("nameLockPath: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lockName(context.Background(), name); err == nil {
		t.Fatal("lockName accepted a world-readable lock file")
	}
}

func TestNameLockReusesStaleFile(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("initial lock: %v", err)
	}
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	// The file is deliberately retained after unlock. It is stale as a
	// lock, but its inode is still the coordination point and must be
	// reusable rather than deleted.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stale lock file missing: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("lock file size = %d, want zero", info.Size())
	}
	unlock, err = lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lock stale file: %v", err)
	}
	unlock()
}

func TestTerminateWaitsForNameLockBeforeInspecting(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	// Another process holds the name: this handle's inspect-then-delete
	// must not start until it is released, so a bounded context fails
	// before any backend call.
	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = ctr.Terminate(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Terminate = %v, want lock failure", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 while the name is locked elsewhere", r.deleteCalls)
	}
}

func TestTerminateContainerBoundsNameLockWait(t *testing.T) {
	oldTimeout := terminateTimeout
	terminateTimeout = 100 * time.Millisecond
	t.Cleanup(func() { terminateTimeout = oldTimeout })

	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	start := time.Now()
	err = TerminateContainer(ctr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TerminateContainer = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("TerminateContainer took %v, want bounded wait", elapsed)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", r.deleteCalls)
	}
}

func TestNameLockHelperProcess(t *testing.T) {
	if os.Getenv("CONTAINERGO_LOCK_HELPER") != "1" {
		return
	}
	name := os.Getenv("CONTAINERGO_LOCK_NAME")
	ctx := context.Background()
	if raw := os.Getenv("CONTAINERGO_LOCK_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	unlock, err := lockName(ctx, name)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer unlock()
	fmt.Println("locked")
}

func TestNameLockSerializesAcrossProcessesWithDifferentTempDirs(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("parent lockName: %v", err)
	}

	runHelper := func(tempDir, timeout string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestNameLockHelperProcess$")
		cmd.Env = append(os.Environ(),
			"CONTAINERGO_LOCK_HELPER=1",
			"CONTAINERGO_LOCK_NAME="+name,
			"CONTAINERGO_LOCK_TIMEOUT="+timeout,
			"TMPDIR="+tempDir,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lock helper: %v\n%s", err, output)
		}
		return string(output)
	}

	output := runHelper(t.TempDir(), "150ms")
	if !strings.Contains(output, "deadline exceeded") {
		unlock()
		t.Fatalf("held lock helper output = %q, want deadline exceeded", output)
	}
	unlock()

	output = runHelper(t.TempDir(), "1s")
	if !strings.Contains(output, "locked") {
		t.Fatalf("released lock helper output = %q, want locked", output)
	}
}
