//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type waitDelayExecRunner struct {
	*fakeRunner
	command *cli.ExecRunner
}

func (r *waitDelayExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		return r.command.Run(ctx, args...)
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecWaitDelayRedactsCommandAndPreservesDiagnostics(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 5 &\nprintf 'backend diagnostic password=stderrsecret\\n' >&2\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &waitDelayExecRunner{fakeRunner: newTestRunner(), command: &cli.ExecRunner{Binary: stub, Identity: "container"}}
	ctr := runTestContainer(t, runner, withEngine(appleEngine{}))
	_, _, err := ctr.Exec(context.Background(), []string{"sh", "-c", "mysql --password=hunter2 -e 'select 1'"})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Exec error=%v, want ErrWaitDelay", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "stderrsecret") {
		t.Fatalf("secret leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "backend diagnostic") {
		t.Fatalf("missing stderr diagnostic: %v", err)
	}
}
