//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
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

func TestEnsurePrivateDirTightensOwnerOnlyMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Owner-only but not 0700, which a restrictive umask can produce.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != nameLockDirPerm {
		t.Fatalf("lock directory mode = %04o, want %04o", info.Mode().Perm(), nameLockDirPerm)
	}
}

func TestEnsurePrivateDirRejectsGroupAccessibleMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err == nil {
		t.Fatal("group-accessible lock directory was accepted")
	}
}

func TestOpenNameLockPathTightensOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "name.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A restrictive umask can leave an owner-only file without the write
	// bit; the protocol must repair it instead of failing forever.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	f, err := openNameLockPath(path)
	if err != nil {
		t.Fatalf("openNameLockPath: %v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != nameLockFilePerm {
		t.Fatalf("lock file mode = %04o, want %04o", info.Mode().Perm(), nameLockFilePerm)
	}
}

func TestOpenNameLockPathRejectsGroupAccessibleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "name.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := openNameLockPath(path)
	if err == nil {
		_ = f.Close()
		t.Fatal("group-readable lock file was accepted")
	}
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
