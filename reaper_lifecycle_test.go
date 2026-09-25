package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const reviewReaperDockerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func isolateReviewGlobalReapers(t *testing.T) {
	t.Helper()
	t.Setenv("CONTAINERGO_KEEP", "")
	globalReapersMu.Lock()
	previous := globalReapers
	globalReapers = make(map[string]*reaper)
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		globalReapersMu.Lock()
		current := globalReapers
		globalReapers = previous
		globalReapersMu.Unlock()
		for _, r := range current {
			waitReviewReaperExit(r)
		}
	})
}

func waitReviewReaperExit(r *reaper) {
	if r == nil {
		return
	}
	r.closeStdin()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
}

type reviewExternalRunner struct {
	*fakeRunner
	binary string
	onRun  func([]string) ([]byte, error)
}

func (r *reviewExternalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		stdout, err := r.onRun(args)
		return stdout, nil, err
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *reviewExternalRunner) External() bool         { return true }
func (r *reviewExternalRunner) ExternalBinary() string { return r.binary }

func reviewRunCreation(args []string) string {
	for i, arg := range args {
		if arg == "--label" && i+1 < len(args) {
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				return value
			}
		}
	}
	return ""
}

func writeReviewGenerationReaperStub(t *testing.T, docker bool) (bin, logPath, generationPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "container")
	logPath = filepath.Join(dir, "calls.log")
	generationPath = filepath.Join(dir, "generation")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  generation=$(cat " + generationPath + ")\n" +
		"  printf '    \"" + creationLabel + "\": \"%s\"\\n' \"$generation\"\n" +
		"  printf '    \"" + managedLabel + "\": \"true\"\\n'\n" +
		"  printf '    \"" + sessionLabel + "\": \"" + sessionID() + "\"\\n'\n" +
		"  printf '    \"state\": \"running\"\\n'\n"
	if docker {
		script += "  printf '    \"Id\": \"" + reviewReaperDockerID + "\"\\n'\n"
	}
	script += "fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath, generationPath
}

func TestReaperPreRegistrationCoversCreateBoundary(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	for _, tc := range []struct {
		name       string
		eng        engine
		docker     bool
		wantDelete string
	}{
		{name: "apple", eng: appleEngine{}, wantDelete: "delete --force review-pre-register"},
		{name: "docker", eng: dockerEngine{}, docker: true, wantDelete: "rm --force " + reviewReaperDockerID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, logPath, generationPath := writeReviewGenerationReaperStub(t, tc.docker)
			isolateReviewGlobalReapers(t)
			base := newTestRunner()
			base.imagePresent = true
			var generation string
			runner := &reviewExternalRunner{
				fakeRunner: base,
				binary:     bin,
				onRun: func(args []string) ([]byte, error) {
					generation = reviewRunCreation(args)
					if !creationRE.MatchString(generation) {
						return nil, errors.New("run did not carry a creation generation")
					}
					if err := os.WriteFile(generationPath, []byte(generation), 0o600); err != nil {
						return nil, err
					}
					globalReapersMu.Lock()
					r := globalReapers[bin]
					globalReapersMu.Unlock()
					if r == nil {
						return nil, errors.New("pre-registration did not create a reaper")
					}
					r.closeStdin() // model parent death before completion is observed
					if tc.docker {
						return []byte(reviewReaperDockerID + "\n"), nil
					}
					return []byte("review-pre-register\n"), nil
				},
			}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("review-pre-register"), WithPullPolicy(PullNever),
				withRunner(runner), withEngine(tc.eng))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr.creation != generation {
				t.Fatalf("handle generation = %q, run generation = %q", ctr.creation, generation)
			}
			waitForLogLines(t, logPath, "inspect review-pre-register", tc.wantDelete)
		})
	}
}

func TestReaperPromotesDockerNameToImmutableID(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	bin, _, generationPath := writeReviewGenerationReaperStub(t, true)
	isolateReviewGlobalReapers(t)
	base := newTestRunner()
	base.imagePresent = true
	runner := &reviewExternalRunner{
		fakeRunner: base,
		binary:     bin,
		onRun: func(args []string) ([]byte, error) {
			generation := reviewRunCreation(args)
			if err := os.WriteFile(generationPath, []byte(generation), 0o600); err != nil {
				return nil, err
			}
			return []byte(reviewReaperDockerID + "\n"), nil
		},
	}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("promote-review"), WithPullPolicy(PullNever),
		withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ctr.reapers) != 1 || ctr.reapers[0].entry.id != reviewReaperDockerID {
		t.Fatalf("registrations = %+v", ctr.reapers)
	}
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rm := base.callWith("rm")
	if len(rm) == 0 || rm[len(rm)-1] != reviewReaperDockerID {
		t.Fatalf("rm call = %v, want immutable ID", rm)
	}
	// The active registration was promoted before the explicit delete;
	// unregistering it therefore cancels the reaper record without a
	// name-addressed fallback.
}

