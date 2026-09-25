//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
