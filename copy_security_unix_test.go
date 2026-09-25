//go:build darwin || freebsd || linux || netbsd || openbsd

package container

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCopyFileFromContainerRejectsFIFOWithoutOpeningIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)

	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(dst string) error {
			return syscall.Mkfifo(dst, 0o600)
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	result := make(chan error, 1)
	go func() {
		_, err := ctr.CopyFileFromContainer(context.Background(), "/container/fifo")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO was accepted")
		}
		if !errors.Is(err, ErrCopyFileNotRegular) {
			t.Errorf("FIFO error = %v, want ErrCopyFileNotRegular", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CopyFileFromContainer blocked while opening a FIFO")
	}
}

func TestCopyFileFromContainerHonorsCanceledContextBeforeOpen(t *testing.T) {
	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(dst string) error {
			return syscall.Mkfifo(dst, 0o600)
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	go func() {
		_, err := ctr.CopyFileFromContainer(ctx, "/container/fifo")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled copy unexpectedly succeeded")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled copy error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled copy blocked while opening a FIFO")
	}
}

func TestCopyFileFromContainerRejectsUnixSocket(t *testing.T) {
	var listener *net.UnixListener
	var socketErr error
	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(dst string) error {
			listener, socketErr = net.ListenUnix("unix", &net.UnixAddr{Name: dst, Net: "unix"})
			return socketErr
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/container/socket")
	if listener != nil {
		defer listener.Close()
	}
	if socketErr != nil {
		if rc != nil {
			_ = rc.Close()
		}
		t.Skipf("Unix socket creation unavailable: %v", socketErr)
	}
	if rc != nil {
		_ = rc.Close()
		t.Fatal("Unix socket returned a reader")
	}
	if err == nil {
		t.Fatal("Unix socket was accepted")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Errorf("Unix socket error = %v, want ErrCopyFileNotRegular", err)
	}
}

// Device nodes require a privileged or otherwise capable host to
// materialize, so this test skips explicitly when a hard link cannot be
// created for the fake runner.
func TestCopyFileFromContainerRejectsDeviceWhenHostAllowsIt(t *testing.T) {
	const devicePath = "/dev/null"
	probe := filepath.Join(t.TempDir(), "device")
	if err := os.Link(devicePath, probe); err != nil {
		t.Skipf("device-node materialization unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("remove device probe: %v", err)
	}

	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(dst string) error {
			return os.Link(devicePath, dst)
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/container/device")
	if rc != nil {
		_ = rc.Close()
		t.Fatal("device node returned a reader")
	}
	if err == nil {
		t.Fatal("device node was accepted")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Errorf("device error = %v, want ErrCopyFileNotRegular", err)
	}
}

func TestCopyFileFromContainerRemovesPrivateDirectoryAfterRejectedTarget(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)

	var dst string
	f := &cpRunner{
		fakeRunner: newTestRunner(),
		materialize: func(path string) error {
			dst = path
			return syscall.Mkfifo(path, 0o600)
		},
	}
	ctr := runCopyDockerTestContainer(t, f)

	_, err := ctr.CopyFileFromContainer(context.Background(), "/container/fifo")
	if err == nil {
		t.Fatal("FIFO was accepted")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Errorf("FIFO cleanup error = %v, want ErrCopyFileNotRegular", err)
	}
	if dst == "" {
		t.Fatal("copy destination was not recorded")
	}
	if _, err := os.Stat(filepath.Dir(dst)); !os.IsNotExist(err) {
		t.Errorf("private copy directory %q remains after rejection", filepath.Dir(dst))
	}
}

func TestOpenCopyFileRejectsSymlinkReplacementAfterLstat(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	target := filepath.Join(dir, "host-secret")
	if err := os.WriteFile(payload, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, payload); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	f, err := openCopyFile(payload)
	if f != nil {
		_ = f.Close()
		t.Fatal("open followed a symlink replaced after Lstat")
	}
	if err == nil {
		t.Fatal("open unexpectedly accepted a symlink")
	}
}

func TestOpenCopyFileDoesNotBlockOnFIFOReplacementAfterLstat(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	if err := os.WriteFile(payload, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(payload, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		file *os.File
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		f, err := openCopyFile(payload)
		resultCh <- result{file: f, err: err}
	}()

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("open FIFO: %v", got.err)
		}
		if got.file == nil {
			t.Fatal("open FIFO returned a nil file")
		}
		info, err := got.file.Stat()
		_ = got.file.Close()
		if err != nil {
			t.Fatalf("stat FIFO: %v", err)
		}
		if info.Mode().IsRegular() {
			t.Fatal("FIFO descriptor was reported as a regular file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openCopyFile blocked on a FIFO replacement")
	}
}
