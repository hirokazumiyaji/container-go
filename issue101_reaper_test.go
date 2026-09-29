package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// (5) A verified failed-create candidate whose automatic delete fails is
// handed to the watchdog, so the container cannot outlive the process
// unnoticed. Diagnostic retention and shared generations stay out of the
// watchdog's ownership.
func TestFailedCreateDeletionFailureRegistersReaper(t *testing.T) {
	const creation = "0123456789abcdef"
	bin, logPath := writeGenerationReaperStub(t, creation)
	deleteErr := &cli.CLIError{Args: []string{"delete", "--force", "myctr"}, ExitCode: 1, Stderr: "delete denied"}

	runner := &externalFailRunner{
		externalBinary: bin,
		failRunRunner: &failRunRunner{
			fakeRunner:  newTestRunner(),
			runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
			inspectJSON: inspectJSONWithStateAndLabels("myctr", "created", "redis:7-alpine", sessionOwnedLabels(creation)),
			deleteErr:   deleteErr,
		},
	}
	runner.imagePresent = true
	runner.creation = creation

	dropTestReaper(t, bin)

	runErr := &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"}
	cfg := &config{runner: runner, eng: appleEngine{}, name: "myctr", creation: creation}
	if err := cleanupFailedCreate(context.Background(), cfg, runErr, runErr); err == nil {
		t.Fatal("cleanupFailedCreate = nil, want the delete failure")
	}

	// The candidate was verified owned before the delete, so the watchdog
	// now holds this exact generation.
	entries := reaperEntries(t, bin)
	if len(entries) != 1 || entries[0].id != "myctr" || entries[0].creation != creation {
		t.Fatalf("reaper entries = %+v, want the verified failed-create generation", entries)
	}

	// Parent death is the only trigger, and the reaper must then reclaim
	// the container the failed delete left behind. The stub reports this
	// exact generation, so the guard must pass and the delete run.
	if r := globalReaperFor(bin); r != nil {
		r.closeStdin()
	}
	waitForLogLines(t, logPath, "inspect myctr", "delete --force myctr")
}

// writeGenerationReaperStub creates a fake `container` binary that logs
// its argv and answers inspect with a fixed creation generation, so the
// reaper's generation guard can be exercised.
func writeGenerationReaperStub(t *testing.T, creation string) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binPath = filepath.Join(dir, "container")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo '  \"" + creationLabel + "\": \"" + creation + "\",'; fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, logPath
}

// A reuse generation is shared: the watchdog must never own it, even
// when its automatic delete fails.
func TestFailedCreateReuseDeletionFailureSkipsReaper(t *testing.T) {
	const creation = "0123456789abcdef"
	bin, _ := writeReaperStub(t)
	deleteErr := &cli.CLIError{Args: []string{"delete", "--force", "shared"}, ExitCode: 1, Stderr: "delete denied"}

	runner := &externalFailRunner{
		externalBinary: bin,
		missingAtFirst: true,
		failRunRunner: &failRunRunner{
			fakeRunner: newTestRunner(),
			runErr:     &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
			inspectJSON: inspectJSONWithStateAndLabels("shared", "created", "redis:7-alpine", map[string]string{
				managedLabel:  "true",
				reuseLabel:    "true",
				sessionLabel:  sessionID(),
				creationLabel: creation,
			}),
			deleteErr: deleteErr,
		},
	}
	runner.imagePresent = true
	runner.creation = creation

	dropTestReaper(t, bin)

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), withRunner(runner), withEngine(appleEngine{})); err == nil {
		t.Fatal("Run succeeded, want the failed create")
	}
	if r := globalReaperFor(bin); r != nil {
		t.Fatalf("shared generation registered a watchdog entry: %+v", reaperEntries(t, bin))
	}
}

// CONTAINERGO_KEEP=1 asks the caller to inspect the retained container,
// so the watchdog must not delete it later either.
func TestFailedCreateKeepDeletionFailureSkipsReaper(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	const creation = "0123456789abcdef"
	bin, _ := writeReaperStub(t)

	runner := &externalFailRunner{
		externalBinary: bin,
		failRunRunner: &failRunRunner{
			fakeRunner:  newTestRunner(),
			runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
			inspectJSON: inspectJSONWithStateAndLabels("myctr", "created", "redis:7-alpine", sessionOwnedLabels(creation)),
		},
	}
	runner.imagePresent = true
	runner.creation = creation

	dropTestReaper(t, bin)

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{})); err == nil {
		t.Fatal("Run succeeded, want the failed create")
	}
	if r := globalReaperFor(bin); r != nil {
		t.Fatalf("CONTAINERGO_KEEP registered a watchdog entry: %+v", reaperEntries(t, bin))
	}
}

