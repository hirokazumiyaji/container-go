package cli

import (
	"context"
	"fmt"
	"io"
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("container %s: %w", strings.Join(args, " "), err)
	}
	return &processStream{ReadCloser: stdout, cmd: cmd}, nil
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
