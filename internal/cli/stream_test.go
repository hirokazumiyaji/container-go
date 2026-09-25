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

const (
	terminalErrorExitHelperEnv     = "CONTAINER_GO_TERMINAL_ERROR_EXIT_HELPER"
	terminalErrorExitCodeHelperEnv = "CONTAINER_GO_TERMINAL_ERROR_EXIT_CODE"
)

func terminalErrorExitError(t *testing.T) *exec.ExitError {
	return terminalErrorExitErrorWithCode(t, 17)
}

func terminalErrorExitErrorWithCode(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalErrorExitHelper$")
	cmd.Env = append(os.Environ(),
		terminalErrorExitHelperEnv+"=1",
		terminalErrorExitCodeHelperEnv+"="+strconv.Itoa(code),
	)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper exit error = %v, want *exec.ExitError", err)
	}
	return exitErr
}

func TestTerminalErrorExitHelper(t *testing.T) {
	if os.Getenv(terminalErrorExitHelperEnv) == "1" {
		code := 17
		if value := os.Getenv(terminalErrorExitCodeHelperEnv); value != "" {
			var err error
			code, err = strconv.Atoi(value)
			if err != nil {
				os.Exit(2)
			}
		}
		os.Exit(code)
	}
}

func TestTerminalErrorResultJoinsContextAndExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:    ctx,
		binary: "docker",
		args:   []string{"logs", "--follow", "x"},
		stderr: &tailBuffer{},
	}
	ps.drainCompleted.Store(true)
	ps.stateMu.Lock()
	ps.cancelled = true
	ps.stateMu.Unlock()
	err := ps.terminalError(terminalErrorExitError(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TerminalError = %v, want context.Canceled", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("TerminalError = %v, want settled exit CLIError", err)
	}
}

func TestTerminalErrorDrainsStderrWhenPublicReaderIsBlocked(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `printf 'ready\n'; head -c 70000 /dev/zero >&2; printf 'TERMINAL_STDERR_MARKER\n' >&2; exit 17`)}

	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- ps.TerminalError() }()
	var cliErr *CLIError
	select {
	case err = <-errCh:
		if !errors.As(err, &cliErr) {
			t.Fatalf("TerminalError = %v, want *CLIError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TerminalError blocked on a public-stream backpressure pump")
	}
	if !strings.Contains(cliErr.Stderr, "TERMINAL_STDERR_MARKER") {
		t.Fatalf("Stderr = %q, want final terminal marker", cliErr.Stderr)
	}
	if !ps.drainCompleted.Load() {
		t.Fatal("TerminalError returned before drain completion")
	}
	select {
	case <-ps.pumpsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("output pumps did not finish after TerminalError drain")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDrainTimeoutWaitsForPumpsBeforeReturning(t *testing.T) {
	sourceRead, sourceWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	publicRead, publicWrite := io.Pipe()
	defer func() {
		_ = sourceWrite.Close()
		_ = publicRead.Close()
		_ = publicWrite.Close()
	}()

	ps := &processStream{
		ReadCloser: publicRead,
		output:     publicWrite,
		stderrRead: sourceRead,
		pumpsDone:  make(chan struct{}),
	}
	go func() {
		_, _ = io.Copy(io.Discard, sourceRead)
		time.Sleep(25 * time.Millisecond)
		close(ps.pumpsDone)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := ps.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain error = %v, want caller deadline", err)
	}
	select {
	case <-ps.pumpsDone:
	case <-time.After(10 * time.Millisecond):
		t.Fatal("Drain returned before pumpsDone after closing sources")
	}
	if !ps.drainCompleted.Load() {
		t.Fatal("Drain returned without recording completion")
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
	r := &ExecRunner{Binary: writeStub(t, `printf 'terminal failure\n' >&2; exit 17`)}
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
	terminal := ps.TerminalError()
	var cliErr *CLIError
	if !errors.As(terminal, &cliErr) || cliErr.ExitCode != 17 {
		t.Fatalf("TerminalError = %v, want settled exit 17", terminal)
	}

	// Simulate a context callback and a caller Close arriving after the
	// direct child has been waited. Neither may signal the old PID/PGID.
	cancel()
	if err := ps.requestTermination(true); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("late cancellation error = %v, want os.ErrProcessDone", err)
	}
	if got := ps.TerminalError(); got != terminal {
		t.Fatalf("TerminalError after cancellation = %v, want cached %v", got, terminal)
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

func TestTerminalErrorRetainsSettledProcessErrorWithoutPositiveTermination(t *testing.T) {
	exitErr := terminalErrorExitError(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:                 ctx,
		binary:              "docker",
		args:                []string{"logs", "--follow", "x"},
		stderr:              &tailBuffer{},
		cancelled:           true,
		terminationSignaled: false,
	}
	ps.drainCompleted.Store(true)
	err := ps.terminalError(exitErr)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != exitErr.ExitCode() {
		t.Fatalf("terminal error = %v, want settled exit %d CLIError", err, exitErr.ExitCode())
	}

	processErr := errors.New("wait failed after cancellation callback")
	ps.waitErr = processErr
	err = ps.terminalError(processErr)
	if !errors.Is(err, processErr) {
		t.Fatalf("terminal error = %v, want settled process error", err)
	}
}

func TestTerminalErrorReturnsContextOnlyAfterPositiveTermination(t *testing.T) {
	exitErr := terminalErrorExitError(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:                 ctx,
		binary:              "docker",
		args:                []string{"logs", "--follow", "x"},
		stderr:              &tailBuffer{},
		cancelled:           true,
		terminationSignaled: true,
	}
	ps.drainCompleted.Store(true)
	err := ps.terminalError(exitErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal error = %v, want context cancellation", err)
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		t.Fatalf("terminal error = %v, unexpectedly retained CLIError", err)
	}
}

func TestTerminalErrorDoesNotTurnSuccessfulSettledProcessIntoCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:       ctx,
		binary:    "docker",
		args:      []string{"logs", "--follow", "x"},
		stderr:    &tailBuffer{},
		cancelled: true,
	}
	ps.drainCompleted.Store(true)
	if err := ps.terminalError(nil); err != nil {
		t.Fatalf("terminal error = %v, want successful settled process", err)
	}
}

func TestStreamPreservesChildStdoutStderrChronology(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ordering fixture requires a POSIX shell")
	}
	binary := writeStub(t, `printf 'first\\n'; sleep 0.02; printf 'second\\n' >&2; printf 'third\\n'; sleep 0.02; printf 'fourth\\n' >&2`)
	stream, err := (&ExecRunner{Binary: binary}).Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got, want := string(data), "first\\nsecond\\nthird\\nfourth\\n"; got != want {
		t.Fatalf("stream chronology = %q, want %q", got, want)
	}
}

func assertStreamDirectChildReaped(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the process-state probe uses Unix ps; Windows tests use the retained process handle")
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
