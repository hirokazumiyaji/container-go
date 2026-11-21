package container

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateEnvFileRoot points the per-user cache base at a temporary directory
// and pre-creates the environment root there. It returns the root path that
// the platform implementation will use.
func isolateEnvFileRoot(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("LocalAppData", base)

	cache := base
	if runtime.GOOS == "darwin" {
		cache = filepath.Join(base, "Library", "Caches")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(cache, envFileRootName)
	if err := withEnvFileRoot(cache, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return root
}

func envDirsIn(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), envFileDirPrefix) {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}
