package cli

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStreamReadsOutputAndCloseKillsProcess(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `while true; do echo line; sleep 0.05; done`)}

	stream, err := r.Stream(context.Background(), "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	scanner := bufio.NewScanner(stream)
	if !scanner.Scan() {
		t.Fatalf("no output before EOF: %v", scanner.Err())
	}
	if scanner.Text() != "line" {
		t.Errorf("line = %q", scanner.Text())
	}

	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; child process not killed")
	}
}

// docker logs writes the container's stderr to the CLI's stderr; the
// stream must carry both output streams.
func TestStreamMergesStderrIntoStream(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `echo out-line; echo err-line >&2`)}

	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var lines []string
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "out-line") || !strings.Contains(joined, "err-line") {
		t.Errorf("stream = %q, want both stdout and stderr lines", joined)
	}
}

func TestStreamHonorsContextCancellation(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `while true; do echo line; sleep 0.05; done`)}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := r.Stream(ctx, "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	cancel()
	// After cancellation the stream must reach EOF/error promptly
	// instead of blocking forever.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := stream.Read(buf); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stream still open after context cancellation")
		}
	}
}

func TestStreamCloseIsIdempotent(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `while true; do echo line; sleep 0.05; done`)}

	stream, err := r.Stream(context.Background(), "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStreamClassifiesNonExecutableAbsolutePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce Unix executable permission bits")
	}
	backend := filepath.Join(t.TempDir(), "backend")
	if err := os.WriteFile(backend, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}

	stream, err := (&ExecRunner{Binary: backend}).Stream(context.Background(), "logs", "--follow", "x")
	if err == nil {
		_ = stream.Close()
		t.Fatal("Stream unexpectedly started a non-executable backend")
	}
	if !errors.Is(err, ErrStreamSetup) {
		t.Fatalf("error = %v, want ErrStreamSetup", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want underlying permission cause", err)
	}
}
