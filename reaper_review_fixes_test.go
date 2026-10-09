//go:build !windows

package container

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestReaperPrefersFlockBeforeLockf(t *testing.T) {
	flock := strings.Index(reaperScript, `flock_bin=$(command -v flock`)
	lockf := strings.Index(reaperScript, `lockf_bin=$(command -v lockf`)
	if flock < 0 || lockf < 0 {
		t.Fatalf("reaper script lock helpers = flock:%d lockf:%d, want both", flock, lockf)
	}
	if flock > lockf {
		t.Fatalf("reaper script checks lockf before flock (flock=%d, lockf=%d)", flock, lockf)
	}
}

func waitForReaperTestExit(t *testing.T, r *reaper) {
	t.Helper()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper child did not exit")
	}
}

func TestReaperSpawnRecoveryIsBoundedAndReplaysEntries(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "rm", "--volumes")
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperTestExit(t, r)
	})
	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }
	r.retryBackoff = func(int) time.Duration { return 5 * time.Second }

	attempts := 0
	failSpawn := true
	r.spawnCommand = func() (*exec.Cmd, error) {
		attempts++
		if failSpawn {
			return nil, errors.New("synthetic reaper spawn failure")
		}
		args := []string{"-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel), "1", "1"}
		args = append(args, r.deleteFlags...)
		return exec.Command("/bin/sh", args...), nil
	}
	ids := []string{
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
		strings.Repeat("c", 64),
	}

	if err := r.register(ids[0], ""); !errors.Is(err, errReaperSpawnFailed) {
		t.Fatalf("initial registration error = %v, want repeated spawn failure", err)
	}
	if attempts != maxReaperSpawnFailures {
		t.Fatalf("initial spawn attempts = %d, want %d", attempts, maxReaperSpawnFailures)
	}
	if !r.gaveUpValue() {
		t.Fatal("reaper did not enter the bounded give-up state")
	}

	// The first recovery burst is allowed immediately, but remains bounded
	// to the same three attempts. A second failed burst then starts cooldown.
	if err := r.register(ids[1], ""); !errors.Is(err, errReaperSpawnFailed) {
		t.Fatalf("recovery registration error = %v, want repeated spawn failure", err)
	}
	if attempts != 2*maxReaperSpawnFailures {
		t.Fatalf("recovery spawn attempts = %d, want %d", attempts, 2*maxReaperSpawnFailures)
	}

	// Registrations during cooldown retain their entries but cannot issue
	// destructive commands or start an unbounded stream of retries.
	if err := r.register(ids[2], ""); !errors.Is(err, errReaperSpawnCooldown) {
		t.Fatalf("cooldown registration error = %v, want cooldown", err)
	}
	if attempts != 2*maxReaperSpawnFailures {
		t.Fatalf("cooldown changed attempts to %d", attempts)
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("reaper issued commands before a child was recovered: %q", data)
	}
	r.mu.Lock()
	entries := len(r.entries)
	r.mu.Unlock()
	if entries != 3 {
		t.Fatalf("retained entries = %d, want 3", entries)
	}

	// Once the bounded cooldown elapses, one normal registration recovers
	// and replays every retained identity into a fresh child.
	now = now.Add(5 * time.Second)
	failSpawn = false
	if err := r.register(strings.Repeat("d", 64), ""); err != nil {
		t.Fatalf("registration after cooldown: %v", err)
	}
	if r.gaveUpValue() || r.spawnFailuresValue() != 0 {
		t.Fatalf("failure state after recovery = gaveUp:%v failures:%d", r.gaveUpValue(), r.spawnFailuresValue())
	}
	r.closeStdin()
	waitForReaperTestExit(t, r)
	waitForLogLines(t, logPath,
		"rm --force --volumes "+ids[0],
		"rm --force --volumes "+ids[1],
		"rm --force --volumes "+ids[2],
		"rm --force --volumes "+strings.Repeat("d", 64),
	)
}
