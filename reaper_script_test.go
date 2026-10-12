//go:build !windows

package container

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// reaperProbe stands in for the backend CLI. It records the delete calls
// it received and can be told to never answer, which is the case the
// per-entry timeout and the overall budget exist for.
type reaperProbe struct {
	dir string
}

func newReaperProbe(t *testing.T) *reaperProbe {
	t.Helper()
	return &reaperProbe{dir: t.TempDir()}
}

func (p *reaperProbe) script() string {
	return filepath.Join(p.dir, "backend")
}

func (p *reaperProbe) log() string {
	return filepath.Join(p.dir, "deleted.log")
}

func (p *reaperProbe) install(t *testing.T) {
	t.Helper()
	body := `#!/bin/sh
sub="$1"
shift
if [ "$sub" = "inspect" ]; then
  printf '%s\n' "$*" >> "` + p.log() + `.inspect"
  exit 0
fi
printf '%s\n' "$*" >> "` + p.log() + `"
exit 0
`
	if err := os.WriteFile(p.script(), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (p *reaperProbe) deletedTargets(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(p.log())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func dockerID(prefix byte) string {
	return string(prefix) + strings.Repeat("a", 63)
}

// runReaperScript feeds entries to the real reaper script as stdin and
// waits for it, exactly as the spawned /bin/sh child would be driven.
func runReaperScript(t *testing.T, binary, subcommand, key string, entries []string, env ...string) (string, error) {
	t.Helper()
	requirePOSIXShell(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", reaperScript, "containergo-reaper",
		binary, subcommand, breQuote(key), "30", "1")
	cmd.Stdin = strings.NewReader(strings.Join(entries, "\n") + "\n")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestReaperScriptDeletesEveryRegisteredEntry is the baseline: the
// restructuring must not lose entries. Immutable Docker IDs exercise the
// unlocked delete path; a generation entry with an empty inspect is
// skipped rather than deleted.
func TestReaperScriptDeletesEveryRegisteredEntry(t *testing.T) {
	probe := newReaperProbe(t)
	probe.install(t)

	a, b := dockerID('1'), dockerID('2')
	entries := []string{
		a,
		b,
		// A generation entry makes the script inspect first. The probe
		// returns an empty inspect, so the generation cannot match and
		// this entry must be skipped rather than deleted.
		"guarded " + strings.Repeat("a", 16),
	}
	out, err := runReaperScript(t, probe.script(), "rm", creationLabel, entries)
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}

	// Entries are independent and run concurrently, so the delete order
	// is not defined; the set of deleted targets is what matters.
	got := probe.deletedTargets(t)
	slices.Sort(got)
	want := []string{"--force " + a, "--force " + b}
	if !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
}

// TestReaperScriptDeletesGenerationEntriesWhenInspectMatches covers the
// path the empty-inspect case above skips: when the reported generation
// equals the registered one, the entry is deleted.
func TestReaperScriptDeletesGenerationEntriesWhenInspectMatches(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	creation := strings.Repeat("b", 16)
	uid := strings.Repeat("c", 64)
	script := filepath.Join(dir, "backend")
	body := `#!/bin/sh
sub="$1"
shift
if [ "$sub" = "inspect" ]; then
  cat <<'JSON'
  "com.github.hirokazumiyaji.container-go.creation": "` + creation + `",
  "Id": "` + uid + `"
JSON
  exit 0
fi
printf '%s\n' "$*" >> "` + logPath + `"
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := runReaperScript(t, script, "rm", creationLabel, []string{"guarded " + creation})
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("no delete was issued: %v", err)
	}
	// Docker has an immutable ID, so the delete must target it rather
	// than the name.
	if got := strings.TrimSpace(string(data)); got != "--force "+uid {
		t.Errorf("delete = %q, want %q", got, "--force "+uid)
	}
}

// TestReaperScriptLeavesNoSleepBehind is the orphan regression. The old
// watchdog armed a `(sleep N; kill -9 $pid)` subshell and killed the
// subshell, which left its sleep child reparented to init: every entry
// leaked a sleep that outlived the reaper. The timer cleanup must not
// leave helper sleeps behind after a fast reap.
func TestReaperScriptLeavesNoSleepBehind(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
	probe := newReaperProbe(t)
	probe.install(t)

	// Enough entries that a leak per entry is unmistakable.
	var entries []string
	for i := range 12 {
		entries = append(entries, dockerID(byte('a'+i)))
	}

	// A reap with an entry list twice over: the second pass proves the
	// count is not drifting upward with each completed call.
	for pass := range 2 {
		before := countSleepProcesses(t)
		if _, err := runReaperScript(t, probe.script(), "rm", creationLabel, entries); err != nil {
			t.Fatalf("pass %d: reaper script: %v", pass, err)
		}
		// The old sleeps were 30s; give the count a moment to settle.
		time.Sleep(300 * time.Millisecond)
		after := countSleepProcesses(t)
		// Allow one unrelated host sleep; a per-entry leak would raise
		// the count by roughly len(entries).
		if after > before+1 {
			t.Fatalf("pass %d: sleep processes went %d -> %d; the timeout left orphan helpers behind",
				pass, before, after)
		}
	}
}

// countSleepProcesses counts live `sleep` processes on the host. The
// oracle is deliberately about the system rather than about the script's
// internals, because the defect being pinned is an orphan process, not
// a return value.
func countSleepProcesses(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "comm=", "-ax").Output()
	if err != nil {
		t.Skipf("ps failed: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "sleep" {
			n++
		}
	}
	return n
}

// TestReaperScriptBudgetStartsAfterEOF covers the clock starting at
// cleanup time rather than at spawn: a parent that stays alive longer
// than the budget must still get a full cleanup window after EOF.
func TestReaperScriptBudgetStartsAfterEOF(t *testing.T) {
	requirePOSIXShell(t)
	probe := newReaperProbe(t)
	probe.install(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := dockerID('9')
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", reaperScript, "containergo-reaper",
		probe.script(), "rm", breQuote(creationLabel), "30", "1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "REAPER_BUDGET_SECONDS=2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(id + "\n")); err != nil {
		t.Fatal(err)
	}
	// Hold the pipe open past the configured budget. If the clock
	// started at spawn, closing stdin now would observe a spent budget
	// and skip the entry entirely.
	time.Sleep(3 * time.Second)
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reaper script: %v", err)
	}
	if got := probe.deletedTargets(t); !slices.Contains(got, "--force "+id) {
		t.Fatalf("deleted = %v, want --force %s after a long-lived parent", got, id)
	}
}

// TestReaperScriptStopsAtTheOverallBudget covers the second half of the
// issue: a backend that answers nothing must not make the reap run for
// the container count times the per-entry timeout. The budget is what
// lets the reaper finish, since it is the last-resort cleanup and a
// wedged backend must not keep it running indefinitely.
func TestReaperScriptStopsAtTheOverallBudget(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "backend")
	inspectLog := filepath.Join(dir, "inspects")
	// The inspect is the slow call: it is the one bounded at a fixed 10s
	// timeout that the budget has to cut short of.
	body := `#!/bin/sh
sub="$1"
shift
if [ "$sub" = "inspect" ]; then
  printf 'x' >> "` + inspectLog + `"
  sleep 10
  exit 0
fi
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	// 4 entries, sequential, each carrying a generation. Without a budget
	// the loop pays the 10s inspect timeout per entry: ~40s. With the 1s
	// budget it stops after the first.
	const entries = 4
	var list []string
	for i := range entries {
		list = append(list, "ctr-"+string(rune('a'+i))+" "+strings.Repeat("a", 16))
	}

	start := time.Now()
	out, err := runReaperScript(t, script, "delete", creationLabel, list,
		"REAPER_BUDGET_SECONDS=1",
		"REAPER_PARALLEL=1",
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	if elapsed > 20*time.Second {
		t.Errorf("reap of %d entries took %v; the overall budget did not stop the entry loop", entries, elapsed)
	}
	// The loop must have stopped after the first entry's inspect, not
	// worked through the rest. Each inspect appends a byte.
	inspected := int64(0)
	if data, err := os.ReadFile(inspectLog); err == nil {
		inspected = int64(len(data))
	}
	if inspected >= entries {
		t.Errorf("the script issued %d inspects for %d entries; the budget did not stop the loop", inspected, entries)
	}
}

// TestReaperScriptFinishesWhenTheBackendNeverAnswers is the insurance
// property: whatever the backend does, the reaper process exits.
func TestReaperScriptFinishesWhenTheBackendNeverAnswers(t *testing.T) {
	// Skip in the test goroutine: requirePOSIXShell inside the worker
	// would call t.SkipNow off-goroutine and leave this select hanging.
	requirePOSIXShell(t)
	dir := t.TempDir()
	// A backend that blocks forever, ignoring signals it cannot avoid.
	script := filepath.Join(dir, "backend")
	body := "#!/bin/sh\ntrap '' TERM\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := runReaperScript(t, script, "rm", creationLabel, []string{dockerID('s')}, "REAPER_BUDGET_SECONDS=2")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reaper script: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("reaper did not exit against a backend that never answers")
	}
}

// TestReaperScriptProcessesEntriesConcurrently pins the bounded
// parallelism: N slow entries must take roughly N/parallel units, not N.
func TestReaperScriptProcessesEntriesConcurrently(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	// Each delete appends a line, sleeps 1s, then exits. Sequential
	// processing of 8 entries is ~8s; with parallelism 4 it is ~2s.
	script := filepath.Join(dir, "backend")
	body := `#!/bin/sh
if [ "$1" = "inspect" ]; then
  exit 0
fi
: >> "` + counter + `"
sleep 1
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var entries []string
	for i := range 8 {
		entries = append(entries, dockerID(byte('a'+i)))
	}

	start := time.Now()
	out, err := runReaperScript(t, script, "rm", creationLabel, entries,
		"REAPER_BUDGET_SECONDS=60", "REAPER_PARALLEL=4")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	if elapsed > 5*time.Second {
		t.Errorf("8 one-second deletes took %v; entries are not running concurrently", elapsed)
	}
}

// TestReaperScriptKeepsTheGenerationGuard covers the property the
// generation check exists for, which the restructure must preserve: an
// entry whose generation no longer matches is not deleted, so a
// same-name replacement made by another process survives.
func TestReaperScriptKeepsTheGenerationGuard(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	script := filepath.Join(dir, "backend")
	body := `#!/bin/sh
sub="$1"
shift
if [ "$sub" = "inspect" ]; then
  echo '  "com.github.hirokazumiyaji.container-go.creation": "ffffffffffffffff",'
  exit 0
fi
printf '%s\n' "$*" >> "` + logPath + `"
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := runReaperScript(t, script, "delete", creationLabel,
		[]string{"mine " + strings.Repeat("a", 16)})
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("a replaced generation was deleted; the guard did not hold")
	}
}
