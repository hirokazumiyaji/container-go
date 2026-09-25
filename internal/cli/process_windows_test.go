//go:build windows

package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

const terminateProcessTreeHelperEnv = "CONTAINER_GO_TERMINATE_PROCESS_TREE_HELPER"

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
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	stop := func() {
		_ = cmd.Process.Kill()
		<-waitDone
	}

	ready := make([]byte, 1)
	readyResult := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(stdout, ready)
		readyResult <- err
	}()
	select {
	case err := <-readyResult:
		if err != nil {
			stop()
			t.Fatalf("wait for helper readiness: %v", err)
		}
	case <-time.After(5 * time.Second):
		stop()
		<-readyResult
		t.Fatal("helper did not become ready")
	}
	if err := terminateProcessTree(cmd); err != nil {
		stop()
		t.Fatalf("terminateProcessTree on live process = %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("live process was not terminated")
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
