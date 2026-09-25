//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

const (
	terminateProcessTreeHelperEnv = "CONTAINER_GO_TERMINATE_PROCESS_TREE_HELPER"
	processHelperTimeout          = 2 * time.Second
)

func waitProcessHelper(done <-chan struct{}) error {
	timer := time.NewTimer(processHelperTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errors.New("process helper wait timed out")
	}
}

func stopProcessHelper(cmd *exec.Cmd, waitDone <-chan struct{}) error {
	select {
	case <-waitDone:
		return nil
	default:
	}

	killResult := make(chan error, 1)
	go func() { killResult <- cmd.Process.Kill() }()
	var cleanupErr error
	select {
	case err := <-killResult:
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("kill helper: %w", err))
		}
	case <-time.After(processHelperTimeout):
		cleanupErr = errors.Join(cleanupErr, errors.New("kill helper timed out"))
	}
	if err := waitProcessHelper(waitDone); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func TestTerminateProcessTreeUsesRetainedProcessHandle(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminateProcessTreeHelper$")
	cmd.Env = append(os.Environ(), terminateProcessTreeHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		if err := stopProcessHelper(cmd, waitDone); err != nil {
			t.Errorf("helper cleanup: %v", err)
		}
	})

	ready := make([]byte, 1)
	readyResult := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(stdout, ready)
		readyResult <- err
	}()
	select {
	case err := <-readyResult:
		if err != nil {
			t.Fatalf("wait for helper readiness: %v (cleanup: %v)", err, stopProcessHelper(cmd, waitDone))
		}
	case <-time.After(processHelperTimeout):
		cleanupErr := stopProcessHelper(cmd, waitDone)
		_ = stdout.Close()
		var readyErr error
		select {
		case readyErr = <-readyResult:
		case <-time.After(processHelperTimeout):
			readyErr = errors.New("readiness wait timed out")
		}
		t.Fatalf("helper did not become ready (cleanup: %v, readiness: %v)", cleanupErr, readyErr)
	}
	if err := terminateProcessTree(cmd); err != nil {
		t.Fatalf("terminateProcessTree on live process = %v (cleanup: %v)", err, stopProcessHelper(cmd, waitDone))
	}
	if err := waitProcessHelper(waitDone); err != nil {
		t.Fatalf("%v (cleanup: %v)", err, stopProcessHelper(cmd, waitDone))
	}
}

func TestCancellationEvidenceSuppressesWindowsKillExitOneOnly(t *testing.T) {
	exitErr := terminalErrorExitErrorWithCode(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:                      ctx,
		binary:                   "docker",
		args:                     []string{"logs", "--follow", "x"},
		stderr:                   &tailBuffer{},
		cancelled:                true,
		terminatedByCancellation: true,
	}
	ps.drainCompleted.Store(true)
	err := ps.terminalError(exitErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled kill result = %v, want context cancellation", err)
	}
	var suppressed *CLIError
	if errors.As(err, &suppressed) {
		t.Fatalf("cancelled kill result = %v, unexpectedly retained CLIError", err)
	}

	genuine := &processStream{
		ctx:    ctx,
		binary: "docker",
		args:   []string{"logs", "--follow", "x"},
		stderr: &tailBuffer{},
	}
	genuine.drainCompleted.Store(true)
	err = genuine.terminalError(exitErr)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 1 {
		t.Fatalf("genuine exit result = %v, want exit-1 CLIError", err)
	}
}

func TestTerminateProcessTreeHelper(t *testing.T) {
	if os.Getenv(terminateProcessTreeHelperEnv) != "1" {
		return
	}
	if _, err := os.Stdout.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestTerminateProcessTreeRejectsMissingProcess(t *testing.T) {
	if err := terminateProcessTree(&exec.Cmd{}); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminateProcessTree without Process = %v, want os.ErrProcessDone", err)
	}
}
