//go:build windows

package container

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestWindowsEnvFilesFailClosedBeforeUse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)

	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if !errors.Is(err, ErrEnvFileUnsupported) {
		t.Fatalf("writeEnvFile error = %v, want ErrEnvFileUnsupported", err)
	}
	if path != "" || dir != "" {
		t.Fatalf("writeEnvFile returned a path after failing closed: path=%q dir=%q", path, dir)
	}
	assertNoWindowsEnvArtifacts(t, tmp)

	f := newTestRunner()
	_, err = Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithEnv(map[string]string{"TOKEN": "secret"}),
		withRunner(f), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrEnvFileUnsupported) {
		t.Fatalf("Run error = %v, want ErrEnvFileUnsupported", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Run invoked the backend before failing closed: %v", f.calls)
	}
	assertNoWindowsEnvArtifacts(t, tmp)

	execFake := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, execFake)
	execFake.calls = nil
	_, _, err = ctr.Exec(context.Background(), []string{"true"},
		WithExecEnv(map[string]string{"TOKEN": "secret"}))
	if !errors.Is(err, ErrEnvFileUnsupported) {
		t.Fatalf("Exec error = %v, want ErrEnvFileUnsupported", err)
	}
	if len(execFake.calls) != 0 {
		t.Fatalf("Exec invoked the backend before failing closed: %v", execFake.calls)
	}
	assertNoWindowsEnvArtifacts(t, tmp)

	if err := cleanupStaleEnvFiles(); !errors.Is(err, ErrEnvFileUnsupported) {
		t.Fatalf("stale cleanup error = %v, want ErrEnvFileUnsupported", err)
	}
}

func assertNoWindowsEnvArtifacts(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == envFileRootName ||
			entry.Name() == envFileRootMarkerName ||
			entry.Name() == envFileDirMarkerName ||
			entry.Name() == envFileLockName ||
			entry.Name() == envFileName {
			t.Fatalf("Windows guard created environment-file artifact %q", entry.Name())
		}
	}
}
