package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecRunnerRunWithStdinCopiesInput(t *testing.T) {
	requirePOSIXShell(t)
	r := &ExecRunner{Binary: writeStub(t, `cat`)}

	var stdout, stderr bytes.Buffer
	stats, err := r.RunWithStdin(context.Background(), strings.NewReader("hello\n"), &stdout, &stderr, "exec")
	if err != nil {
		t.Fatalf("RunWithStdin: %v", err)
	}
	if got, want := stdout.String(), "hello\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := stats.StdoutBytes; got != int64(len("hello\n")) {
		t.Errorf("StdoutBytes = %d, want %d", got, len("hello\n"))
	}
}

func TestExecRunnerRunWithStdinLeavesStdinClosedWhenUnset(t *testing.T) {
	requirePOSIXShell(t)
	r := &ExecRunner{Binary: writeStub(t, `cat`)}

	var stdout bytes.Buffer
	if _, err := r.RunTo(context.Background(), &stdout, io.Discard, "exec"); err != nil {
		t.Fatalf("RunTo: %v", err)
	}
	if got := stdout.String(); got != "" {
		t.Errorf("stdout = %q, want empty input", got)
	}
}

func TestExecRunnerRunWithStdinCancellationClosesBlockingReader(t *testing.T) {
	requirePOSIXShell(t)
	// The child does not consume stdin, so the copy goroutine is blocked on
	// the caller reader. Cancellation must still terminate the child and
	// release the copy goroutine by closing the closable caller reader.
	r := &ExecRunner{Binary: writeStub(t, `sleep 5`)}
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := r.RunWithStdin(ctx, reader, io.Discard, io.Discard, "exec")
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunWithStdin error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunWithStdin did not return after cancellation")
	}
	if _, err := writer.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("pipe write after cancellation = %v, want ErrClosedPipe (reader was not closed)", err)
	}
}

func TestExecRunnerRunWithStdinChildExitClosesBlockingReader(t *testing.T) {
	requirePOSIXShell(t)
	// The child exits without reading stdin, so the copy goroutine is blocked
	// on the caller reader. Completion must close the closable reader rather
	// than leaving that goroutine behind.
	r := &ExecRunner{Binary: writeStub(t, `exit 0`)}
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := r.RunWithStdin(ctx, reader, io.Discard, io.Discard, "exec")
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("RunWithStdin: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWithStdin did not return after the child exited")
	}
	if _, err := writer.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("pipe write after completion = %v, want ErrClosedPipe (reader was not closed)", err)
	}
}

// blockingStdinReader blocks every Read until release closes. It deliberately
// does not implement io.Closer, so the library cannot interrupt a pending
// read.
type blockingStdinReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingStdinReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func TestExecRunnerRunWithStdinNonClosableReaderDoesNotBlockCancellation(t *testing.T) {
	requirePOSIXShell(t)
	r := &ExecRunner{Binary: writeStub(t, `sleep 5`)}
	reader := &blockingStdinReader{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(reader.release) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := r.RunWithStdin(ctx, reader, io.Discard, io.Discard, "exec")
		result <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stdin copy did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunWithStdin error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWithStdin did not return while a non-closable read was blocked")
	}
}

type failingStdinReader struct {
	data []byte
	err  error
}

func (r *failingStdinReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestExecRunnerRunWithStdinReturnsReaderError(t *testing.T) {
	requirePOSIXShell(t)
	readErr := errors.New("stdin source failed")
	r := &ExecRunner{Binary: writeStub(t, `cat`)}

	var stdout bytes.Buffer
	_, err := r.RunWithStdin(context.Background(),
		&failingStdinReader{data: []byte("partial"), err: readErr},
		&stdout, io.Discard, "exec")
	if !errors.Is(err, ErrStdinRead) {
		t.Fatalf("RunWithStdin error = %v, want ErrStdinRead", err)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("RunWithStdin error = %v, want the original reader error", err)
	}
	var stdinErr *StdinReadError
	if !errors.As(err, &stdinErr) {
		t.Fatalf("RunWithStdin error = %v, want *StdinReadError", err)
	}
	if got, want := stdout.String(), "partial"; got != want {
		t.Errorf("stdout = %q, want the data read before the failure", got)
	}
}

type runStdinTestRunner struct {
	Runner
}

func TestRunWithStdinRejectsRunnerWithoutStdinSupport(t *testing.T) {
	r := &runStdinTestRunner{Runner: &ExecRunner{Binary: "ignored"}}
	_, err := RunWithStdin(r, context.Background(), strings.NewReader("x"), io.Discard, io.Discard, "exec")
	if !errors.Is(err, ErrStdinUnsupported) {
		t.Fatalf("RunWithStdin error = %v, want ErrStdinUnsupported", err)
	}
}
