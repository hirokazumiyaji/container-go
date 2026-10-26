//go:build !windows

package container

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	return readLogLines(t, p.log())
}

func dockerID(prefix byte) string {
	return string(prefix) + strings.Repeat("a", 63)
}

// runReaperScript feeds entries to the real reaper script as stdin and
// waits for it, exactly as the spawned /bin/sh child would be driven.
func runReaperScript(t *testing.T, binary, subcommand, key string, entries []string, env ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := runReaperScriptStreams(t, binary, subcommand, key, entries, env...)
	return stdout + stderr, err
}

// runReaperScriptStreams is runReaperScript with stdout and stderr kept
// apart, so a test can pin which stream carries a diagnostic.
func runReaperScriptStreams(t *testing.T, binary, subcommand, key string, entries []string, env ...string) (string, string, error) {
	t.Helper()
	return runReaperScriptWithOptions(t, binary, subcommand, key, 30, entries, env...)
}

func runReaperScriptWithOptions(t *testing.T, binary, subcommand, key string, timeoutSeconds int, entries []string, env ...string) (string, string, error) {
	t.Helper()
	requirePOSIXShell(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", reaperScript, "containergo-reaper",
		binary, subcommand, breQuote(key), strconv.Itoa(timeoutSeconds), "1")
	cmd.Stdin = strings.NewReader(strings.Join(entries, "\n") + "\n")
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// lockedEntry is a completed-entry event for a name-addressed container
// carrying the shared name-lock path that the Go registration path always
// supplies. Passing it keeps tests focused on reaping behavior instead of
// the no-lock skip.
func lockedEntry(id, creation, lockPath string) string {
	return "+\t" + id + "\t" + creation + "\t" + lockPath
}

// readLogLines returns the non-empty lines of a reaper test log.
func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
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
		lockedEntry("guarded", strings.Repeat("a", 16), filepath.Join(probe.dir, "guarded.lock")),
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

	out, err := runReaperScript(t, script, "rm", creationLabel,
		[]string{lockedEntry("guarded", creation, filepath.Join(dir, "guarded.lock"))})
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

	// Separate concurrent test binaries' timers as well as unrelated host
	// sleeps so one process cannot report another process's timer as a leak.
	timeoutSeconds := 37 + os.Getpid()
	sleepArg := "sleep " + strconv.Itoa(timeoutSeconds)

	// A reap with an entry list twice over: the second pass proves the
	// count is not drifting upward with each completed call.
	for pass := range 2 {
		before := countSleepProcesses(t, sleepArg)
		if _, _, err := runReaperScriptWithOptions(t, probe.script(), "rm", creationLabel, timeoutSeconds, entries); err != nil {
			t.Fatalf("pass %d: reaper script: %v", pass, err)
		}
		// An orphan outlives the reaper; give the process table a moment
		// to settle before counting.
		time.Sleep(300 * time.Millisecond)
		after := countSleepProcesses(t, sleepArg)
		// Any increase indicates a timer this test left behind.
		if after > before {
			t.Fatalf("pass %d: reaper-owned sleep processes went %d -> %d; the timeout left orphan helpers behind",
				pass, before, after)
		}
	}
}

