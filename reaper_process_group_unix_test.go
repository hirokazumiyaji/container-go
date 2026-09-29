//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReaperDescendantsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- reaperDescendantsWithLookup(ctx, 1, nil, func(ctx context.Context, _ int) ([]int, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded descendant lookup did not return")
	}
}

func TestReaperDescendantsWalksNestedTree(t *testing.T) {
	graph := map[int][]int{
		100: {101},
		101: {102},
		102: nil,
	}
	var visited []int
	err := reaperDescendantsWithLookup(
		context.Background(),
		100,
		func(pid int) error { visited = append(visited, pid); return nil },
		func(_ context.Context, parent int) ([]int, error) { return graph[parent], nil },
	)
	if err != nil {
		t.Fatalf("nested traversal: %v", err)
	}
	if len(visited) != 2 || visited[0] != 101 || visited[1] != 102 {
		t.Fatalf("visited = %v, want [101 102]", visited)
	}
}

func TestReaperDescendantsStopsAtTraversalLimit(t *testing.T) {
	var visits int
	err := reaperDescendantsWithLookupAndIdentity(
		context.Background(),
		1,
		func(int) error { visits++; return nil },
		func(_ context.Context, parent int) ([]int, error) { return []int{parent + 1}, nil },
		nil,
	)
	if !errors.Is(err, errReaperDescendantLimit) {
		t.Fatalf("error = %v, want descendant traversal limit", err)
	}
	if visits == 0 || visits > maxReaperDescendantPIDs {
		t.Fatalf("visits = %d, want a bounded nonzero traversal", visits)
	}
}

func TestReaperScriptSetsidContainsDeleteDescendants(t *testing.T) {
	setsidPath, err := trustedReaperTool("setsid")
	if err != nil {
		t.Skipf("setsid unavailable: %v", err)
	}
	pgrepPath, err := trustedReaperTool("pgrep")
	if err != nil {
		t.Skipf("pgrep unavailable: %v", err)
	}
	awkPath, err := trustedReaperTool("awk")
	if err != nil {
		t.Skipf("awk unavailable: %v", err)
	}
	sleepPath, err := trustedReaperTool("sleep")
	if err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	psPath, err := trustedReaperTool("ps")
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	trPath, err := trustedReaperTool("tr")
	if err != nil {
		t.Skipf("tr unavailable: %v", err)
	}
	rmPath, err := trustedReaperTool("rm")
	if err != nil {
		t.Skipf("rm unavailable: %v", err)
	}

	dir := t.TempDir()
	binPath := filepath.Join(dir, "container")
	logPath := filepath.Join(dir, "calls.log")
	childPath := filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = delete ] && [ \"$3\" = first ]; then\n" +
		"  sleep 30 >/dev/null 2>&1 &\n" +
		"  echo $! > " + childPath + "\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	statusDir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", reaperScript,
		"containergo-reaper", binPath, "delete", creationLabel, "1", "1",
		awkPath, sleepPath, "", "", rmPath, "setsid-test", "main",
		psPath, trPath, statusDir, "lockf", pgrepPath, setsidPath)
	group, err := newReaperGroupOwner()
	if err != nil {
		t.Fatal(err)
	}
	prepareReaperCommand(cmd, group)
	cmd.Stdin = strings.NewReader("first\nlater\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("reaper script: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"delete --force first", "delete --force later"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("calls = %q, want %q", data, want)
		}
	}
	childData, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	var child int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(childData)), "%d", &child); err != nil || child <= 0 {
		t.Fatalf("child pid = %q: %v", childData, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived setsid cleanup", child)
}
