//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func TestIssue90ForLogClassifiesRealTerminalFollowError(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	contents := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'ready\n'
printf 'logs stream failed\n' >&2
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
	if !strings.Contains(cliErr.Stderr, "logs stream failed") {
		t.Fatalf("Stderr = %q, want terminal diagnostic", cliErr.Stderr)
	}
	if errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, generic CLI failure was misclassified as not-found", err)
	}
}

func TestIssue90ForLogRetainsTerminalStderrBeforeNotFoundClassification(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	contents := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'ready\n'
head -c 70000 /dev/zero >&2
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
	err := wait.ForLog("ready").
		WithStartupTimeout(5*time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err == nil {
		t.Fatal("terminal not-found error unexpectedly satisfied the log pattern")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *cli.CLIError", err)
	}
	if !strings.Contains(cliErr.Stderr, "No such container: myctr") {
		t.Fatalf("Stderr = %q, want terminal not-found diagnostic", cliErr.Stderr)
	}
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
}

func TestIssue90ForLogConsumesPendingOutputBeforeAcceptingMatch(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	contents := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'ready\n'
i=0
while [ "$i" -lt 4000 ]; do
  printf 'noise-%04d\\n' "$i" >&2
  i=$((i+1))
done
sleep 0.05
printf 'Error response from daemon: No such container: myctr\\n' >&2
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
	err := wait.ForLog("ready").
		WithStartupTimeout(5*time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err == nil {
		t.Fatal("ForLog returned success before observing the terminal failure")
	}
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *cli.CLIError", err)
	}
	if !strings.Contains(cliErr.Stderr, "No such container: myctr") {
		t.Fatalf("Stderr = %q, want final not-found marker", cliErr.Stderr)
	}
}

func TestIssue90FollowLogsClassifiesTerminalNotFound(t *testing.T) {
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
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %v, want *CLIError", readErr)
	}
	if !strings.Contains(cliErr.Stderr, "No such container: myctr") {
		t.Fatalf("Stderr = %q, want not-found diagnostic", cliErr.Stderr)
	}
}
