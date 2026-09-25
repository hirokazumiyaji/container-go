package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
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

func TestStreamReapsChildAfterExitWithoutReadOrClose(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'line\n'`)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() { _ = stream.Close() })

	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped without Read or Close")
	}
	if got := ps.waitCalls.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want exactly one", got)
	}
}

func TestStreamCancellationReapsAndClosesWithoutClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &ExecRunner{Binary: writeStub(t, `while true; do printf 'line\\n'; done`)}

	stream, err := r.Stream(ctx, "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() { _ = stream.Close() })

	cancel()
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped after cancellation without Close")
	}
	if ps.cmd.ProcessState == nil {
		t.Fatal("child was not waited after cancellation")
	}
	if got := ps.waitCalls.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want exactly one", got)
	}

	if _, err := io.ReadAll(stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want context.Canceled", err)
	}
	select {
	case <-ps.pumpsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("source pipes remained blocked after cancellation")
	}
	if _, err := ps.stdoutRead.Stat(); err == nil {
		t.Fatal("stdout source descriptor remained open after cancellation")
	}
	if _, err := ps.stderrRead.Stat(); err == nil {
		t.Fatal("stderr source descriptor remained open after cancellation")
	}
}

func TestStreamCancellationDuringStartUsesImmutableEndpointOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminateEntered := make(chan struct{})
	releaseTerminate := make(chan struct{})
	r := &ExecRunner{Binary: writeStub(t, `sleep 30`)}
	hooks := streamHooks{
		afterStart: func(*processStream) {
			cancel()
			select {
			case <-terminateEntered:
			case <-time.After(5 * time.Second):
				t.Errorf("context cancellation did not reach process termination")
			}
		},
		terminate: func(cmd *exec.Cmd) error {
			close(terminateEntered)
			<-releaseTerminate
			return killProcessGroup(cmd)
		},
	}

	stream, err := r.stream(ctx, hooks, "logs", "--follow", "x")
	close(releaseTerminate)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped after cancellation during start")
	}
	if _, err := io.ReadAll(stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want context.Canceled", err)
	}
}
