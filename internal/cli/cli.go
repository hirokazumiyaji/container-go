// Package cli executes the Apple Container CLI (`container`) and
// classifies its failures.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxStderr bounds the stderr captured into a CLIError.
const maxStderr = 64 * 1024

// ErrSystemNotRunning reports that the Apple Container system service
// (container-apiserver) is not running.
var ErrSystemNotRunning = errors.New("apple container system service is not running: run `container system start`")

// Runner executes one `container` CLI invocation.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}

// CLIError is a non-zero exit from the `container` CLI.
type CLIError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *CLIError) Error() string {
	msg := fmt.Sprintf("container %s: exit code %d", strings.Join(e.Args, " "), e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + strings.TrimSpace(e.Stderr)
	}
	return msg
}

// ExecRunner runs the CLI as a child process. Arguments are passed as an
// argv vector; no shell is involved.
type ExecRunner struct {
	// Binary is the CLI executable. Empty means "container" resolved
	// from PATH.
	Binary string
}

func (r *ExecRunner) binary() string {
	if r.Binary == "" {
		return "container"
	}
	return r.Binary
}

func (r *ExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, r.binary(), args...)
	var stdout bytes.Buffer
	stderr := &boundedBuffer{max: maxStderr}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	// If the process ignores the kill long enough to hold pipes open,
	// give up waiting shortly after.
	cmd.WaitDelay = 3 * time.Second

	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("container %s: %w", strings.Join(args, " "), ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), stderr.Bytes(), &CLIError{
				Args:     args,
				ExitCode: exitErr.ExitCode(),
				Stderr:   stderr.String(),
			}
		}
		return stdout.Bytes(), stderr.Bytes(), err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// Classify augments a failed CLI call: if the system service does not
// answer a status probe, the failure is reported as ErrSystemNotRunning
// instead of the original error.
func Classify(ctx context.Context, r Runner, err error) error {
	if err == nil {
		return nil
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	if _, _, probeErr := r.Run(ctx, "system", "status"); probeErr != nil {
		return fmt.Errorf("%w (underlying error: %v)", ErrSystemNotRunning, err)
	}
	return err
}

// boundedBuffer keeps at most max bytes and discards the rest.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *boundedBuffer) String() string { return b.buf.String() }
