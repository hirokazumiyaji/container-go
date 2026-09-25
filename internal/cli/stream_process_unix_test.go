//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForSinglePID(t *testing.T, path, errorPath string) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if pid, err := readPID(path); err == nil {
			return pid, true
		}
		if _, err := os.Stat(errorPath); err == nil {
			return 0, false
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID file %q was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("PID file %q = %q", path, data)
	}
	return pid, nil
}

func TestStreamEOFBoundsDescendantPipeRetentionAfterChildExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	r := &ExecRunner{Binary: writeStub(t, `(sleep 5) & echo $! > "$3"; printf 'parent\\n'`)}
	stream, err := r.Stream(context.Background(), "logs", "x", pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	descendantPID, available := waitForSinglePID(t, pidFile, filepath.Join(filepath.Dir(pidFile), "missing.error"))
	if !available {
		t.Fatal("descendant PID file was not created")
	}
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })

	started := time.Now()
	data, readErr := io.ReadAll(stream)
	if readErr != nil {
		t.Fatalf("ReadAll: %v", readErr)
	}
	if !strings.Contains(string(data), "parent") {
		t.Fatalf("stream data = %q, want parent output", data)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stream EOF took %v, want bounded drain after direct child exit", elapsed)
	}
}
