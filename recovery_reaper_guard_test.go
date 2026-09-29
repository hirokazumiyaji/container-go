//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func reaperEntriesForTest(t *testing.T, binary string) []reaperEntry {
	t.Helper()
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reaperEntry(nil), r.entries...)
}

func closeRecoveryReaperForTest(t *testing.T, binary string) {
	t.Helper()
	globalReapersMu.Lock()
	r := globalReapers[binary]
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	if r != nil {
		closeMalformedRecoveryReaper(t, binary, r)
	}
}

func TestIssue94MalformedOrdinaryRecoveryDeleteModeFollowsState(t *testing.T) {
	cases := []struct {
		name      string
		state     string
		wantForce bool
	}{
		{name: "running", state: "running", wantForce: true},
		{name: "stopping", state: "restarting", wantForce: true},
		{name: "stopped", state: "exited", wantForce: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", "")
			bin, _ := writeReaperStub(t)
			uid := strings.Repeat("1", 64)
			base := newTestRunner()
			base.imagePresent = true
			runner := &issue94MalformedDockerRecoveryRunner{
				fakeRunner: base,
				binary:     bin,
				uid:        uid,
				state:      tc.state,
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("malformed-recovery"), withRunner(runner), withEngine(dockerEngine{}))
			if err == nil {
				t.Fatal("Run succeeded although docker printed no usable ID")
			}
			runner.mu.Lock()
			calls := append([][]string(nil), runner.deleteArgs...)
			runner.mu.Unlock()
			if len(calls) != 1 {
				t.Fatalf("rm calls = %v, want one delete", calls)
			}
			if got := slices.Contains(calls[0], "--force"); got != tc.wantForce {
				t.Errorf("rm = %v, want force=%v", calls[0], tc.wantForce)
			}
			closeRecoveryReaperForTest(t, bin)
		})
	}
}

func TestIssue94MalformedReuseRecoveryWithdrawsGuardedTargetOnDeleteRefusal(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	uid := strings.Repeat("2", 64)
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue94MalformedDockerRecoveryRunner{
		fakeRunner: base,
		binary:     bin,
		uid:        uid,
		state:      "exited",
		deleteErr:  &cli.CLIError{Args: []string{"rm", uid}, ExitCode: 1, Stderr: "container is running"},
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("malformed-reuse"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("Run succeeded although the guarded delete refused")
	}
	runner.mu.Lock()
	calls := append([][]string(nil), runner.deleteArgs...)
	runner.mu.Unlock()
	if len(calls) != 1 || slices.Contains(calls[0], "--force") {
		t.Fatalf("rm calls = %v, want one non-forced guarded delete", calls)
	}
	if entries := reaperEntriesForTest(t, bin); len(entries) != 0 {
		t.Fatalf("reaper entries = %+v, want the guarded target withdrawn", entries)
	}
	closeRecoveryReaperForTest(t, bin)
}

func TestIssue94MalformedReuseRecoveryRejectsStartBeforeWatchdogRegistration(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	uid := strings.Repeat("3", 64)
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue94MalformedDockerRecoveryRunner{
		fakeRunner: base,
		binary:     bin,
		uid:        uid,
		states:     []string{"exited", "running"},
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("malformed-reuse"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
	var cleanupErr *CleanupError
	if err == nil || !errors.As(err, &cleanupErr) {
		t.Fatalf("Run error = %v, want a guarded CleanupError", err)
	}
	runner.mu.Lock()
	deleted := append([]string(nil), runner.deleted...)
	runner.mu.Unlock()
	if len(deleted) != 0 {
		t.Fatalf("delete targets = %v, want none after the reuse generation started", deleted)
	}
	if entries := reaperEntriesForTest(t, bin); len(entries) != 0 {
		t.Fatalf("reaper entries = %+v, want none when revalidation refuses", entries)
	}
	closeRecoveryReaperForTest(t, bin)
}

// guardedReuseRunner serves a stopped, owned Docker reuse generation and
// refuses the non-forced rm as a real daemon does after a concurrent start.
type guardedReuseRunner struct {
	*fakeRunner
	binary   string
	uid      string
	creation string
	mu       sync.Mutex
	calls    [][]string
}

func (r *guardedReuseRunner) External() bool         { return true }
func (r *guardedReuseRunner) ExternalBinary() string { return r.binary }

func (r *guardedReuseRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Image":"redis:7-alpine","Labels":{%q:"true",%q:"true",%q:%q}}}]`,
			r.uid, managedLabel, reuseLabel, creationLabel, r.creation)), nil, nil
	}
	if args[0] == "rm" {
		r.mu.Lock()
		r.calls = append(r.calls, append([]string(nil), args...))
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "container is running"}
	}
	return r.fakeRunner.Run(context.Background(), args...)
}

func TestIssue94StoppedReuseDeleteRefusalWithdrawsWatchdogTarget(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	uid := strings.Repeat("4", 64)
	runner := &guardedReuseRunner{
		fakeRunner: newTestRunner(),
		binary:     bin,
		uid:        uid,
		creation:   "0123456789abcdef",
	}
	info := &engineInfo{
		state: StateStopped,
		uid:   uid,
		labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: runner.creation,
		},
	}
	cfg := &config{runner: runner, eng: dockerEngine{}, name: "shared"}
	removed, err := deleteStoppedReuseChecked(context.Background(), cfg, info)
	if err == nil || removed {
		t.Fatalf("deleteStoppedReuseChecked = (%v, %v), want a refusal", removed, err)
	}
	runner.mu.Lock()
	calls := append([][]string(nil), runner.calls...)
	runner.mu.Unlock()
	if len(calls) != 1 || slices.Contains(calls[0], "--force") {
		t.Fatalf("rm calls = %v, want one non-forced delete", calls)
	}
	if entries := reaperEntriesForTest(t, bin); len(entries) != 0 {
		t.Fatalf("reaper entries = %+v, want the stopped target withdrawn", entries)
	}
	closeRecoveryReaperForTest(t, bin)
}
