//go:build !windows

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNameLockResolvesTrustedSymlinkBases(t *testing.T) {
	realHome := t.TempDir()
	realCache := t.TempDir()
	realTemp := t.TempDir()
	aliasRoot := t.TempDir()
	homeAlias := filepath.Join(aliasRoot, "home")
	cacheAlias := filepath.Join(aliasRoot, "cache")
	tempAlias := filepath.Join(aliasRoot, "tmp")
	for alias, target := range map[string]string{
		homeAlias:  realHome,
		cacheAlias: realCache,
		tempAlias:  realTemp,
	} {
		if err := os.Symlink(target, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	oldRoot := nameLockStateRootOverride
	nameLockStateRootOverride = homeAlias
	t.Cleanup(func() { nameLockStateRootOverride = oldRoot })
	t.Setenv("HOME", homeAlias)
	t.Setenv("XDG_CACHE_HOME", cacheAlias)
	t.Setenv("XDG_STATE_HOME", homeAlias)
	t.Setenv("TMPDIR", tempAlias)

	name := "symlink-base-" + newContainerName()
	paths := make([]string, 0, 3)
	for _, resolve := range []func(string) (string, error){
		legacyNameLockPath,
		transitionalNameLockPath,
		nameLockPath,
	} {
		path, err := resolve(name)
		if err != nil {
			t.Fatalf("resolve lock path: %v", err)
		}
		paths = append(paths, path)
		for _, alias := range []string{homeAlias, cacheAlias, tempAlias} {
			if path == alias || strings.HasPrefix(path, alias+string(filepath.Separator)) {
				t.Fatalf("resolved path %q still uses trusted alias %q", path, alias)
			}
		}
	}

	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName through symlink bases: %v", err)
	}
	unlock()
	if len(paths) != 3 {
		t.Fatalf("resolved lock barriers = %d, want 3", len(paths))
	}
}

func TestNameLockStillRejectsSymlinkInsidePrivateNamespace(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	aliasParent := filepath.Join(root, "alias")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ensureDirectoryParents(filepath.Join(aliasParent, "container-go", "locks")); err == nil {
		t.Fatal("private namespace followed a symlink below the trusted base")
	}
}
