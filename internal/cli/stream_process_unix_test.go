//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStreamCloseKillsDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	stub := writeStub(t, `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; wait "$child"`)
	stream, err := (&ExecRunner{Binary: stub}).Stream(context.Background(), pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	descendantPID := 0
	t.Cleanup(func() {
		_ = stream.Close()
		if descendantPID > 1 {
			if p, findErr := os.FindProcess(descendantPID); findErr == nil {
				_ = p.Kill()
			}
		}
	})
	descendantPID = waitForDescendantPID(t, pidFile)

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped after Close")
	}
	assertStreamProcessReaped(t, descendantPID)
}

func TestStreamCloseKillsDescendantsAfterParentExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	stub := writeStub(t, `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; exit 0`)
	stream, err := (&ExecRunner{Binary: stub}).Stream(context.Background(), pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	descendantPID := 0
	t.Cleanup(func() {
		_ = stream.Close()
		if descendantPID > 1 {
			if p, findErr := os.FindProcess(descendantPID); findErr == nil {
				_ = p.Kill()
			}
		}
	})
	descendantPID = waitForDescendantPID(t, pidFile)
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped")
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertStreamProcessReaped(t, descendantPID)
}

func TestStreamCancellationKillsDescendantsWithoutReadOrClose(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	stub := writeStub(t, `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; wait "$child"`)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := (&ExecRunner{Binary: stub}).Stream(ctx, pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	descendantPID := 0
	t.Cleanup(func() {
		_ = stream.Close()
		if descendantPID > 1 {
			if p, findErr := os.FindProcess(descendantPID); findErr == nil {
				_ = p.Kill()
			}
		}
	})
	descendantPID = waitForDescendantPID(t, pidFile)

	cancel()
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped after cancellation")
	}
	assertStreamProcessReaped(t, descendantPID)
}

func waitForDescendantPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant PID was not written to %s", path)
	return 0
}
