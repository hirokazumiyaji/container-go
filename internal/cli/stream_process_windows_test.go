//go:build windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamWindowsJobAttachmentWindow(t *testing.T) {
	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skipf("cmd.exe is unavailable: %v", err)
	}
	dir := t.TempDir()
	started := filepath.Join(dir, "descendant.started")
	escaped := filepath.Join(dir, "descendant.escaped")
	descendant := filepath.Join(dir, "descendant.cmd")
	parent := filepath.Join(dir, "parent.cmd")
	descendantScript := fmt.Sprintf("@echo off\r\necho started>\"%s\"\r\nping -n 3 127.0.0.1 >nul\r\necho escaped>\"%s\"\r\n", started, escaped)
	if err := os.WriteFile(descendant, []byte(descendantScript), 0o700); err != nil {
		t.Fatal(err)
	}
	parentScript := fmt.Sprintf("@echo off\r\nstart \"\" /b \"%s\" /D /C \"%s\"\r\n\"%s\" /D /C \"ping -n 30 127.0.0.1 >nul\"\r\n", cmdPath, descendant, cmdPath)
	if err := os.WriteFile(parent, []byte(parentScript), 0o700); err != nil {
		t.Fatal(err)
	}

	var attachErr error
	stream, err := (&ExecRunner{Binary: cmdPath}).stream(context.Background(), streamHooks{
		beforeAttach: func(*processStream) {
			// The child is created before newProcessTree can assign the
			// parent to the new Job Object. This is the documented window.
			attachErr = waitForWindowsFile(started, 2*time.Second)
		},
	}, "/D", "/C", parent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if attachErr != nil {
		t.Fatal(attachErr)
	}
	ps := stream.(*processStream)
	if !ps.treeAttached {
		t.Skip("Windows Job Object assignment is unavailable in this host")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := waitForWindowsFile(escaped, 5*time.Second); err != nil {
		t.Fatalf("pre-attachment descendant did not escape as documented: %v", err)
	}
}

func waitForWindowsFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
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
		t.Skip("Windows Job Object assignment is unavailable in this host")
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
