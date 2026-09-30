package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Streamer starts a long-lived CLI invocation (e.g. `logs --follow`)
// and exposes its stdout as a stream. Closing the stream terminates the
// child process.
type Streamer interface {
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// SnapshotStreamer is the bounded-output counterpart used for diagnostics.
// It exposes both backend output streams without first materializing them in
// a []byte returned by Runner.Run.
type SnapshotStreamer interface {
	StreamSnapshot(ctx context.Context, args ...string) (io.ReadCloser, error)
}

func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	return r.stream(ctx, false, args...)
}

// StreamSnapshot starts a finite CLI invocation and exposes its merged output
// as a stream. At EOF, Read returns the child process's Wait error, if any. The
// caller must still close the stream; an explicit Close terminates and reaps
// the child while suppressing the expected termination status.
func (r *ExecRunner) StreamSnapshot(ctx context.Context, args ...string) (io.ReadCloser, error) {
	return r.stream(ctx, true, args...)
}

func (r *ExecRunner) stream(ctx context.Context, finite bool, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, r.binary(), args...)
	cmd.WaitDelay = 3 * time.Second
	// One pipe carries both output streams: `docker logs` splits the
	// container's stdout/stderr across the CLI's two streams.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("container %s: %w", strings.Join(NewRedactor().Args(args), " "), err)
	}
	// The child holds its own copy of the write end; releasing ours
	// lets the reader see EOF when the child exits.
	_ = pw.Close()
	return &processStream{ReadCloser: pr, cmd: cmd, finite: finite}, nil
}

type processStream struct {
	io.ReadCloser
	cmd     *exec.Cmd
	finite  bool
	once    sync.Once
	wait    sync.Once
	waitErr error
}

func (s *processStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err == io.EOF && s.finite {
		if waitErr := s.waitForChild(); waitErr != nil {
			err = waitErr
		}
	}
	return n, err
}

func (s *processStream) waitForChild() error {
	s.wait.Do(func() {
		s.waitErr = s.cmd.Wait()
	})
	return s.waitErr
}

func (s *processStream) Close() error {
	s.once.Do(func() {
		_ = s.cmd.Process.Kill()
		_ = s.ReadCloser.Close()
		// Reap the child. An explicit Close is the termination path, so the
		// expected kill status is intentionally suppressed.
		_ = s.waitForChild()
	})
	return nil
}