// countSleepProcesses counts live processes whose argument list is
// exactly sleepArg, for example "sleep 37". Scoping the oracle to argv
// rather than every `sleep` on the host keeps unrelated workloads (some
// tools arm `sleep 0.05` continuously) out of the count while still
// seeing a leaked reaper timer.
func countSleepProcesses(t *testing.T, sleepArg string) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "args=", "-ax").Output()
	if err != nil {
		t.Skipf("ps failed: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == sleepArg {
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
	lockPath := filepath.Join(dir, "names.lock")
	var list []string
	for i := range entries {
		list = append(list, lockedEntry("ctr-"+string(rune('a'+i)), strings.Repeat("a", 16), lockPath))
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

// TestReaperScriptStartsNextEntryWhenAnyWorkerFinishes pins the worker
// pool rather than a batch wait: slow entries spread one per four-entry
// batch must overlap, so the reap takes roughly one slow entry rather than
// four. The backend logs each delete and sleeps only for the slow IDs.
func TestReaperScriptStartsNextEntryWhenAnyWorkerFinishes(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based")
	}
	requirePOSIXShell(t)
	dir := t.TempDir()
	rankDir := filepath.Join(dir, "ranks")
	if err := os.Mkdir(rankDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "backend")
	body := `#!/bin/sh
sub="$1"
shift
[ "$sub" = inspect ] && exit 0
for arg in "$@"; do target="$arg"; done

# Separate rank claims and target records avoid a shared counter or append log.
# The first, fifth, ninth, and thirteenth starts sleep. Those ranks fall in four different batches under
# the old loop but can overlap in a worker pool. awk emits entries in an
# unspecified order, so the sleep is keyed to start order, not to an ID.
rank=1
while ! mkdir "` + rankDir + `/$rank" 2>/dev/null; do rank=$((rank + 1)); done
printf '%s\n' "$target" > "` + rankDir + `/$rank/target"
case "$rank" in
  1|5|9|13) sleep 2 ;;
esac
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var entries []string
	for i := 1; i <= 16; i++ {
		entries = append(entries, fmt.Sprintf("%064x", i))
	}

	start := time.Now()
	out, err := runReaperScript(t, script, "rm", creationLabel, entries,
		"REAPER_BUDGET_SECONDS=60", "REAPER_PARALLEL=4")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	// Four batches of one 2s slow entry each take about 8s. A worker pool
	// runs all four slow entries at once and finishes with the slowest.
	if elapsed > 5*time.Second {
		t.Errorf("16 entries with four 2s slow ones took %v; workers waited in batches", elapsed)
	}
	if elapsed < 2*time.Second {
		t.Errorf("reap finished in %v, before the slowest entry could finish", elapsed)
	}

	claims, err := os.ReadDir(rankDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != len(entries) {
		t.Fatalf("start records = %d, want %d", len(claims), len(entries))
	}
	var ranks, targets []string
	for _, claim := range claims {
		rank := claim.Name()
		data, err := os.ReadFile(filepath.Join(rankDir, rank, "target"))
		if err != nil {
			t.Fatal(err)
		}
		ranks = append(ranks, rank)
		targets = append(targets, strings.TrimSpace(string(data)))
	}
	wantRanks := make([]string, 0, len(entries))
	for i := 1; i <= len(entries); i++ {
		wantRanks = append(wantRanks, fmt.Sprint(i))
	}
	slices.Sort(wantRanks)
	slices.Sort(ranks)
	if !slices.Equal(ranks, wantRanks) {
		t.Fatalf("start ranks = %v, want one of each of %v", ranks, wantRanks)
	}
	slices.Sort(targets)
	wantTargets := append([]string(nil), entries...)
	slices.Sort(wantTargets)
	if !slices.Equal(targets, wantTargets) {
		t.Fatalf("started %v, want all %d entries", targets, len(entries))
	}
}

// TestReaperScriptReportsEntriesDroppedAtTheBudget pins the budget
// diagnostic: when the overall budget expires with entries still queued,
// the script must say how many remain on stderr without naming IDs, and the
// started plus reported count must cover every registered entry.
func TestReaperScriptReportsEntriesDroppedAtTheBudget(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "deleted.log")
	script := filepath.Join(dir, "backend")
	body := `#!/bin/sh
sub="$1"
shift
[ "$sub" = inspect ] && exit 0
for arg in "$@"; do target="$arg"; done
printf '%s\n' "$target" >> "` + logPath + `"
sleep 1
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	const entries = 20
	var list []string
	for i := 1; i <= entries; i++ {
		list = append(list, fmt.Sprintf("%064x", i))
	}

	stdout, stderr, err := runReaperScriptStreams(t, script, "rm", creationLabel, list,
		"REAPER_BUDGET_SECONDS=2", "REAPER_PARALLEL=4")
	if err != nil {
		t.Fatalf("reaper script: %v (stdout: %s, stderr: %s)", err, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr = %q, want one budget line", stderr)
	}
	const prefix = "container-go: reaper budget expired with "
	if !strings.HasPrefix(lines[0], prefix) {
		t.Fatalf("stderr = %q, want a budget diagnostic", lines[0])
	}
	var remaining int
	if _, err := fmt.Sscanf(strings.TrimPrefix(lines[0], prefix), "%d", &remaining); err != nil {
		t.Fatalf("stderr = %q: %v", lines[0], err)
	}
	processed := len(readLogLines(t, logPath))
	if remaining <= 0 {
		t.Fatalf("remaining = %d, want a positive count", remaining)
	}
	if processed == 0 {
		t.Fatal("no deletes ran before the budget expired")
	}
	if processed+remaining != entries {
		t.Fatalf("processed %d + remaining %d != %d; the report lost entries", processed, remaining, entries)
	}
}

// TestReaperScriptCountsTheEntryReadWhenTheBudgetExpires pins the count
// boundary: the entry the loop already read but could not start when the
// budget expired counts as remaining. One worker runs three entries and
// blocks on the third past the budget, so the already-read fourth and the
// queued fifth are both reported.
func TestReaperScriptCountsTheEntryReadWhenTheBudgetExpires(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "deleted.log")
	counterPath := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "backend")
	body := `#!/bin/sh
sub="$1"
shift
[ "$sub" = inspect ] && exit 0
for arg in "$@"; do target="$arg"; done
calls=0
[ -f "` + counterPath + `" ] && calls=$(cat "` + counterPath + `")
calls=$((calls + 1))
printf '%s\n' "$calls" > "` + counterPath + `"
printf '%s\n' "$target" >> "` + logPath + `"
if [ "$calls" -eq 3 ]; then sleep 4; fi
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	const entries = 5
	var list []string
	for i := 1; i <= entries; i++ {
		list = append(list, fmt.Sprintf("%064x", i))
	}

	stdout, stderr, err := runReaperScriptStreams(t, script, "rm", creationLabel, list,
		"REAPER_BUDGET_SECONDS=3", "REAPER_PARALLEL=1")
	if err != nil {
		t.Fatalf("reaper script: %v (stdout: %s, stderr: %s)", err, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr = %q, want one budget line", stderr)
	}
	const prefix = "container-go: reaper budget expired with "
	if !strings.HasPrefix(lines[0], prefix) {
		t.Fatalf("stderr = %q, want a budget diagnostic", lines[0])
	}
	var remaining int
	if _, err := fmt.Sscanf(strings.TrimPrefix(lines[0], prefix), "%d", &remaining); err != nil {
		t.Fatalf("stderr = %q: %v", lines[0], err)
	}
	if processed := len(readLogLines(t, logPath)); processed != 3 {
		t.Fatalf("processed = %d, want 3 started before the budget expired", processed)
	}
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2 (the already-read fourth plus the fifth)", remaining)
	}
}

// TestReaperScriptDoesNotFabricateNameLock pins the removal of the old
// TMPDIR fallback: a name-addressed entry with no lock path must be skipped
// rather than locking a path the Go name lock does not share.
func TestReaperScriptDoesNotFabricateNameLock(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	backendLog := filepath.Join(dir, "backend.log")
	lockLog := filepath.Join(dir, "lock.log")
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A fake flock makes fallback use observable on hosts without flock,
	// where the old script would otherwise silently skip the entry.
	fakeFlock := filepath.Join(binDir, "flock")
	fakeBody := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + lockLog + "\"\nexit 0\n"
	if err := os.WriteFile(fakeFlock, []byte(fakeBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tmpDir := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpDir)

	backend := filepath.Join(dir, "backend")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + backendLog + "\"\nexit 0\n"
	if err := os.WriteFile(backend, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	const name = "guarded-name"
	out, err := runReaperScript(t, backend, "rm", creationLabel,
		[]string{"+\t" + name + "\t" + strings.Repeat("b", 16)})
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	if got := readLogLines(t, backendLog); len(got) != 0 {
		t.Errorf("backend calls = %v, want none for an entry without a lock", got)
	}
	if got := readLogLines(t, lockLog); len(got) != 0 {
		t.Errorf("lock helper calls = %v, want none; the script fabricated a lock", got)
	}
	fallback := filepath.Join(tmpDir, "containergo-"+name+".lock")
	if _, err := os.Stat(fallback); err == nil {
		t.Errorf("fallback lock %s was created", fallback)
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
		[]string{lockedEntry("mine", strings.Repeat("a", 16), filepath.Join(dir, "mine.lock"))})
	if err != nil {
		t.Fatalf("reaper script: %v (output: %s)", err, out)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("a replaced generation was deleted; the guard did not hold")
	}
}
