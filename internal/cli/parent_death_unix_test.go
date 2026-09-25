//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunnerParentDeathPreservesArgv(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "argv")
	target := filepath.Join(dir, "backend")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + output + "\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "name with spaces", "value;$(touch nope)", "--label", "a=b"}
	if _, _, err := (&ExecRunner{Binary: target}).RunWithParentDeath(context.Background(), args...); err != nil {
		t.Fatalf("RunWithParentDeath: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), strings.Join(args, "\n"); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestExecRunnerParentDeathKillsBackendTree(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "backend.pid")
	childPath := filepath.Join(dir, "child.pid")
	target := filepath.Join(dir, "backend")
	script := "#!/bin/sh\n" +
		"echo $$ > " + pidPath + "\n" +
		"sleep 30 &\n" +
		"echo $! > " + childPath + "\n" +
		"trap '' TERM\n" +
		"wait\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run", "^TestExecRunnerParentDeathHelper$")
	helper.Env = append(os.Environ(),
		"CONTAINERGO_PARENT_DEATH_HELPER=1",
		"CONTAINERGO_PARENT_DEATH_TARGET="+target,
		"SHELLOPTS=monitor",
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}

	pid := waitForPIDFile(t, pidPath)
	child := waitForPIDFile(t, childPath)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	waitForProcessGone(t, pid)
	waitForProcessGone(t, child)
}

func TestExecRunnerCancellationKillsOrdinaryTree(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "backend.pid")
	childPath := filepath.Join(dir, "child.pid")
	target := filepath.Join(dir, "backend")
	script := "#!/bin/sh\n" +
		"echo $$ > " + pidPath + "\n" +
		"sleep 30 &\n" +
		"echo $! > " + childPath + "\n" +
		"trap '' TERM\n" +
		"wait\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := &ExecRunner{Binary: target}
	result := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(ctx, "run")
		result <- err
	}()
	waitForPIDFile(t, pidPath)
	waitForPIDFile(t, childPath)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	waitForProcessGone(t, mustReadPIDFile(t, pidPath))
	waitForProcessGone(t, mustReadPIDFile(t, childPath))
}

func mustReadPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file %s = %q", path, data)
	}
	return pid
}

func TestExecRunnerStreamCloseKillsTree(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "stream-child.pid")
	target := filepath.Join(dir, "stream-target")
	script := "#!/bin/sh\n" +
		"sleep 30 &\n" +
		"echo $! > " + childPath + "\n" +
		"echo line\n" +
		"wait\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stream, err := (&ExecRunner{Binary: target}).Stream(context.Background(), "logs")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	childPID := waitForPIDFile(t, childPath)
	scanner := bufio.NewScanner(stream)
	if !scanner.Scan() {
		t.Fatalf("stream ended before output: %v", scanner.Err())
	}
	if scanner.Text() != "line" {
		t.Fatalf("stream line = %q, want line", scanner.Text())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitForProcessGone(t, childPID)
}

func TestExecRunnerParentDeathHelper(t *testing.T) {
	if os.Getenv("CONTAINERGO_PARENT_DEATH_HELPER") != "1" {
		return
	}
	r := &ExecRunner{Binary: os.Getenv("CONTAINERGO_PARENT_DEATH_TARGET")}
	if _, _, err := r.RunWithParentDeath(context.Background(), "run"); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestExecRunnerParentDeathHonorsContextCancellation(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "backend")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := (&ExecRunner{Binary: target}).RunWithParentDeath(ctx, "run")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for PID file %s", path)
	return 0
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend process %d survived parent death", pid)
}
