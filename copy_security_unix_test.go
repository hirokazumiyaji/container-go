//go:build darwin || linux

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
	ctr := runTestContainer(t, f)

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
	ctr := runTestContainer(t, f)

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
	ctr := runTestContainer(t, f)

	rc, err := ctr.CopyFileFromContainer(context.Background(), "/container/socket")
	if listener != nil {
		defer listener.Close()
	}
	if socketErr != nil {
		t.Skipf("Unix socket creation unavailable: %v", socketErr)
	}
	if err == nil {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatal("Unix socket was accepted")
	}
	if !errors.Is(err, ErrCopyFileNotRegular) {
		t.Errorf("Unix socket error = %v, want ErrCopyFileNotRegular", err)
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
	ctr := runTestContainer(t, f)

	if _, err := ctr.CopyFileFromContainer(context.Background(), "/container/fifo"); err == nil {
		t.Fatal("FIFO was accepted")
	}
	if dst == "" {
		t.Fatal("copy destination was not recorded")
	}
	if _, err := os.Stat(filepath.Dir(dst)); !os.IsNotExist(err) {
		t.Errorf("private copy directory %q remains after rejection", filepath.Dir(dst))
	}
}
