//go:build !windows

package container

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLockNameSerializesHolders(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock while held: err = %v, want deadline exceeded", err)
	}

	unlock()
	unlock2, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestTerminateWaitsForNameLockBeforeInspecting(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	// Another process holds the name: this handle's inspect-then-delete
	// must not start until it is released, so a bounded context fails
	// before any backend call.
	r := &generationRunner{creation: "aaaaaaaaaaaaaaaa"}
	ctr := &Container{id: name, runner: r, eng: appleEngine{}, creation: "aaaaaaaaaaaaaaaa"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = ctr.Terminate(ctx)
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Terminate = %v, want lock failure", err)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 while the name is locked elsewhere", r.deleteCalls)
	}
}

func TestPruneAppleWaitsForNameLockBeforeInspect(t *testing.T) {
	const creation = "aaaaaaaaaaaaaaaa"
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	r := &pruneSafetyRunner{
		list:     []pruneFixtureContainer{pruneFixture(name, creation, string(StateStopped), "", true)},
		inspects: []pruneFixtureContainer{pruneFixture(name, creation, string(StateStopped), "", true)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = pruneWith(ctx, r, appleEngine{})
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Prune = %v, want lock failure", err)
	}
	if r.callCount("inspect") != 0 || len(r.deleted) != 0 {
		t.Fatalf("inspect calls = %d, deleted = %v; backend touched while locked", r.callCount("inspect"), r.deleted)
	}
}

func TestRunAppleCreateTakesNameLock(t *testing.T) {
	name := "lock-" + newContainerName()
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName: %v", err)
	}
	defer unlock()

	f := newTestRunner()
	f.imagePresent = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = Run(ctx, "redis:7-alpine", WithName(name), withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "lock name") {
		t.Fatalf("Run = %v, want create lock failure", err)
	}
	if f.callWith("run") != nil {
		t.Error("run was issued while name lock was held")
	}
}

const nameLockHelperEnv = "CONTAINERGO_NAMELOCK_HELPER"

func TestNameLockExcludesAnotherProcess(t *testing.T) {
	name := "lock-" + newContainerName()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNameLockHelperProcess$")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), nameLockHelperEnv+"=1", "CONTAINERGO_NAMELOCK_NAME="+name)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		if strings.Contains(line, "CONTAINERGO_LOCKED") {
			break
		}
		if readErr != nil {
			t.Fatalf("helper output = %q, err = %v", line, readErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockName(ctx, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockName while helper holds lock = %v, want deadline exceeded", err)
	}

	// Closing stdin is the helper's deterministic release signal.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatalf("lockName after helper release: %v", err)
	}
	unlock()
}

// TestNameLockHelperProcess is launched as a separate test binary by
// TestNameLockExcludesAnotherProcess. It holds the lock until stdin closes.
func TestNameLockHelperProcess(t *testing.T) {
	if os.Getenv(nameLockHelperEnv) != "1" {
		return
	}
	name := os.Getenv("CONTAINERGO_NAMELOCK_NAME")
	unlock, err := lockName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fmt.Fprintln(os.Stdout, "CONTAINERGO_LOCKED")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
