package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

func TestStreamReturnsTerminalExitErrorAndStderr(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'log line\n'; printf 'terminal stderr\n' >&2; exit 17`)}

	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	data, readErr := io.ReadAll(stream)
	if !strings.Contains(string(data), "log line") {
		t.Errorf("stream data = %q, want log output", data)
	}
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %v, want *CLIError", readErr)
	}
	if cliErr.ExitCode != 17 {
		t.Errorf("ExitCode = %d, want 17", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "terminal stderr") {
		t.Errorf("Stderr = %q, want terminal diagnostic", cliErr.Stderr)
	}
}

func TestStreamRetainsTerminalStderrTail(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `head -c 70000 /dev/zero >&2; printf 'Error response from daemon: No such container: terminal\n' >&2; exit 1`)}

	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, readErr := io.ReadAll(stream)
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %v, want *CLIError", readErr)
	}
	if len(cliErr.Stderr) > maxStderr {
		t.Errorf("len(Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderr)
	}
	if !strings.Contains(cliErr.Stderr, "No such container: terminal") {
		t.Errorf("Stderr = %q, want terminal not-found diagnostic", cliErr.Stderr)
	}
}

func TestStreamReturnsTerminalSignalError(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'signal stderr\n' >&2; kill -TERM $$`)}

	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, readErr := io.ReadAll(stream)
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %v, want *CLIError", readErr)
	}
	if cliErr.ExitCode >= 0 {
		t.Errorf("ExitCode = %d, want signal exit", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "signal stderr") {
		t.Errorf("Stderr = %q, want signal diagnostic", cliErr.Stderr)
	}
}

func TestStreamReapsChildAfterExitWithoutReadOrClose(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'line\n'`)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	pid := ps.cmd.Process.Pid
	t.Cleanup(func() { _ = stream.Close() })

	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped without a Read or Close")
	}
	if got := ps.waitCalls.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want exactly one", got)
	}
	assertStreamDirectChildReaped(t, pid)
}

func TestStreamReapsChildAfterCancellationWithoutClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &ExecRunner{Binary: writeStub(t, `while true; do printf 'line\n'; done`)}
	stream, err := r.Stream(ctx, "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	pid := ps.cmd.Process.Pid
	t.Cleanup(func() { _ = stream.Close() })

	cancel()
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped after cancellation without a Read or Close")
	}
	assertStreamDirectChildReaped(t, pid)
	_, readErr := io.ReadAll(stream)
	if !errors.Is(readErr, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want context.Canceled", readErr)
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
			return terminateProcessTree(cmd)
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

func TestStreamDelayedCloseAndCancelDoNotSignalAfterReap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signaled := make(chan struct{}, 1)
	r := &ExecRunner{Binary: writeStub(t, `exit 0`)}
	hooks := streamHooks{
		terminate: func(*exec.Cmd) error {
			signaled <- struct{}{}
			return nil
		},
	}
	stream, err := r.stream(ctx, hooks, "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped")
	}

	// Simulate a context callback and a caller Close arriving after the
	// direct child has been waited. Neither may signal the old PID/PGID.
	cancel()
	if err := ps.requestTermination(true); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("late cancellation error = %v, want os.ErrProcessDone", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-signaled:
		t.Fatal("late lifecycle operation signaled a reaped process")
	default:
	}
}

func TestStreamLifecyclePathsDoNotDoubleWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &ExecRunner{Binary: writeStub(t, `printf 'line\n'; sleep 0.1; exit 0`)}
	stream, err := r.Stream(ctx, "logs", "--follow", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	pid := ps.cmd.Process.Pid
	t.Cleanup(func() { _ = stream.Close() })

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			_ = stream.Close()
		}()
		go func() {
			defer wg.Done()
			<-start
			cancel()
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = stream.Read(make([]byte, 32))
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent lifecycle operations did not finish")
	}
	if ps.cmd.ProcessState == nil {
		t.Fatal("child was not waited")
	}
	if got := ps.waitCalls.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want exactly one", got)
	}
	assertStreamDirectChildReaped(t, pid)
}

func assertStreamDirectChildReaped(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the process-state probe uses Unix ps; Windows termination is guarded by taskkill")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d was not reaped", pid)
}
