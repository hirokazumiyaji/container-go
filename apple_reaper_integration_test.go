//go:build integration && !windows

package container_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
)

// TestIntegrationReaperSurvivesSIGKILL re-runs this test binary as a
// child that starts a container and blocks. The parent SIGKILLs the
// child (no defers, no signal handlers run) and then watches the
// watchdog reaper remove the container.
func TestIntegrationReaperSurvivesSIGKILL(t *testing.T) {
	if os.Getenv("CONTAINERGO_REAPER_CHILD") == "1" {
		ctx := context.Background()
		ctr, err := container.Run(ctx, integrationAlpine,
			container.WithName(os.Getenv("CONTAINERGO_REAPER_NAME")),
			container.WithCmd("sleep", "120"))
		if err != nil {
			fmt.Println("CHILD-ERROR:", err)
			os.Exit(1)
		}
		fmt.Println("READY:", ctr.ID())
		select {} // block until SIGKILLed
	}

	requireSystem(t)
	// Keep mode disables the watchdog; clear it in both the parent and
	// child so an inherited CI/debug setting cannot mask reaper behavior.
	t.Setenv("CONTAINERGO_KEEP", "")
	name := fmt.Sprintf("containergo-reapertest-%d", os.Getpid())

	cmd := exec.Command(os.Args[0], "-test.run", "TestIntegrationReaperSurvivesSIGKILL")
	cmd.Env = append(os.Environ(),
		"CONTAINERGO_REAPER_CHILD=1",
		"CONTAINERGO_REAPER_NAME="+name,
		"CONTAINERGO_KEEP=")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = exec.Command("container", "delete", "--force", name).Run()
	}()

	// Wait for the child to report the running container.
	ready := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := stdout.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "READY:") {
				ready <- acc
				return
			}
			if err != nil {
				ready <- acc
				return
			}
		}
	}()
	select {
	case out := <-ready:
		if !strings.Contains(out, "READY:") {
			t.Fatalf("child failed before starting container: %q", out)
		}
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("child never became ready")
	}

	// SIGKILL: no Go cleanup runs in the child; only the reaper can
	// remove the container.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("container", "ls", "--all", "--quiet").Output()
		if err == nil && !strings.Contains(string(out), name) {
			return // reaper removed it
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("container %s still present 60s after SIGKILL; reaper did not fire", name)
}
