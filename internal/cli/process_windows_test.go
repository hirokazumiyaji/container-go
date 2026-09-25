//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	terminateProcessTreeHelperEnv = "CONTAINER_GO_TERMINATE_PROCESS_TREE_HELPER"
	processHelperTimeout          = 2 * time.Second
	retainedHandoffHelperEnv      = "CONTAINER_GO_RETAINED_HANDOFF_HELPER"
	retainedHandoffReadyEnv       = "CONTAINER_GO_RETAINED_HANDOFF_READY"
	retainedHandoffReleaseEnv     = "CONTAINER_GO_RETAINED_HANDOFF_RELEASE"
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

func TestRetainedProcessTreeExitCancelHandoffUsesOriginalHandle(t *testing.T) {
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	releasePath := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRetainedProcessTreeExitHelper$")
	cmd.Env = append(os.Environ(),
		retainedHandoffHelperEnv+"=1",
		retainedHandoffReadyEnv+"="+readyPath,
		retainedHandoffReleaseEnv+"="+releasePath,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree, err := newProcessTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("retain process tree: %v", err)
	}

	waitForRetainedHandoffFile(t, readyPath)
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		tree.close()
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
		tree.close()
		t.Fatalf("helper wait error = %v, want exit 1", waitErr)
	}

	result := tree.terminate(cmd)
	tree.close()
	if !errors.Is(result.err, os.ErrProcessDone) {
		t.Fatalf("post-Wait termination = %+v, want retained handle process-done", result)
	}
}

func waitForRetainedHandoffFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handoff helper did not write %s", path)
}

func TestRetainedProcessTreeExitHelper(t *testing.T) {
	if os.Getenv(retainedHandoffHelperEnv) != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv(retainedHandoffReadyEnv), nil, 0o600); err != nil {
		os.Exit(2)
	}
	for {
		if _, err := os.Stat(os.Getenv(retainedHandoffReleaseEnv)); err == nil {
			os.Exit(1)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancellationEvidenceSuppressesWindowsKillExitOneOnly(t *testing.T) {
	exitErr := terminalErrorExitErrorWithCode(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:                  ctx,
		binary:               "docker",
		args:                 []string{"logs", "--follow", "x"},
		stderr:               &tailBuffer{},
		cancelled:            true,
		syntheticTermination: true,
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
