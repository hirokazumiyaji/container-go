//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewNameLockStableAndSecure(t *testing.T) {
	old := nameLockStateRootOverride
	root := t.TempDir()
	nameLockStateRootOverride = root
	t.Cleanup(func() { nameLockStateRootOverride = old })
	name := "review-" + newContainerName()
	first, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	second, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("paths changed: %q %q", first, second)
	}
	if strings.Contains(filepath.Base(first), name) {
		t.Fatal("name leaked")
	}
	u, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	u()
	if err := os.Chmod(second, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := lockName(context.Background(), name); err == nil {
		t.Fatal("accepted 0644")
	}
}

func TestReviewNameLockRejectsSymlinkInstalledBeforeOpen(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	name := "review-race-" + newContainerName()
	path, err := nameLockPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	var hookErr error
	hooks := &nameLockHooks{beforeOpen: func(stage nameLockStage, got string) {
		if stage != stateNameLockStage || got != path || hookErr != nil {
			return
		}
		if err := os.Remove(path); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Symlink(target, path)
	}}
	if _, err := lockNameWithHooks(context.Background(), name, hooks); err == nil || (!strings.Contains(err.Error(), "without following") && !strings.Contains(err.Error(), "symbolic link")) {
		t.Fatalf("lockName error = %v, want O_NOFOLLOW rejection", err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestReviewNameLockCompatibilityFailsClosed(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = ""
	t.Cleanup(func() { nameLockStateRootOverride = old })
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if _, err := lockName(context.Background(), "compat-review"); err == nil || !errors.Is(err, ErrNameLockCompatibility) {
		t.Fatalf("lockName error = %v, want compatibility failure", err)
	}
}

func TestReviewPrivateDirRejectsSymlinkParentBeforeMissingTail(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	aliasParent := filepath.Join(root, "alias")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skip(err)
	}
	if err := ensurePrivateDir(filepath.Join(aliasParent, "missing", "locks")); err == nil {
		t.Fatal("ensurePrivateDir followed a symlink in an existing parent")
	}
}

func TestReviewNameLockRejectsSymlinkRoot(t *testing.T) {
	old := nameLockStateRootOverride
	t.Cleanup(func() { nameLockStateRootOverride = old })
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skip(err)
	}
	nameLockStateRootOverride = alias
	if _, err := lockName(context.Background(), "symlink-review"); err == nil || !errors.Is(err, ErrNameLockCompatibility) {
		t.Fatalf("err=%v", err)
	}
}
