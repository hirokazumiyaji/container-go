//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"context"
	"errors"
	"net"
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

func TestCopyToContainerRejectsUnixSocket(t *testing.T) {
	source := filepath.Join(t.TempDir(), "socket")
	listener, err := net.Listen("unix", source)
	if err != nil {
		t.Skipf("Unix socket unavailable: %v", err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(true)
	}
	defer listener.Close()

	runner := &cpRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, runner)
	err = ctr.CopyToContainer(context.Background(), source, "/tmp/socket")
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
	if runner.callWith("cp") != nil {
		t.Error("CLI was called for a Unix socket source")
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

	opened, err := openVerifiedCopySource(source, openCopySource)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.file.Close()
	if dir, err := newCopyStagingDir(opened); err == nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("newCopyStagingDir used an untrusted configured directory: %q", dir)
	}
}

func TestSnapshotCopyDirectoryIgnoresPostOpenReplacementForStagingAncestry(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("environment-specific cache-directory test")
	}
	root := t.TempDir()
	source := filepath.Join(root, "tree")
	original := filepath.Join(root, "original-tree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data.txt"), []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", filepath.Join(source, "home"))
	} else {
		t.Setenv("XDG_CACHE_HOME", filepath.Join(source, "cache"))
	}

	opened, err := openVerifiedCopySource(source, openCopySource)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.file.Close()
	if err := os.Rename(source, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		if err := os.MkdirAll(filepath.Join(source, "home", "Library"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	staging, err := newCopyStagingDir(opened)
	if err != nil {
		t.Fatalf("newCopyStagingDir followed post-open replacement: %v", err)
	}
	if err := cleanupCopyStagingDir(staging); err != nil {
		t.Fatal(err)
	}
}

func TestCopyStagingRejectsCacheInsideOpenedSourceBeforeCreate(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("environment-specific cache-directory test")
	}
	root := t.TempDir()
	source := filepath.Join(root, "tree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	var cache string
	if runtime.GOOS == "darwin" {
		home := filepath.Join(source, "home")
		if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		cache = filepath.Join(home, "Library", "Caches")
	} else {
		cache = filepath.Join(source, "cache")
		t.Setenv("XDG_CACHE_HOME", cache)
	}
	opened, err := openVerifiedCopySource(source, openCopySource)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.file.Close()

	if _, err := newCopyStagingDir(opened); err == nil {
		t.Fatal("cache inside opened source was accepted")
	}
	if _, err := os.Lstat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging cache was created inside source: %v", err)
	}
}

func TestCopyStagingCreatesMissingUserCacheBasePrivately(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("environment-specific cache-directory test")
	}
	root := t.TempDir()
	var cache string
	if runtime.GOOS == "darwin" {
		home := filepath.Join(root, "home")
		if err := os.MkdirAll(filepath.Join(home, "Library"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		cache = filepath.Join(home, "Library", "Caches")
	} else {
		cache = filepath.Join(root, "cache")
		t.Setenv("XDG_CACHE_HOME", cache)
	}

	if _, err := os.Lstat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache precondition failed: %v", err)
	}
	if _, err := prepareCopyStagingRoot(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("fresh cache permissions = %04o, want 0700", info.Mode().Perm())
	}
}
