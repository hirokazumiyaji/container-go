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

// writeReaperStub creates a fake `container` binary that logs its argv.
func writeReaperStub(t *testing.T) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binPath = filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, logPath
}

func waitForLogLines(t *testing.T, path string, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		found := true
		for _, w := range wants {
			if !strings.Contains(string(data), w) {
				found = false
				break
			}
		}
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log %s = %q, want all of %q", path, data, wants)
}

func TestReaperDeletesRegisteredContainersOnEOF(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	if err := r.register("ctr-one", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register("ctr-two", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Closing stdin is what the reaper sees when the parent process
	// dies, however it dies.
	r.closeStdin()

	waitForLogLines(t, logPath, "delete --force ctr-one", "delete --force ctr-two")
}

func TestReaperRejectsInvalidID(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer r.closeStdin()

	for _, id := range []string{"", "bad id", "a;b", "x\ny", "-leading"} {
		if err := r.register(id, ""); err == nil {
			t.Errorf("register(%q): want error", id)
		}
	}
	if err := r.register("ctr-one", "not-hex"); err == nil {
		t.Error("register bad creation: want error")
	}
}

func TestReaperRespawnsAndReplaysAfterUnexpectedExit(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")

	for _, id := range []string{"before-crash", "second-entry"} {
		if err := r.register(id, ""); err != nil {
			t.Fatalf("register %q: %v", id, err)
		}
	}

	r.mu.Lock()
	oldCmd := r.cmd
	r.mu.Unlock()
	r.killForTest()
	waitForReaperReplacement(t, r, oldCmd)

	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force before-crash", "delete --force second-entry")
}

func waitForReaperReplacement(t *testing.T, r *reaper, old *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		replaced := r.cmd != nil && r.cmd != old
		r.mu.Unlock()
		if replaced {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaper child was not replaced after unexpected exit")
}

func TestReaperIntentionalEOFDoesNotRespawn(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	if err := r.register("eof-once", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	r.mu.Lock()
	oldCmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	r.closeStdin()
	<-exited

	// Give the exit watcher time to observe EOF before checking that it
	// treated the close as intentional rather than an unexpected exit.
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	current := r.cmd
	r.mu.Unlock()
	if current != oldCmd {
		t.Fatal("reaper respawned after intentional EOF")
	}
	waitForLogLines(t, logPath, "delete --force eof-once")
}

func TestReaperRegistrationRetryReplaysRetainedEntries(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.entries = []reaperEntry{{id: "retained", creation: ""}}
	r.spawnFailures = maxReaperSpawnFailures
	r.gaveUp = true

	if err := r.register("retained", ""); err != nil {
		t.Fatalf("retry after spawn failure: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force retained")
}

const reaperTestImmutableID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type reaperBoundaryRunner struct {
	*fakeRunner
	binary string
	onRun  func([]string) ([]byte, error)
}

func (r *reaperBoundaryRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		stdout, err := r.onRun(args)
		return stdout, nil, err
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *reaperBoundaryRunner) External() bool         { return true }
func (r *reaperBoundaryRunner) ExternalBinary() string { return r.binary }

func isolateGlobalReapers(t *testing.T) {
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
			r.closeStdin()
		}
	})
}

func closeGlobalReaper(binary string) error {
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return errors.New("reaper was not registered before run")
	}
	r.closeStdin()
	return nil
}

func runCreation(args []string) string {
	for i, arg := range args {
		if arg == "--label" && i+1 < len(args) {
			if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
				return value
			}
		}
	}
	return ""
}

func writeGenerationReaperStub(t *testing.T, docker bool) (binPath, logPath, generationPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "container")
	logPath = filepath.Join(dir, "calls.log")
	generationPath = filepath.Join(dir, "generation")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  generation=$(cat " + generationPath + ")\n" +
		"  printf '    \"" + creationLabel + "\": \"%s\"\\n' \"$generation\"\n"
	if docker {
		script += "  printf '    \"Id\": \"" + reaperTestImmutableID + "\"\\n'\n"
	}
	script += "fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, logPath, generationPath
}

func TestRunPreRegistersReaperBeforeCreate(t *testing.T) {
	tests := []struct {
		name       string
		engine     engine
		docker     bool
		deleteCall string
	}{
		{name: "apple", engine: appleEngine{}, deleteCall: "delete --force pre-register-ctr"},
		{name: "docker", engine: dockerEngine{}, docker: true, deleteCall: "rm --force " + reaperTestImmutableID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, logPath, generationPath := writeGenerationReaperStub(t, tt.docker)
			isolateGlobalReapers(t)
			base := newTestRunner()
			base.imagePresent = true
			var runGeneration string
			runner := &reaperBoundaryRunner{
				fakeRunner: base,
				binary:     bin,
				onRun: func(args []string) ([]byte, error) {
					creation := runCreation(args)
					if !creationRE.MatchString(creation) {
						return nil, errors.New("run did not carry a valid generation")
					}
					runGeneration = creation
					if err := os.WriteFile(generationPath, []byte(creation), 0o600); err != nil {
						return nil, err
					}
					// Simulate the parent receiving EOF at the exact boundary
					// between backend run success and the old registration.
					if err := closeGlobalReaper(bin); err != nil {
						return nil, err
					}
					if tt.docker {
						return []byte(reaperTestImmutableID + "\n"), nil
					}
					return []byte("pre-register-ctr\n"), nil
				},
			}

			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("pre-register-ctr"),
				WithPullPolicy(PullNever),
				withRunner(runner),
				withEngine(tt.engine),
			)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr.creation != runGeneration {
				t.Fatalf("handle generation = %q, run generation = %q", ctr.creation, runGeneration)
			}
			waitForLogLines(t, logPath, "inspect pre-register-ctr", tt.deleteCall)
		})
	}
}

