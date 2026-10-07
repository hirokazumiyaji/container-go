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

func TestIntegrationDockerReaperSurvivesSIGKILL(t *testing.T) {
	if os.Getenv("CONTAINERGO_REAPER_CHILD") == "1" {
		// The child's TestMain unsets CONTAINERGO_BACKEND, so the
		// env-passed selection never reaches Run; pin it here. This
		// test is specifically about the reaper, not keep mode.
		_ = os.Setenv("CONTAINERGO_BACKEND", "docker")
		_ = os.Unsetenv("CONTAINERGO_KEEP")
		ctx := context.Background()
		ctr, err := container.Run(ctx, integrationRedis,
			container.WithName(os.Getenv("CONTAINERGO_REAPER_NAME")),
			container.WithEntrypoint("/bin/sh"),
			container.WithCmd("-c", "sleep 120"),
			container.WithMounts(container.Mount{
				Type:   container.MountVolume,
				Source: os.Getenv("CONTAINERGO_REAPER_VOLUME"),
				Target: "/named",
			}))
		if err != nil {
			fmt.Println("CHILD-ERROR:", err)
			os.Exit(1)
		}
		fmt.Println("READY:", ctr.ID())
		select {}
	}

	requireDocker(t)
	// The reaper is disabled by CONTAINERGO_KEEP=1. Clear it in the
	// parent so the child process exercises reaper behavior even when
	// the surrounding test environment enables keep mode.
	t.Setenv("CONTAINERGO_KEEP", "")
	name := fmt.Sprintf("containergo-dockerreap-%d", os.Getpid())
	namedVolume := name + "-named"
	if out, err := exec.Command("docker", "volume", "create", namedVolume).CombinedOutput(); err != nil {
		t.Fatalf("create named volume: %v: %s", err, out)
	}
	var anonymousVolume string
	defer func() {
		_ = exec.Command("docker", "rm", "--force", "--volumes", name).Run()
		if anonymousVolume != "" {
			_ = exec.Command("docker", "volume", "rm", anonymousVolume).Run()
		}
		_ = exec.Command("docker", "volume", "rm", namedVolume).Run()
	}()

	cmd := exec.Command(os.Args[0], "-test.run", "TestIntegrationDockerReaperSurvivesSIGKILL")
	cmd.Env = append(os.Environ(),
		"CONTAINERGO_BACKEND=docker",
		"CONTAINERGO_REAPER_CHILD=1",
		"CONTAINERGO_REAPER_NAME="+name,
		"CONTAINERGO_REAPER_VOLUME="+namedVolume)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	ready := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := stdout.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "READY:") || err != nil {
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

	anonymousVolume = dockerMountVolumeName(t, name, "/data")
	if anonymousVolume == "" {
		t.Fatal("image-defined /data volume not found")
	}
	if mounted := dockerMountVolumeName(t, name, "/named"); mounted != namedVolume {
		t.Fatalf("named volume mount = %q, want %q", mounted, namedVolume)
	}
	waitForDockerVolume(t, anonymousVolume, true)
	waitForDockerVolume(t, namedVolume, true)

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "ps", "--all", "--format", "{{.Names}}").Output()
		if err == nil && !strings.Contains(string(out), name) {
			waitForDockerVolume(t, anonymousVolume, false)
			waitForDockerVolume(t, namedVolume, true)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("container %s still present 60s after SIGKILL; reaper did not fire", name)
}
