package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestStreamClassifiesExecutableFormatStartErrorAsSetup(t *testing.T) {
	name, mode := "backend", os.FileMode(0o700)
	if runtime.GOOS == "windows" {
		name, mode = "backend.exe", 0o600
	}
	backend := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(backend, []byte("not an executable format"), mode); err != nil {
		t.Fatal(err)
	}

	stream, err := (&ExecRunner{Binary: backend}).Stream(context.Background(), "logs", "--follow", "x")
	if err == nil {
		_ = stream.Close()
		t.Fatal("Stream unexpectedly started an invalid executable")
	}
	if !errors.Is(err, ErrStreamSetup) {
		t.Fatalf("error = %v, want ErrStreamSetup", err)
	}
	if runtime.GOOS != "windows" && !errors.Is(err, syscall.ENOEXEC) {
		t.Fatalf("error = %v, want ENOEXEC cause", err)
	}
}

func TestStreamClassifiesRelativePermissionStartErrorAsSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce Unix executable permission bits")
	}
	dir, err := os.MkdirTemp(".", ".stream-setup-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	relative := filepath.Join(dir, "backend")
	if err := os.WriteFile(relative, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(relative) {
		t.Fatalf("test backend is not relative: %q", relative)
	}

	stream, err := (&ExecRunner{Binary: relative}).Stream(context.Background(), "logs", "--follow", "x")
	if err == nil {
		_ = stream.Close()
		t.Fatal("Stream unexpectedly started a non-executable relative backend")
	}
	if !errors.Is(err, ErrStreamSetup) {
		t.Fatalf("error = %v, want ErrStreamSetup", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want permission cause", err)
	}
}

func TestStreamClassifiesRelativeMissingStartErrorAsSetup(t *testing.T) {
	relative := filepath.Join("testdata", "missing-stream-backend")
	stream, err := (&ExecRunner{Binary: relative}).Stream(context.Background(), "logs", "--follow", "x")
	if err == nil {
		_ = stream.Close()
		t.Fatal("Stream unexpectedly started a missing relative backend")
	}
	if !errors.Is(err, ErrStreamSetup) {
		t.Fatalf("error = %v, want ErrStreamSetup", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want not-exist cause", err)
	}
}