func TestRunPreRegistrationPreservesNameConflict(t *testing.T) {
	tests := []struct {
		name       string
		engine     engine
		docker     bool
		conflict   string
		deleteCall string
	}{
		{
			name:       "apple",
			engine:     appleEngine{},
			conflict:   `container with id "conflict-ctr" already exists`,
			deleteCall: "delete --force conflict-ctr",
		},
		{
			name:       "docker",
			engine:     dockerEngine{},
			docker:     true,
			conflict:   `Conflict. The container name "/conflict-ctr" is already in use`,
			deleteCall: "rm --force " + reaperTestImmutableID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, logPath, generationPath := writeGenerationReaperStub(t, tt.docker)
			isolateGlobalReapers(t)
			base := newTestRunner()
			base.imagePresent = true
			runner := &reaperBoundaryRunner{
				fakeRunner: base,
				binary:     bin,
				onRun: func(args []string) ([]byte, error) {
					if !creationRE.MatchString(runCreation(args)) {
						return nil, errors.New("run did not carry a valid generation")
					}
					if err := os.WriteFile(generationPath, []byte("ffffffffffffffff"), 0o600); err != nil {
						return nil, err
					}
					if err := closeGlobalReaper(bin); err != nil {
						return nil, err
					}
					return nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: tt.conflict}
				},
			}

			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("conflict-ctr"),
				WithPullPolicy(PullNever),
				withRunner(runner),
				withEngine(tt.engine),
			)
			if err == nil {
				t.Fatal("Run succeeded, want name conflict")
			}
			waitForLogLines(t, logPath, "inspect conflict-ctr")
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(logPath)
				if strings.Contains(string(data), tt.deleteCall) {
					t.Fatalf("pre-registration deleted conflicting container: %q", data)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestReaperScriptHasTimeoutAndAnchoredLabelMatch(t *testing.T) {
	if !strings.Contains(reaperScript, "sleep 30") || !strings.Contains(reaperScript, "kill -9") {
		t.Error("reaper script must bound each backend call with sleep/kill (no timeout(1))")
	}
	// The creation label must be read as a structural JSON field, anchored
	// at line start on the quoted key, and compared for exact equality.
	if !strings.Contains(reaperScript, `s/^[[:space:]]*\"$key\"[[:space:]]*:`) {
		t.Error("reaper script must anchor the creation label match on the quoted key")
	}
	if !strings.Contains(reaperScript, `[ "$got" = "$creation" ] || continue`) {
		t.Error("reaper script must compare the extracted generation exactly")
	}
}

func TestBreQuoteEscapesLabelKey(t *testing.T) {
	if got := breQuote("com.github.x-y"); got != `com\.github\.x-y` {
		t.Errorf("breQuote = %q", got)
	}
}

func TestReaperSpawnFailuresResetOnSuccess(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	r.spawnFailures = 2
	if err := r.register("ok", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	if r.spawnFailures != 0 {
		t.Errorf("spawnFailures = %d, want 0 after success (consecutive counting)", r.spawnFailures)
	}
}

func TestReaperRegisterWithCreationValidation(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	defer r.closeStdin()
	if err := r.register("good-id", "0123456789abcdef"); err != nil {
		t.Errorf("valid creation rejected: %v", err)
	}
	if err := r.register("good-id", "not-hex"); err == nil {
		t.Error("invalid creation accepted")
	}
}

func TestReaperGuardsDeleteByCreation(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/container"
	// Stub: inspect prints the creation it was told to know; delete is logged.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo '  \"" + creationLabel + "\": \"0123456789abcdef\"'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("guarded", "0123456789abcdef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register("stale", "ffffffffffffffff"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force guarded")
	// Stale generation must not be deleted; poll briefly to confirm absence.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force stale") {
			t.Fatal("stale generation was deleted; want guard to skip it")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReaperRejectsLabelValueContainingAssociation(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/container"
	oldCreation := "0123456789abcdef"
	// The live creation label differs; the old generation appears only
	// inside user label values, in both key=value and (JSON-escaped)
	// quoted forms, and under a look-alike key. None is the field.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then\n" +
		"  echo '  \"" + creationLabel + "\": \"ffffffffffffffff\",'\n" +
		"  echo '  \"user.a\": \"" + creationLabel + "=" + oldCreation + "\",'\n" +
		"  echo '  \"user.b\": \"\\\"" + creationLabel + "\\\": \\\"" + oldCreation + "\\\"\",'\n" +
		"  echo '  \"" + strings.ReplaceAll(creationLabel, ".", "-") + "\": \"" + oldCreation + "\",'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	if err := r.register("ctr", oldCreation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "inspect ctr")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "delete --force ctr") {
			t.Fatalf("reaper deleted on label value collision: %q", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReaperDeletesByImmutableID(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/calls.log"
	binPath := dir + "/docker"
	creation := "0123456789abcdef"
	uid := strings.Repeat("ab", 32)
	// Docker-style inspect: the generation matches and an immutable Id is
	// present, so the delete must target the Id rather than the name.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then\n" +
		"  echo '    \"Id\": \"" + uid + "\",'\n" +
		"  echo '      \"" + creationLabel + "\": \"" + creation + "\"'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "rm")
	if err := r.register("ctr", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "rm --force "+uid)
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "rm --force ctr") {
		t.Fatalf("reaper deleted by name despite an immutable Id: %q", data)
	}
}