// A resolved candidate is withdrawn again, so the watchdog never acts
// on an entry the parent already cleaned up.
func TestFailedCreateSuccessfulDeletionRetiresReaperEntry(t *testing.T) {
	const creation = "0123456789abcdef"
	bin, _ := writeReaperStub(t)
	runner := &externalFailRunner{
		externalBinary: bin,
		failRunRunner: &failRunRunner{
			fakeRunner:  newTestRunner(),
			runErr:      &cli.CLIError{Args: []string{"run"}, ExitCode: 125, Stderr: "entrypoint not found"},
			inspectJSON: inspectJSONWithStateAndLabels("myctr", "created", "redis:7-alpine", sessionOwnedLabels(creation)),
		},
	}
	runner.imagePresent = true
	runner.creation = creation

	dropTestReaper(t, bin)

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{})); err == nil {
		t.Fatal("Run succeeded, want the failed create")
	}
	if entries := reaperEntries(t, bin); len(entries) != 0 {
		t.Fatalf("reaper entries = %+v, want none after a successful delete", entries)
	}
}

func sessionOwnedLabels(creation string) map[string]string {
	return map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		creationLabel: creation,
	}
}

// dropTestReaper isolates one backend binary's global reaper and closes
// it when the test ends.
func dropTestReaper(t *testing.T, bin string) {
	t.Helper()
	globalReapersMu.Lock()
	delete(globalReapers, bin)
	globalReapersMu.Unlock()
	t.Cleanup(func() {
		if r := globalReaperFor(bin); r != nil {
			r.closeStdin()
		}
		globalReapersMu.Lock()
		delete(globalReapers, bin)
		globalReapersMu.Unlock()
	})
}

func reaperEntries(t *testing.T, bin string) []reaperEntry {
	t.Helper()
	r := globalReaperFor(bin)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reaperEntry(nil), r.entries...)
}

// externalFailRunner marks a fake runner as an external CLI so the
// watchdog path stays reachable in tests.
type externalFailRunner struct {
	*failRunRunner
	externalBinary string
	// missingAtFirst answers the initial reuse lookup with not-found so
	// the run reaches a create that then fails.
	missingAtFirst bool
	inspects       int
}

func (e *externalFailRunner) External() bool         { return true }
func (e *externalFailRunner) ExternalBinary() string { return e.externalBinary }

func (e *externalFailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if e.missingAtFirst && len(args) > 0 && args[0] == "inspect" {
		e.inspects++
		if e.inspects == 1 {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: fmt.Sprintf("not found: %q", args[len(args)-1])}
		}
	}
	return e.failRunRunner.Run(ctx, args...)
}

func TestReaperCancelDropsEntryFromLiveChild(t *testing.T) {
	bin, logPath := writeReaperStub(t)
	r := newReaper(bin, "delete")
	if err := r.register("cancelled-ctr", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.register("kept-ctr", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.unregister("cancelled-ctr", ""); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if got := len(r.entries); got != 1 || r.entries[0].id != "kept-ctr" {
		t.Fatalf("entries = %+v, want only the kept entry", r.entries)
	}
	r.closeStdin()
	waitForLogLines(t, logPath, "delete --force kept-ctr")
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "delete --force cancelled-ctr") {
		t.Fatalf("cancelled entry was deleted: %q", data)
	}
}

func TestReaperUnregisterUnknownEntryIsNoop(t *testing.T) {
	bin, _ := writeReaperStub(t)
	r := newReaper(bin, "delete")
	if err := r.unregister("never-registered", ""); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if got := len(r.entries); got != 0 {
		t.Fatalf("entries = %+v, want none", r.entries)
	}
	if err := r.unregister("bad id", ""); err == nil {
		t.Fatal("unregister accepted an invalid id")
	}
	r.closeStdin()
}

func TestReaperScriptHandlesCancelRecords(t *testing.T) {
	if !strings.Contains(reaperScript, `cancel\ *)`) {
		t.Error("reaper script must recognize cancel records")
	}
	if !strings.Contains(reaperScript, `cancelled "$line" && continue`) {
		t.Error("reaper script must skip cancelled entries")
	}
}
