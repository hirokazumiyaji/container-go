package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestReview91ForLogReturnsTerminalFollowError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell stream fixture is unavailable on Windows")
	}
	script := filepath.Join(t.TempDir(), "docker")
	contents := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'ready\n'
printf 'log stream failed\n' >&2
exit 17
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}

	ctr := &Container{
		id:     "myctr",
		runner: &cli.ExecRunner{Binary: script},
		eng:    dockerEngine{},
	}
	err := wait.ForLog("ready").
		WithStartupTimeout(5*time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err == nil {
		t.Fatal("terminal logs --follow error unexpectedly satisfied the pattern")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *cli.CLIError", err)
	}
	if cliErr.ExitCode != 17 {
		t.Fatalf("ExitCode = %d, want 17", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "log stream failed") {
		t.Fatalf("Stderr = %q, want terminal diagnostic", cliErr.Stderr)
	}
}

func TestReview91FollowLogsClassifiesTerminalNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell stream fixture is unavailable on Windows")
	}
	script := filepath.Join(t.TempDir(), "docker")
	contents := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'Error response from daemon: No such container: myctr\n' >&2
exit 1
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	ctr := &Container{
		id:     "myctr",
		runner: &cli.ExecRunner{Binary: script},
		eng:    dockerEngine{},
	}
	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	defer stream.Close()
	_, readErr := io.ReadAll(stream)
	if !errors.Is(readErr, ErrContainerNotFound) {
		t.Fatalf("read error = %v, want ErrContainerNotFound", readErr)
	}
}
