//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestNameLockBarriersCoverLegacyTransitionalAndDurable(t *testing.T) {
	name := "barriers-" + newContainerName()
	resolved, err := resolveNameLocks(name)
	if err != nil {
		t.Fatalf("resolveNameLocks: %v", err)
	}
	paths := resolved.ordered()
	if len(paths) != nameLockBarrierCount {
		t.Fatalf("barriers = %v, want %d", paths, nameLockBarrierCount)
	}
	seen := map[string]bool{}
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			t.Errorf("barrier %d = %q, want an absolute path", i, path)
		}
		if seen[path] {
			t.Errorf("barrier %d = %q, want a distinct file per stage", i, path)
		}
		seen[path] = true
	}
	if want := filepath.Join(os.TempDir(), "containergo-"+name+".lock"); paths[0] != want {
		t.Errorf("legacy barrier = %q, want %q", paths[0], want)
	}
	if !filepath.IsAbs(paths[nameLockBarrierCount-1]) ||
		!filepathContains(filepath.Dir(paths[nameLockBarrierCount-1]), strconv.Itoa(os.Getuid())) {
		t.Errorf("durable barrier = %q, want an account-uid-scoped path", paths[nameLockBarrierCount-1])
	}
}

func TestNameLockDurableBarrierIgnoresEnvironment(t *testing.T) {
	// Cooperating processes may be launched with a different HOME,
	// XDG_CACHE_HOME, or TMPDIR. The durable barrier must still be the
	// same file for the account, while the environment-derived barriers
	// follow the environment.
	name := "envless-" + newContainerName()
	before, err := resolveNameLocks(name)
	if err != nil {
		t.Fatalf("resolveNameLocks: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("TMPDIR", t.TempDir())

	after, err := resolveNameLocks(name)
	if err != nil {
		t.Fatalf("resolveNameLocks after environment change: %v", err)
	}
	if before.durable != after.durable {
		t.Errorf("durable barrier = %q, want the unchanged %q", after.durable, before.durable)
	}
	if before.legacy == after.legacy {
		t.Error("legacy barrier did not follow TMPDIR")
	}
	if before.transitional == after.transitional {
		t.Error("transitional barrier did not follow XDG_CACHE_HOME")
	}
}

func TestLockNameWaitsForEveryBarrier(t *testing.T) {
	// Holding any single barrier must keep a cooperating caller out, so
	// a process built against an older lock location still excludes a new
	// one and the reverse.
	name := "held-" + newContainerName()
	resolved, err := resolveNameLocks(name)
	if err != nil {
		t.Fatalf("resolveNameLocks: %v", err)
	}
	for i, path := range resolved.ordered() {
		f, err := openNameLockFile(path)
		if err != nil {
			t.Fatalf("open barrier %d (%s): %v", i, path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("flock barrier %d: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, lockErr := lockName(ctx, name)
		cancel()
		if !errors.Is(lockErr, context.DeadlineExceeded) {
			t.Errorf("lockName with barrier %d held = %v, want deadline exceeded", i, lockErr)
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := lockName(ctx, name)
	if err != nil {
		t.Fatalf("lockName after release: %v", err)
	}
	unlock()
}

func TestReaperNameLockMetadataTracksBarrierIdentity(t *testing.T) {
	// The child refuses to delete when a barrier no longer has the
	// identity the library registered, so a replaced lock file must
	// produce a different identity.
	name := "identity-" + newContainerName()
	paths, identities, err := reaperNameLockMetadata(name)
	if err != nil {
		t.Fatalf("reaperNameLockMetadata: %v", err)
	}
	if len(paths) != nameLockBarrierCount || len(identities) != nameLockBarrierCount {
		t.Fatalf("paths = %v, identities = %v", paths, identities)
	}
	seen := map[string]bool{}
	for i, identity := range identities {
		if !reaperLockIdentityRE.MatchString(identity) {
			t.Errorf("barrier %d identity = %q, want dev:inode:uid", i, identity)
		}
		if seen[identity] {
			t.Errorf("barrier %d shares identity %q with another barrier", i, identity)
		}
		seen[identity] = true
	}

	// Replacing the durable barrier inode must be observable.
	replacement := paths[nameLockBarrierCount-1] + ".replacement"
	if err := os.Rename(paths[nameLockBarrierCount-1], replacement); err != nil {
		t.Skipf("cannot replace the durable barrier: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Rename(replacement, paths[nameLockBarrierCount-1])
	})
	if err := os.WriteFile(paths[nameLockBarrierCount-1], nil, nameLockFilePerm); err != nil {
		t.Fatalf("recreate durable barrier: %v", err)
	}
	_, changed, err := reaperNameLockMetadata(name)
	if err != nil {
		t.Fatalf("reaperNameLockMetadata after replacement: %v", err)
	}
	if changed[nameLockBarrierCount-1] == identities[nameLockBarrierCount-1] {
		t.Error("a replaced barrier kept its identity; the reaper could not detect the swap")
	}
}

// filepathContains reports whether any path element of dir equals
// element.
func filepathContains(dir, element string) bool {
	current := dir
	for {
		if filepath.Base(current) == element {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}
