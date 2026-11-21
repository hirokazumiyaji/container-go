//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateUserCacheBase points os.UserCacheDir at a temporary path and returns
// the cache base and its immediate parent without creating the base. It clears
// CONTAINERGO_LOCK_DIR so callers exercise the default path. When createParent
// is true the parent already exists as a private directory; otherwise it is
// left missing. On Darwin the cache lives in $HOME/Library/Caches, so Library
// is the parent.
func isolateUserCacheBase(t *testing.T, createParent bool) (cache, parent string) {
	t.Helper()
	t.Setenv("CONTAINERGO_LOCK_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "darwin" {
		parent = filepath.Join(home, "Library")
		cache = filepath.Join(parent, "Caches")
	} else {
		parent = filepath.Join(home, "cache-parent")
		cache = filepath.Join(parent, "cache")
		t.Setenv("XDG_CACHE_HOME", cache)
	}
	if createParent {
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("create cache parent: %v", err)
		}
	}
	return cache, parent
}

// assertContainergoCacheRootRejected checks that the shared helper and both of
// its callers refuse the current cache layout.
func assertContainergoCacheRootRejected(t *testing.T) {
	t.Helper()
	if root, err := ensureContainergoCacheRoot(nil); err == nil {
		t.Fatalf("ensureContainergoCacheRoot accepted %q", root)
	}
	if root, cleanup, err := prepareCopyStagingRoot(); err == nil {
		_ = cleanup()
		t.Fatalf("prepareCopyStagingRoot accepted %q", root)
	}
	if dir, err := nameLockDir(); err == nil {
		t.Fatalf("nameLockDir accepted %q", dir)
	}
}

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("%s is not a real directory", path)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("%s permissions = %04o, want 0700", path, info.Mode().Perm())
	}
}

func TestContainergoCacheRootRejectsSymlinkedBase(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cache); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assertContainergoCacheRootRejected(t)
	if _, err := os.Lstat(filepath.Join(target, "containergo")); !os.IsNotExist(err) {
		t.Fatalf("symlink target gained a cache root: %v", err)
	}
}

func TestContainergoCacheRootRejectsWritableBase(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o777); err != nil {
		t.Fatal(err)
	}
	assertContainergoCacheRootRejected(t)
	if _, err := os.Lstat(filepath.Join(cache, "containergo")); !os.IsNotExist(err) {
		t.Fatalf("writable cache base gained a containergo directory: %v", err)
	}
}

func TestContainergoCacheRootAcceptsExistingBaseMode(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := ensureContainergoCacheRoot(nil)
	if err != nil {
		t.Fatalf("ensureContainergoCacheRoot rejected a 0755 cache base: %v", err)
	}
	if info, statErr := os.Lstat(cache); statErr != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("cache base mode changed: info=%v err=%v", info, statErr)
	}
	assertPrivateDir(t, root)

	dir, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir: %v", err)
	}
	assertPrivateDir(t, dir)
}

func TestContainergoCacheRootRejectsNonStickyWritableParentBeforeCreate(t *testing.T) {
	cache, parent := isolateUserCacheBase(t, true)
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	assertContainergoCacheRootRejected(t)
	if _, err := os.Lstat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache base was created below a writable parent: %v", err)
	}
}

func TestContainergoCacheRootAllowsStickyWritableParent(t *testing.T) {
	cache, parent := isolateUserCacheBase(t, true)
	if err := os.Chmod(parent, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	root, err := ensureContainergoCacheRoot(nil)
	if err != nil {
		t.Fatalf("ensureContainergoCacheRoot: %v", err)
	}
	if want := filepath.Join(cache, "containergo"); root != want {
		t.Fatalf("cache root = %q, want %q", root, want)
	}
	assertPrivateDir(t, cache)
	assertPrivateDir(t, root)

	dir, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir: %v", err)
	}
	if want := filepath.Join(root, "locks"); dir != want {
		t.Fatalf("nameLockDir = %q, want %q", dir, want)
	}
}

func TestContainergoCacheRootRejectsSubstitutedRoot(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cache, "containergo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assertContainergoCacheRootRejected(t)
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("substituted cache root received entries: %v", entries)
	}
}

func TestNameLockDirRejectsMissingCacheParent(t *testing.T) {
	cache, parent := isolateUserCacheBase(t, false)
	dir, err := nameLockDir()
	if err == nil {
		t.Fatalf("nameLockDir accepted %q with a missing cache parent", dir)
	}
	if !strings.Contains(err.Error(), "CONTAINERGO_LOCK_DIR") {
		t.Fatalf("nameLockDir error = %v, want a CONTAINERGO_LOCK_DIR hint", err)
	}
	if _, statErr := os.Lstat(parent); !os.IsNotExist(statErr) {
		t.Fatalf("cache parent %q was created: %v", parent, statErr)
	}
	if _, statErr := os.Lstat(cache); !os.IsNotExist(statErr) {
		t.Fatalf("cache base %q was created: %v", cache, statErr)
	}
}

func TestContainergoCacheRootRepairsRootPermissions(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	appRoot := filepath.Join(cache, "containergo")
	if err := os.MkdirAll(appRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(appRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := ensureContainergoCacheRoot(nil)
	if err != nil {
		t.Fatalf("ensureContainergoCacheRoot: %v", err)
	}
	if root != appRoot {
		t.Fatalf("cache root = %q, want %q", root, appRoot)
	}
	assertPrivateDir(t, root)

	dir, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir: %v", err)
	}
	if want := filepath.Join(root, "locks"); dir != want {
		t.Fatalf("nameLockDir = %q, want %q", dir, want)
	}
	assertPrivateDir(t, dir)
}

func TestNameLockDirRepairsLockDirectoryPermissions(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	locks := filepath.Join(cache, "containergo", "locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locks, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir: %v", err)
	}
	if dir != locks {
		t.Fatalf("nameLockDir = %q, want %q", dir, locks)
	}
	assertPrivateDir(t, dir)
}

func TestContainergoCacheRootSharedByCopyStagingAndNameLock(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	staging, cleanup, err := prepareCopyStagingRoot()
	if err != nil {
		t.Fatalf("prepareCopyStagingRoot: %v", err)
	}
	defer func() { _ = cleanup() }()
	locks, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir: %v", err)
	}

	appRoot := filepath.Join(cache, "containergo")
	if got := filepath.Dir(staging); got != appRoot {
		t.Fatalf("staging root parent = %q, want %q", got, appRoot)
	}
	if got := filepath.Dir(locks); got != appRoot {
		t.Fatalf("lock directory parent = %q, want %q", got, appRoot)
	}
	for _, path := range []string{cache, appRoot, staging, locks} {
		assertPrivateDir(t, path)
	}
}

func TestNameLockDirHonorsExplicitLockDirOverride(t *testing.T) {
	cache, _ := isolateUserCacheBase(t, true)
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o777); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(t.TempDir(), "override")
	if err := os.Mkdir(override, nameLockDirPerm); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERGO_LOCK_DIR", override)

	dir, err := nameLockDir()
	if err != nil {
		t.Fatalf("nameLockDir with override: %v", err)
	}
	if dir != override {
		t.Fatalf("nameLockDir = %q, want override %q", dir, override)
	}
	if _, err := os.Lstat(filepath.Join(cache, "containergo")); !os.IsNotExist(err) {
		t.Fatalf("override touched the rejected cache base: %v", err)
	}
}
