package cli

import (
	"bufio"
	"context"
	"errors"
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

func TestReviewStreamReapsBeforePublicOutputIsRead(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `i=0; while [ "$i" -lt 32 ]; do printf 'line-%s\\n' "$i"; i=$((i + 1)); done; exit 7`)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatal(err)
	}
	process, ok := stream.(interface {
		Done() <-chan struct{}
		TerminalError() error
	})
	if !ok {
		t.Fatalf("stream type %T does not expose process status", stream)
	}
	select {
	case <-process.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process was not reaped while public output was unread")
	}
	var cliErr *CLIError
	if err := process.TerminalError(); !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("terminal error = %v, want exit 7", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
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
