//go:build windows && !go1.26

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsCopyFileOpenFailsClosedBeforeGo126(t *testing.T) {
	if err := checkCopyFileOpenCapability(); !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
		t.Fatalf("capability error = %v, want ErrCopyFileFromContainerUnsupported", err)
	}

	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte("must not be opened"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openCopyFile(payload)
	if f != nil {
		_ = f.Close()
		t.Fatal("openCopyFile returned a file on an unsafe Windows toolchain")
	}
	if !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
		t.Fatalf("openCopyFile error = %v, want ErrCopyFileFromContainerUnsupported", err)
	}

	runner := &cpRunner{fakeRunner: newTestRunner(), fileContent: "must not be copied"}
	ctr := runCopyDockerTestContainer(t, runner)
	if _, err := ctr.CopyFileFromContainer(context.Background(), "/out/result.txt"); !errors.Is(err, ErrCopyFileFromContainerUnsupported) {
		t.Fatalf("CopyFileFromContainer error = %v, want ErrCopyFileFromContainerUnsupported", err)
	}
	if call := runner.callWith("cp"); call != nil {
		t.Fatalf("unsafe Windows toolchain invoked copy-out CLI: %v", call)
	}
}
