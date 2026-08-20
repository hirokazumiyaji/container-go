package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Streamer starts a long-lived CLI invocation (e.g. `logs --follow`)
// and exposes its stdout as a stream. Closing the stream terminates the
// child process.
type Streamer interface {
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
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
		return nil, fmt.Errorf("container %s: %w", strings.Join(args, " "), err)
	}
	// The child holds its own copy of the write end; releasing ours
	// lets the reader see EOF when the child exits.
	_ = pw.Close()
	return &processStream{ReadCloser: pr, cmd: cmd}, nil
}

type processStream struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (s *processStream) Close() error {
	_ = s.cmd.Process.Kill()
	_ = s.ReadCloser.Close()
	// Reap the child; the error is the expected kill signal.
	_ = s.cmd.Wait()
	return nil
}
