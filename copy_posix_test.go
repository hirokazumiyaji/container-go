//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestCopyToContainerRejectsFIFO(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(source, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	err := ctr.CopyToContainer(context.Background(), source, "/tmp/pipe")
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
	if runner.callWith("cp") != nil {
		t.Error("CLI was called for a FIFO source")
	}
}

func TestCopyToContainerRejectsDevice(t *testing.T) {
	const source = "/dev/null"
	info, err := os.Lstat(source)
	if err != nil {
		t.Skipf("device unavailable: %v", err)
	}
	if info.Mode()&os.ModeDevice == 0 {
		t.Skip("source is not a device")
	}
	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)

	err = ctr.CopyToContainer(context.Background(), source, "/tmp/device")
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
	if runner.callWith("cp") != nil {
		t.Error("CLI was called for a device source")
	}
}

func TestCopyStagingRootAndDirectoryAreCurrentUserOnly(t *testing.T) {
	root, err := prepareCopyStagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(root), root} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			t.Fatalf("%q is not owned by the current user", path)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%q permissions = %04o, want 0700", path, info.Mode().Perm())
		}
	}

	dir, err := createCopyStagingDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("operation staging directory permissions = %04o, want 0700", info.Mode().Perm())
	}
}

func TestCopyStagingDoesNotFallBackToMutableConfiguredDirectories(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("environment-specific cache-directory test")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	xdg := filepath.Join(root, "xdg")
	tmp := filepath.Join(root, "tmp")
	for _, path := range []string{home, xdg, tmp, filepath.Join(home, "Library", "Caches")} {
		if err := os.MkdirAll(path, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", xdg)
	t.Setenv("TMPDIR", tmp)
	source := filepath.Join(tmp, "source.txt")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if dir, err := newCopyStagingDir(source); err == nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("newCopyStagingDir used an untrusted configured directory: %q", dir)
	}
}
