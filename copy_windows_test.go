//go:build windows

package container

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsCopyStagingDirectoryHasCurrentUserOnlyDACL(t *testing.T) {
	root, err := prepareCopyStagingRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	file, reparse, err := openCopySource(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if reparse {
		t.Fatal("staging directory opened as a reparse point")
	}
	if err := verifyCurrentUserOnlyDACL(file); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCopySourceRejectsReparsePoint(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Windows symlink unavailable: %v", err)
	}

	_, err := openVerifiedCopySource(link, openCopySource)
	if !errors.Is(err, ErrCopySourceUnsupported) {
		t.Fatalf("error = %v, want ErrCopySourceUnsupported", err)
	}
}
