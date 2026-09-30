//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	windowsExit259HelperEnv  = "CONTAINERGO_WINDOWS_EXIT_259_HELPER"
	windowsExit259ReadyEnv   = "CONTAINERGO_WINDOWS_EXIT_259_READY"
	windowsExit259ReleaseEnv = "CONTAINERGO_WINDOWS_EXIT_259_RELEASE"
)

func TestWindowsExit259Helper(t *testing.T) {
	if os.Getenv(windowsExit259HelperEnv) != "1" {
		return
	}
	ready := os.Getenv(windowsExit259ReadyEnv)
	release := os.Getenv(windowsExit259ReleaseEnv)
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	for {
		if _, err := os.Stat(release); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Exit(259)
}

func TestWindowsExit259IsNotActive(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsExit259Helper$")
	cmd.Env = append(os.Environ(),
		windowsExit259HelperEnv+"=1",
		windowsExit259ReadyEnv+"="+ready,
		windowsExit259ReleaseEnv+"="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Skipf("test helper could not start: %v", err)
	}
	releaseHelper := func() { _ = os.WriteFile(release, []byte("release"), 0o600) }
	t.Cleanup(func() {
		releaseHelper()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exit-259 helper did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}

	tree, err := newProcessTree(cmd)
	if err != nil {
		releaseHelper()
		_ = cmd.Wait()
		t.Skipf("Windows Job Object assignment is unavailable on this host: %v", err)
	}
	windowsTree, ok := tree.(*windowsProcessTree)
	if !ok {
		tree.close()
		t.Fatal("newProcessTree returned an unexpected tree implementation")
	}
	t.Cleanup(tree.close)
	releaseHelper()
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 259 {
		t.Fatalf("helper exit = %v, want exit code 259", waitErr)
	}

	active, err := windowsProcessActive(windowsTree.process)
	if err != nil {
		t.Fatalf("active check: %v", err)
	}
	if active {
		t.Fatal("process with exit code 259 was reported active")
	}
	result := tree.terminate(cmd)
	if result.active || result.err == nil {
		t.Fatalf("termination result = %+v, want inactive/process-done", result)
	}
}

func TestWindowsEmptyJobTerminationDoesNotClaimActiveChild(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/D", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cmd.exe is unavailable: %v", err)
	}
	tree, err := newProcessTree(cmd)
	if err != nil {
		_ = cmd.Wait()
		t.Skipf("Windows Job Object assignment is unavailable on this host: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		tree.close()
		t.Fatal(err)
	}
	result := tree.terminate(cmd)
	tree.close()
	if result.active || result.err == nil {
		t.Fatalf("termination result = %+v, want inactive/process-done", result)
	}
}

func TestStreamCloseTerminatesWindowsDescendants(t *testing.T) {
	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skipf("cmd.exe is unavailable: %v", err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "descendant.marker")
	started := filepath.Join(dir, "descendant.started")
	descendant := filepath.Join(dir, "descendant.cmd")
	parent := filepath.Join(dir, "parent.cmd")
	descendantScript := fmt.Sprintf("@echo off\r\necho started>\"%s\"\r\nping -n 4 127.0.0.1 >nul\r\necho survived>\"%s\"\r\n", started, marker)
	if err := os.WriteFile(descendant, []byte(descendantScript), 0o700); err != nil {
		t.Fatal(err)
	}
	parentScript := fmt.Sprintf("@echo off\r\nstart \"\" /b \"%s\" /D /C \"%s\"\r\n\"%s\" /D /C \"ping -n 30 127.0.0.1 >nul\"\r\n", cmdPath, descendant, cmdPath)
	if err := os.WriteFile(parent, []byte(parentScript), 0o700); err != nil {
		t.Fatal(err)
	}

	stream, err := (&ExecRunner{Binary: cmdPath}).Stream(context.Background(), "/D", "/C", parent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() { _ = stream.Close() })
	if !ps.treeAttached {
		t.Skip("Windows Job Object assignment is unavailable on this host")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Windows descendant did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The detached cmd child would create the marker after about three
	// seconds if the Job Object did not terminate the process tree.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Windows descendant survived stream.Close")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}
