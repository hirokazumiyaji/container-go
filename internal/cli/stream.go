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

func (r *ExecRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, r.binary(), args...)
	cmd.WaitDelay = 3 * time.Second
	configureProcessGroup(cmd)
	// One pipe carries both output streams: `docker logs` splits the
	// container's stdout/stderr across the CLI's two streams.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	lifecycle := newCommandLifecycle(ctx, cmd)
	cmd.Cancel = lifecycle.terminate
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		lifecycle.failStart()
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("container %s: %w", strings.Join(args, " "), err)
	}
	// The child holds its own copy of the write end; releasing ours
	// lets the reader see EOF when the child exits.
	_ = pw.Close()
	tree, err := newProcessTree(cmd)
	if err != nil {
		tree = directProcessTree{}
	}
	lifecycle.publishStart(tree)
	go lifecycle.wait()
	return &processStream{ReadCloser: pr, cmd: cmd, lifecycle: lifecycle}, nil
}

type processStream struct {
	io.ReadCloser
	cmd       *exec.Cmd
	lifecycle *commandLifecycle
	once      sync.Once
}

func (s *processStream) Close() error {
	s.once.Do(func() {
		if s.lifecycle != nil {
			_ = s.lifecycle.terminate()
		} else if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.ReadCloser.Close()
		if s.lifecycle != nil {
			s.lifecycle.wait()
		} else if s.cmd != nil {
			_ = s.cmd.Wait()
		}
	})
	return nil
}
