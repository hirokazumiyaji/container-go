//go:build windows && go1.26

package container

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsCopyFileOpenIsAvailableFromGo126(t *testing.T) {
	if err := checkCopyFileOpenCapability(); err != nil {
		t.Fatalf("checkCopyFileOpenCapability: %v", err)
	}

	runner := &cpRunner{fakeRunner: newTestRunner(), fileContent: "copied safely"}
	ctr := runCopyDockerTestContainer(t, runner)
	rc, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt")
	if err != nil {
		t.Fatalf("CopyFileFromContainer: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "copied safely" {
		t.Errorf("copied content = %q, want %q", data, "copied safely")
	}
}

func TestWindowsCopyFileOpenDoesNotFollowSymlinkFromGo126(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "host-secret")
	payload := filepath.Join(dir, "payload")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, payload); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	f, err := openCopyFile(payload)
	if err != nil {
		// Some Windows filesystems reject opening a reparse point itself.
		// Rejecting the open is also the safe outcome.
		return
	}
	info, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil {
		// An error also prevents the returned handle from being consumed.
		return
	}
	if info.Mode().IsRegular() {
		t.Fatal("openCopyFile followed a symlink on a supported Windows toolchain")
	}
}