func TestReaperPreRegistrationDoesNotDeleteConflict(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	for _, tc := range []struct {
		name   string
		eng    engine
		docker bool
		err    string
	}{
		{name: "apple", eng: appleEngine{}, err: `container with id "review-conflict" already exists`},
		{name: "docker", eng: dockerEngine{}, docker: true, err: `Conflict. The container name "/review-conflict" is already in use`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, logPath, generationPath := writeReviewGenerationReaperStub(t, tc.docker)
			if err := os.WriteFile(generationPath, []byte("ffffffffffffffff"), 0o600); err != nil {
				t.Fatal(err)
			}
			isolateReviewGlobalReapers(t)
			base := newTestRunner()
			base.imagePresent = true
			runner := &reviewExternalRunner{
				fakeRunner: base,
				binary:     bin,
				onRun: func(args []string) ([]byte, error) {
					if err := os.WriteFile(generationPath, []byte("ffffffffffffffff"), 0o600); err != nil {
						return nil, err
					}
					globalReapersMu.Lock()
					r := globalReapers[bin]
					globalReapersMu.Unlock()
					if r == nil {
						return nil, errors.New("pre-registration did not create a reaper")
					}
					r.closeStdin()
					return nil, &cli.CLIError{Binary: bin, Args: args, ExitCode: 1, Stderr: tc.err}
				},
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("review-conflict"), WithPullPolicy(PullNever),
				withRunner(runner), withEngine(tc.eng))
			if err == nil {
				t.Fatal("Run succeeded, want name conflict")
			}
			waitForLogLines(t, logPath, "inspect review-conflict")
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(logPath)
				if strings.Contains(string(data), "delete --force review-conflict") ||
					(strings.Contains(string(data), "rm --force "+reviewReaperDockerID)) {
					t.Fatalf("pre-registration deleted a conflicting object: %q", data)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestReaperUnexpectedExitReplaysEntries(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	bin, logPath, generationPath := writeReviewGenerationReaperStub(t, false)
	if err := os.WriteFile(generationPath, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	defer waitReviewReaperExit(r)
	if err := r.registerPending("review-replay", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	oldCmd := r.cmd
	r.mu.Unlock()
	if oldCmd == nil || oldCmd.Process == nil {
		t.Fatal("reaper child was not started")
	}
	if err := oldCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		replaced := r.cmd != nil && r.cmd != oldCmd
		r.mu.Unlock()
		if replaced {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	replaced := r.cmd != nil && r.cmd != oldCmd
	r.mu.Unlock()
	if !replaced {
		t.Fatal("reaper child was not replaced")
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect review-replay", "delete --force review-replay")
}

func TestReaperIntentionalEOFDoesNotRespawn(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	bin, _, generationPath := writeReviewGenerationReaperStub(t, false)
	if err := os.WriteFile(generationPath, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	if err := r.register("eof-review", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	oldCmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	r.closeStdin()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not exit after intentional EOF")
	}
	r.mu.Lock()
	current := r.cmd
	r.mu.Unlock()
	if current != oldCmd {
		t.Fatal("reaper respawned after intentional EOF")
	}
}

func TestReaperOwnedDeleteRequiresManagedSession(t *testing.T) {
	old := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = old })
	bin, logPath, generationPath := writeReviewGenerationReaperStub(t, false)
	if err := os.WriteFile(generationPath, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Replace the stub's session label with a different valid session.
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(string(data), sessionID(), "ffffffffffffffff", 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(bin, "delete")
	defer waitReviewReaperExit(r)
	if err := r.registerPendingOwned("owned-review", "0123456789abcdef", sessionID()); err != nil {
		t.Fatal(err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect owned-review")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force owned-review") {
			t.Fatalf("reaper deleted an object with a different session: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReaperBackoffRetriesAfterCooldown(t *testing.T) {
	r := newReaper("unused", "rm")
	now := time.Unix(100, 0)
	attempts := 0
	r.now = func() time.Time { return now }
	r.backoff = func(int) time.Duration { return time.Second }
	r.command = func() *exec.Cmd {
		attempts++
		if attempts <= maxReaperSpawnFailures {
			return exec.Command("/definitely/missing/reaper")
		}
		return exec.Command("/bin/sh", "-c", "cat")
	}
	uid := strings.Repeat("a", 64)
	if err := r.register(uid, ""); err == nil {
		t.Fatal("initial spawn unexpectedly succeeded")
	}
	if !r.gaveUp {
		t.Fatal("reaper did not enter cooldown")
	}
	now = now.Add(2 * time.Second)
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("retry after cooldown: %v", err)
	}
	if attempts <= maxReaperSpawnFailures {
		t.Fatalf("spawn attempts = %d, want retry", attempts)
	}
	waitReviewReaperExit(r)
}

func TestReaperBackoffRejectsRegistrationDuringCooldown(t *testing.T) {
	r := newReaper("/does/not/exist", "rm")
	now := time.Unix(100, 0)
	r.now = func() time.Time { return now }
	r.backoff = func(int) time.Duration { return time.Second }
	r.spawnFailures = maxReaperSpawnFailures
	r.gaveUp = true
	r.retryAt = now.Add(time.Second)
	if err := r.register(strings.Repeat("a", 64), ""); !errors.Is(err, errReaperSpawnCooldown) {
		t.Fatalf("register error = %v, want cooldown", err)
	}
}

var _ cli.ExternalRunner = (*reviewExternalRunner)(nil)
