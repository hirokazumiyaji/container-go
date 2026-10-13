//go:build windows

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
)

const (
	issue116WindowsHelperEnv = "CONTAINERGO_ISSUE116_WINDOWS_HELPER"
	issue116WindowsReadyEnv  = "CONTAINERGO_ISSUE116_WINDOWS_READY"
)

type issue116WindowsHelperRunner struct {
	runner cli.Runner
}

func (r *issue116WindowsHelperRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	helperArgs := []string{"-test.run=^TestExecWindowsKillHelper$", "--"}
	return r.runner.Run(ctx, append(helperArgs, args...)...)
}

func TestExecWindowsProcessKillReturnsTerminationError(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv(issue116WindowsHelperEnv, "1")
	t.Setenv(issue116WindowsReadyEnv, ready)

	runner := &issue116WindowsHelperRunner{runner: &cli.ExecRunner{Binary: os.Args[0]}}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()

	waitForIssue116WindowsHelper(t, ready, result)
	cancel()
	got := waitForIssue116ExecResult(t, result)

	if got.code != 1 {
		t.Fatalf("exit code = %d, want Windows Process.Kill status 1", got.code)
	}
	var cliErr *CLIError
	if !errors.As(got.err, &cliErr) || cliErr.ExitCode != 1 {
		t.Fatalf("error = %v, want local CLI exit status 1", got.err)
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on cancellation")
	}
	data, err := io.ReadAll(got.out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "windows stdout") || !strings.Contains(string(data), "windows stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func waitForIssue116WindowsHelper(t *testing.T, ready string, result <-chan issue116ExecResult) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat helper ready file: %v", err)
		}
		select {
		case got := <-result:
			t.Fatalf("Exec returned before the helper started: code=%d output=%v error=%v", got.code, got.out, got.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("Windows exec helper did not start")
}

func TestExecWindowsKillHelper(t *testing.T) {
	if os.Getenv(issue116WindowsHelperEnv) == "" {
		return
	}
	if _, err := os.Stdout.WriteString("windows stdout"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stderr.WriteString("windows stderr"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(issue116WindowsReadyEnv), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}
