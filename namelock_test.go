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
	"syscall"
	"testing"
	"time"
)

// isolateNameLockDir points the name lock at a per-test directory. Tests that
// plant unsafe lock files or count lock files must not observe the shared
// per-user cache, and the spawned helper processes inherit the override.
func isolateNameLockDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "locks")
	if err := os.Mkdir(dir, nameLockDirPerm); err != nil {
		t.Fatalf("create test lock directory: %v", err)
	}
	t.Setenv("CONTAINERGO_LOCK_DIR", dir)
	return dir
}

func TestLockNameSerializesHolders(t *testing.T) {
	isolateNameLockDir(t)
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
	// Exercise the default per-user cache path, not the override.
	isolateUserCacheBase(t, true)
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
	if strings.Contains(first, name) {
		t.Fatalf("lock path %q exposes the container name", first)
	}
}

func TestNameLockRejectsSymlink(t *testing.T) {
	isolateNameLockDir(t)
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
	isolateNameLockDir(t)
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
	isolateNameLockDir(t)
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
	isolateNameLockDir(t)
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

func TestTerminateContainerHonorsContextDeadline(t *testing.T) {
	isolateNameLockDir(t)
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = TerminateContainer(ctx, ctr)
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

func TestCleanupBoundsNameLockWait(t *testing.T) {
	isolateNameLockDir(t)
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
	tb := &cleanupTBRecorder{}
	registerCleanup(tb, ctr, true)
	if len(tb.callbacks) != 1 {
		t.Fatalf("callbacks = %d, want 1", len(tb.callbacks))
	}
	start := time.Now()
	tb.callbacks[0]()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Cleanup took %v, want bounded wait", elapsed)
	}
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], context.DeadlineExceeded.Error()) {
		t.Fatalf("cleanup errors = %v, want deadline exceeded", tb.errors)
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
	isolateNameLockDir(t)
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("parent lockName: %v", err)
	}

	runHelper := func(tempDir, timeout string) string {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNameLockHelperProcess$")
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

func TestNameLockDirUsesPerUserCache(t *testing.T) {
	// Force the default path even if the developer's shell sets an override.
	cacheDir, _ := isolateUserCacheBase(t, true)
	dir, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir: %v", err)
	}
	want := filepath.Join(cacheDir, "containergo", "locks")
	if dir != want {
		t.Fatalf("nameLockDir = %q, want per-user cache path %q", dir, want)
	}
	for _, path := range []string{dir, filepath.Dir(dir)} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != nameLockDirPerm {
			t.Fatalf("%s permissions = %04o, want %04o", path, info.Mode().Perm(), nameLockDirPerm)
		}
	}
}

func TestNameLockDirRejectsWorldWritableCacheBase(t *testing.T) {
	cacheDir, _ := isolateUserCacheBase(t, true)
	if err := os.MkdirAll(cacheDir, nameLockDirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := nameLockDir(); err == nil {
		t.Fatal("nameLockDir accepted a group- or world-writable cache base")
	}
}

func TestNameLockDirRejectsSubstitutedAppDir(t *testing.T) {
	cacheDir, _ := isolateUserCacheBase(t, true)
	if err := os.MkdirAll(cacheDir, nameLockDirPerm); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "substituted")
	if err := os.Mkdir(target, nameLockDirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cacheDir, "containergo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := nameLockDir(); err == nil {
		t.Fatal("nameLockDir accepted a substituted lock directory")
	}
}

func TestLockNameKeepsLockDirectoryBounded(t *testing.T) {
	dir := isolateNameLockDir(t)
	for i := 0; i < nameLockBucketCount*2; i++ {
		name := fmt.Sprintf("bounded-%d", i)
		unlock, err := lockName(context.Background(), name)
		if err != nil {
			t.Fatalf("lockName(%s): %v", name, err)
		}
		unlock()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("lockName created no lock files")
	}
	if len(entries) > nameLockBucketCount {
		t.Fatalf("lock directory holds %d files after %d names, want at most %d",
			len(entries), nameLockBucketCount*2, nameLockBucketCount)
	}
}

func TestNameLockBucketCollisionSerializes(t *testing.T) {
	isolateNameLockDir(t)
	byPath := make(map[string]string)
	var first, second string
	for i := 0; i < nameLockBucketCount*4 && second == ""; i++ {
		name := fmt.Sprintf("collision-%d", i)
		path, err := nameLockPath(name)
		if err != nil {
			t.Fatalf("nameLockPath(%s): %v", name, err)
		}
		other, ok := byPath[path]
		switch {
		case ok && other != name:
			first, second = other, name
		case !ok:
			byPath[path] = name
		}
	}
	if second == "" {
		t.Fatalf("no bucket collision among %d names", nameLockBucketCount*4)
	}
	unlock, err := lockName(context.Background(), first)
	if err != nil {
		t.Fatalf("lockName(%s): %v", first, err)
	}
	defer unlock()

	// A bucket collision must only add serialization; the second name
	// must not be able to run beside the first.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockName(%s) while %s shares its bucket: err = %v, want deadline exceeded", second, first, err)
	}
}

func TestReaperNameLockPathMatchesLockName(t *testing.T) {
	isolateNameLockDir(t)
	name := "reaper-" + newContainerName()
	path, err := reaperNameLockPath(name)
	if err != nil {
		t.Fatalf("reaperNameLockPath: %v", err)
	}
	want, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if path != want {
		t.Fatalf("reaperNameLockPath = %q, nameLockPath = %q", path, want)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat reaper lock file: %v", err)
	}
	if info.Mode().Perm() != nameLockFilePerm {
		t.Fatalf("reaper lock file permissions = %04o, want %04o", info.Mode().Perm(), nameLockFilePerm)
	}
}

func TestReaperNameLockPathRejectsUnsafeLockFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"world-readable file", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Mkdir(path, nameLockFilePerm); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateNameLockDir(t)
			name := "reaper-" + newContainerName()
			path, err := nameLockPath(name)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, path)
			if _, err := reaperNameLockPath(name); err == nil {
				t.Fatalf("reaperNameLockPath accepted a %s", tc.name)
			}
		})
	}
}

func TestReaperNameLockPathRejectsFIFO(t *testing.T) {
	isolateNameLockDir(t)
	name := "reaper-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, nameLockFilePerm); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// The pre-open Lstat check rejects the FIFO; the timeout also catches a
	// regression back to a blocking open.
	done := make(chan error, 1)
	go func() {
		_, err := reaperNameLockPath(name)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reaperNameLockPath accepted a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reaperNameLockPath blocked on a FIFO")
	}
}
