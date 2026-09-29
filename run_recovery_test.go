//go:build !windows

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type issue94MalformedDockerRecoveryRunner struct {
	*fakeRunner
	binary     string
	uid        string
	creation   string
	state      string
	states     []string
	inspects   int
	deleteErr  error
	deleted    []string
	deleteArgs [][]string
}

func (r *issue94MalformedDockerRecoveryRunner) External() bool         { return true }
func (r *issue94MalformedDockerRecoveryRunner) ExternalBinary() string { return r.binary }

func (r *issue94MalformedDockerRecoveryRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "run":
		r.mu.Lock()
		for i, arg := range args {
			if value := strings.TrimPrefix(arg, creationLabel+"="); value != arg {
				r.creation = value
			}
			if arg == "--label" && i+1 < len(args) {
				if value := strings.TrimPrefix(args[i+1], creationLabel+"="); value != args[i+1] {
					r.creation = value
				}
			}
		}
		r.mu.Unlock()
		// The command succeeded, but the daemon's detached ID was
		// truncated on the wire. A container may nevertheless exist.
		return []byte("truncated-id\n"), nil, nil
	case "inspect":
		target := args[len(args)-1]
		r.mu.Lock()
		creation := r.creation
		state := r.state
		if len(r.states) > 0 {
			index := r.inspects
			if index >= len(r.states) {
				index = len(r.states) - 1
			}
			state = r.states[index]
			r.inspects++
		}
		r.mu.Unlock()
		if state == "" {
			state = "running"
		}
		if target == "malformed-recovery" || target == "malformed-reuse" || target == r.uid {
			if creation == "" {
				return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "not found"}
			}
			return issue94MalformedRecoveryInspect(r.uid, creation, state), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "not found"}
	case "rm":
		r.mu.Lock()
		r.deleted = append(r.deleted, args[len(args)-1])
		r.deleteArgs = append(r.deleteArgs, append([]string(nil), args...))
		err := r.deleteErr
		r.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func issue94MalformedRecoveryInspect(uid, creation, state string) []byte {
	labels, _ := json.Marshal(map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		creationLabel: creation,
		reuseLabel:    "true",
	})
	return []byte(fmt.Sprintf(`[{
    "Id": %q,
    "State": {"Status": %q},
    "Config": {"Image": "redis:7-alpine", "Labels": %s}
  }]`, uid, state, labels))
}

func TestIssue94MalformedDockerRunRecoversCleansAndRegistersUID(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	uid := strings.Repeat("f", 64)
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue94MalformedDockerRecoveryRunner{
		fakeRunner: base,
		binary:     bin,
		uid:        uid,
		deleteErr:  &cli.CLIError{Args: []string{"rm", "--force", uid}, ExitCode: 1, Stderr: "delete failed"},
	}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("malformed-recovery"), withRunner(runner), withEngine(dockerEngine{}))
	if ctr != nil {
		t.Fatalf("Run returned handle %v for malformed output", ctr)
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	if cleanupErr.CleanupErr == nil || !strings.Contains(cleanupErr.CleanupErr.Error(), "cleanup") {
		t.Fatalf("cleanup error = %v", cleanupErr.CleanupErr)
	}

	runner.mu.Lock()
	deleted := append([]string(nil), runner.deleted...)
	runner.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != uid {
		t.Fatalf("delete targets = %v, want immutable UID %q", deleted, uid)
	}

	globalReapersMu.Lock()
	reaper := globalReapers[bin]
	if reaper != nil {
		reaper.mu.Lock()
		entries := append([]reaperEntry(nil), reaper.entries...)
		reaper.mu.Unlock()
		if len(entries) != 1 || entries[0].id != uid || entries[0].creation != "" {
			t.Errorf("reaper entries = %+v, want recovered UID", entries)
		}
	}
	globalReapersMu.Unlock()
	if reaper == nil {
		t.Fatal("malformed output recovery did not register a reaper")
	}
	closeMalformedRecoveryReaper(t, bin, reaper)
}

func TestIssue94MalformedDockerReuseRunCleansStoppedGenerationByUID(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "")
	bin, _ := writeReaperStub(t)
	uid := strings.Repeat("e", 64)
	base := newTestRunner()
	base.imagePresent = true
	runner := &issue94MalformedDockerRecoveryRunner{
		fakeRunner: base,
		binary:     bin,
		uid:        uid,
		state:      "exited",
		deleteErr:  &cli.CLIError{Args: []string{"rm", "--force", uid}, ExitCode: 1, Stderr: "delete failed"},
	}

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("malformed-reuse"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
	if ctr != nil {
		t.Fatalf("Run returned handle %v for malformed reuse output", ctr)
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error = %v, want CleanupError", err)
	}
	runner.mu.Lock()
	deleted := append([]string(nil), runner.deleted...)
	runner.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != uid {
		runner.mu.Lock()
		calls := append([][]string(nil), runner.calls...)
		runner.mu.Unlock()
		t.Fatalf("delete targets = %v, calls = %v, err = %v, want immutable UID %q", deleted, calls, err, uid)
	}
	globalReapersMu.Lock()
	reaper := globalReapers[bin]
	globalReapersMu.Unlock()
	if reaper == nil {
		t.Fatal("malformed reuse output did not register a reaper")
	}
	closeMalformedRecoveryReaper(t, bin, reaper)
}

func TestIssue94MalformedDockerUnsafeReuseDoesNotRegisterReaper(t *testing.T) {
	for _, state := range []string{"running", "paused"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("CONTAINERGO_KEEP", "")
			bin, _ := writeReaperStub(t)
			uid := strings.Repeat("d", 64)
			base := newTestRunner()
			base.imagePresent = true
			runner := &issue94MalformedDockerRecoveryRunner{
				fakeRunner: base,
				binary:     bin,
				uid:        uid,
				state:      state,
			}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("malformed-reuse"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
			var cleanupErr *CleanupError
			if ctr != nil || !errors.As(err, &cleanupErr) {
				t.Fatalf("Run = (%v, %v), want CleanupError refusal", ctr, err)
			}
			runner.mu.Lock()
			deleted := append([]string(nil), runner.deleted...)
			runner.mu.Unlock()
			if len(deleted) != 0 {
				t.Fatalf("unsafe reuse generation was deleted: %v", deleted)
			}
			globalReapersMu.Lock()
			reaper := globalReapers[bin]
			globalReapersMu.Unlock()
			if reaper != nil {
				closeMalformedRecoveryReaper(t, bin, reaper)
			}
			if reaper != nil {
				t.Fatal("unsafe reuse generation was registered with the reaper")
			}
		})
	}
}

func closeMalformedRecoveryReaper(t *testing.T, binary string, r *reaper) {
	t.Helper()
	globalReapersMu.Lock()
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	r.closeStdin()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("reaper did not exit after test cleanup")
	}
}
