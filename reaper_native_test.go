package container

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The native reaper tests use this test binary as the backend executable: a
// small init hook turns it into a fake CLI when the marker below is set. That
// exercises the real self-execution path on every platform, including
// Windows, instead of depending on /bin/sh.
const (
	testNativeReaperBackend          = "CONTAINERGO_TEST_NATIVE_REAPER_BACKEND"
	testNativeReaperLog              = "CONTAINERGO_TEST_NATIVE_REAPER_LOG"
	testNativeReaperInspectJSON      = "CONTAINERGO_TEST_NATIVE_REAPER_INSPECT_JSON"
	testNativeReaperInspectCounter   = "CONTAINERGO_TEST_NATIVE_REAPER_INSPECT_COUNTER"
	testNativeReaperInspectFailCount = "CONTAINERGO_TEST_NATIVE_REAPER_INSPECT_FAIL_COUNT"
	testNativeReaperInspectSleepMS   = "CONTAINERGO_TEST_NATIVE_REAPER_INSPECT_SLEEP_MS"
	testNativeReaperDeleteSleepMS    = "CONTAINERGO_TEST_NATIVE_REAPER_DELETE_SLEEP_MS"
	testNativeReaperRankDir          = "CONTAINERGO_TEST_NATIVE_REAPER_RANK_DIR"
	testNativeReaperRankSleepMS      = "CONTAINERGO_TEST_NATIVE_REAPER_RANK_SLEEP_MS"
	testNativeReaperBusyDir          = "CONTAINERGO_TEST_NATIVE_REAPER_BUSY_DIR"

	testNativeReaperParentCommand = "containergo-test-native-reaper-parent"
	testNativeReaperParentID      = "CONTAINERGO_TEST_NATIVE_REAPER_PARENT_ID"
	testNativeReaperParentReady   = "CONTAINERGO_TEST_NATIVE_REAPER_PARENT_READY"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == testNativeReaperParentCommand {
		os.Exit(runTestNativeReaperParent())
	}
	if os.Getenv(testNativeReaperBackend) != "1" || len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "inspect", "rm", "delete":
	default:
		return
	}
	os.Exit(runTestNativeReaperBackend())
}

// runTestNativeReaperParent holds a live reaper until the test kills this
// process, which closes the stdin pipe and gives the reaper an EOF the way a
// real parent death does.
func runTestNativeReaperParent() int {
	exe, err := os.Executable()
	if err != nil {
		return 2
	}
	r := newNativeReaper(exe, "rm")
	if err := r.register(os.Getenv(testNativeReaperParentID), ""); err != nil {
		return 1
	}
	if err := os.WriteFile(os.Getenv(testNativeReaperParentReady), nil, 0o600); err != nil {
		return 1
	}
	for {
		time.Sleep(time.Hour)
	}
}

func runTestNativeReaperBackend() int {
	args := os.Args[1:]
	logPath := os.Getenv(testNativeReaperLog)
	if args[0] == "inspect" {
		if logPath != "" {
			_ = appendTestLine(logPath+".inspect", strings.Join(args[1:], " "))
		}
		if counter := os.Getenv(testNativeReaperInspectCounter); counter != "" {
			count := incrementTestFile(counter)
			failCount, _ := strconv.Atoi(os.Getenv(testNativeReaperInspectFailCount))
			if count <= failCount {
				return 1
			}
		}
		sleepTestMS(os.Getenv(testNativeReaperInspectSleepMS))
		fmt.Print(os.Getenv(testNativeReaperInspectJSON))
		return 0
	}
	target := args[len(args)-1]
	if busyDir := os.Getenv(testNativeReaperBusyDir); busyDir != "" {
		marker, err := os.OpenFile(filepath.Join(busyDir, "busy"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if logPath != "" {
				_ = appendTestLine(logPath+".overlap", target)
			}
			return 1
		}
		_ = marker.Close()
		sleepTestMS(os.Getenv(testNativeReaperDeleteSleepMS))
		_ = os.Remove(filepath.Join(busyDir, "busy"))
	}
	if logPath != "" {
		_ = appendTestLine(logPath, strings.Join(args[1:], " "))
	}
	if rankDir := os.Getenv(testNativeReaperRankDir); rankDir != "" {
		if rank := claimTestRank(rankDir, target); rank == 1 || rank == 5 || rank == 9 || rank == 13 {
			sleepTestMS(os.Getenv(testNativeReaperRankSleepMS))
		}
	} else if os.Getenv(testNativeReaperBusyDir) == "" {
		sleepTestMS(os.Getenv(testNativeReaperDeleteSleepMS))
	}
	return 0
}

func appendTestLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

func incrementTestFile(path string) int {
	data, _ := os.ReadFile(path)
	count, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	count++
	_ = os.WriteFile(path, []byte(strconv.Itoa(count)+"\n"), 0o600)
	return count
}

func sleepTestMS(value string) {
	ms, err := strconv.Atoi(value)
	if err != nil || ms <= 0 {
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func claimTestRank(dir, target string) int {
	for rank := 1; rank < 10_000; rank++ {
		path := filepath.Join(dir, strconv.Itoa(rank))
		if err := os.Mkdir(path, 0o700); err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(path, "target"), []byte(target+"\n"), 0o600)
		return rank
	}
	return 0
}

func readNativeReaperLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func waitForNativeReaperLog(t *testing.T, path string, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		found := true
		for _, want := range wants {
			if !strings.Contains(string(data), want) {
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

// waitForNativeReaperExit waits for the self-executed child to finish after
// an intentional EOF, so a Windows test binary does not outlive the test and
// keep the executable image locked during go test cleanup.
func waitForNativeReaperExit(t *testing.T, r *reaper) {
	t.Helper()
	r.mu.Lock()
	exited := r.exited
	r.mu.Unlock()
	if exited == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("native reaper child did not exit")
	}
}

func waitForTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func nativeReaperID(prefix byte) string {
	return string(prefix) + strings.Repeat("a", 63)
}

func nativeReaperName(prefix string) string {
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// nativeReaperBackend prepares the test binary as the backend CLI and
// returns a reaper config that runs inside this process.
func nativeReaperBackend(t *testing.T, subcommand string) (nativeReaperConfig, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	return nativeReaperConfig{
		binary:          exe,
		subcommand:      subcommand,
		key:             creationLabel,
		timeout:         10 * time.Second,
		pendingAttempts: 4,
		lockTimeout:     time.Second,
	}, logPath
}

func nativeReaperDockerInspect(id, creation string) string {
	return fmt.Sprintf(`[{"Id":%q,"Config":{"Labels":{%q:%q}}}]`, id, creationLabel, creation)
}

func TestParseNativeReaperEvent(t *testing.T) {
	tests := []struct {
		line     string
		event    string
		id       string
		creation string
		lock     string
		ok       bool
	}{
		{line: "plain-id\n", event: "+", id: "plain-id", ok: true},
		{line: "+\tentry\t0123456789abcdef\t/lock\n", event: "+", id: "entry", creation: "0123456789abcdef", lock: "/lock", ok: true},
		{line: "+\tentry\t-\n", event: "+", id: "entry", ok: true},
		{line: "P\tsettling\t0123456789abcdef\t/lock\n", event: "P", id: "settling", creation: "0123456789abcdef", lock: "/lock", ok: true},
		{line: "C\tsettling\t0123456789abcdef\n", event: "C", id: "settling", creation: "0123456789abcdef", ok: true},
		{line: "S\tshared\t0123456789abcdef\n", event: "S", id: "shared", creation: "0123456789abcdef", ok: true},
		{line: "-\tgone\t-\n", event: "-", id: "gone", ok: true},
		{line: "legacy 0123456789abcdef /lock\n", event: "+", id: "legacy", creation: "0123456789abcdef", lock: "/lock", ok: true},
		{line: "   \n", ok: false},
	}
	for _, tc := range tests {
		event, id, creation, lock, ok := parseNativeReaperEvent(tc.line)
		if ok != tc.ok || event != tc.event || id != tc.id || creation != tc.creation || lock != tc.lock {
			t.Errorf("parseNativeReaperEvent(%q) = (%q, %q, %q, %q, %v), want (%q, %q, %q, %q, %v)",
				tc.line, event, id, creation, lock, ok, tc.event, tc.id, tc.creation, tc.lock, tc.ok)
		}
	}
}

func TestReadNativeReaperEntriesAppliesEvents(t *testing.T) {
	creation := strings.Repeat("1", 16)
	other := strings.Repeat("2", 16)
	stream := strings.Join([]string{
		"P\tpending\t" + creation + "\t/lock",
		"C\tpending\t" + creation + "\t/lock",
		"+\tactive\t" + creation + "\t/lock",
		"+\tshared\t" + other + "\t/lock",
		"S\tshared\t" + other + "\t/lock",
		"+\tgone\t-\t",
		"-\tgone\t-\t",
	}, "\n") + "\n"
	entries := readNativeReaperEntries(strings.NewReader(stream))
	want := []nativeReaperEntry{
		{id: "pending", creation: creation, lockPath: "/lock"},
		{id: "active", creation: creation, lockPath: "/lock"},
		{id: "shared", creation: other, lockPath: "/lock", shared: true},
	}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
}

func TestNativeReaperDeletesEveryRegisteredEntry(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	a, b := nativeReaperID('1'), nativeReaperID('2')
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader(a+"\n"+b+"\n"), &stderr)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := readNativeReaperLog(t, logPath)
	slices.Sort(got)
	want := []string{"--force " + a, "--force " + b}
	if !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestNativeReaperDeletesGenerationEntriesWhenInspectMatches(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	creation := strings.Repeat("b", 16)
	target := nativeReaperID('c')
	t.Setenv(testNativeReaperInspectJSON, nativeReaperDockerInspect(target, creation))

	entry := nativeReaperID('d')
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader("+\t"+entry+"\t"+creation+"\n"), &stderr)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	got := readNativeReaperLog(t, logPath)
	want := []string{"--force " + target}
	if !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v; stderr: %s", got, want, stderr.String())
	}
}

func TestNativeReaperKeepsTheGenerationGuard(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	t.Setenv(testNativeReaperInspectJSON, nativeReaperDockerInspect(nativeReaperID('e'), strings.Repeat("f", 16)))
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader("+\t"+nativeReaperID('d')+"\t"+strings.Repeat("1", 16)+"\n"), &stderr)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 0 {
		t.Fatalf("deleted %v, want no delete for a replaced generation", got)
	}
}

func TestNativeReaperIgnoresNestedIDFields(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	creation := strings.Repeat("b", 16)
	target := nativeReaperID('c')
	nested := nativeReaperID('d')
	body := fmt.Sprintf(`[{"Id":%q,"Example":{"Id":%q},"Config":{"Labels":{%q:%q}}}]`,
		target, nested, creationLabel, creation)
	t.Setenv(testNativeReaperInspectJSON, body)
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader("+\t"+nativeReaperID('a')+"\t"+creation+"\n"), &stderr)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	got := readNativeReaperLog(t, logPath)
	want := []string{"--force " + target}
	if !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want the top-level Id %v; stderr: %s", got, want, stderr.String())
	}
}

func TestNativeReaperPendingRegistrationRetriesSettlingCreate(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("subprocess retry; worker concurrency is covered by the quick delete tests")
	}
	cfg, logPath := nativeReaperBackend(t, "rm")
	creation := strings.Repeat("b", 16)
	target := nativeReaperID('c')
	counter := filepath.Join(t.TempDir(), "inspect-count")
	t.Setenv(testNativeReaperInspectCounter, counter)
	t.Setenv(testNativeReaperInspectFailCount, "1")
	t.Setenv(testNativeReaperInspectJSON, nativeReaperDockerInspect(target, creation))

	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader("P\t"+nativeReaperID('d')+"\t"+creation+"\n"), &stderr)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	got := readNativeReaperLog(t, logPath)
	want := []string{"--force " + target}
	if !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v; stderr: %s", got, want, stderr.String())
	}
	if counts := len(readNativeReaperLog(t, logPath+".inspect")); counts < 2 {
		t.Fatalf("inspected %d times, want at least 2 for a settling create", counts)
	}
}

func TestNativeReaperSkipsSharedAndRemovedEntries(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	shared, removed := nativeReaperID('a'), nativeReaperID('b')
	stream := strings.Join([]string{
		"+\t" + shared + "\t-",
		"S\t" + shared + "\t-",
		"+\t" + removed + "\t-",
		"-\t" + removed + "\t-",
	}, "\n") + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(stream), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 0 {
		t.Fatalf("deleted %v, want shared and removed entries skipped", got)
	}
}

func TestNativeReaperSkipsUnlockedAppleEntry(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "delete")
	stream := "+\tunlocked-apple\t" + strings.Repeat("a", 16) + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(stream), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 0 {
		t.Fatalf("backend calls = %v, want none for an unlocked Apple entry", got)
	}
	if got := readNativeReaperLog(t, logPath+".inspect"); len(got) != 0 {
		t.Fatalf("inspect calls = %v, want none for an unlocked Apple entry", got)
	}
}

func TestNativeReaperStopsAtTheOverallBudget(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("subprocess timing; no shared state under test")
	}
	cfg, logPath := nativeReaperBackend(t, "rm")
	cfg.timeout = 5 * time.Second
	t.Setenv(testNativeReaperInspectSleepMS, "1500")
	t.Setenv(testNativeReaperInspectJSON, "[]")
	t.Setenv("REAPER_BUDGET_SECONDS", "1")
	t.Setenv("REAPER_PARALLEL", "1")

	creation := strings.Repeat("b", 16)
	var entries []string
	for i := 1; i <= 4; i++ {
		entries = append(entries, fmt.Sprintf("+\t%064x\t%s", i, creation))
	}
	start := time.Now()
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader(strings.Join(entries, "\n")+"\n"), &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if elapsed > 10*time.Second {
		t.Errorf("reap took %v; the overall budget did not stop the entry loop", elapsed)
	}
	if inspected := len(readNativeReaperLog(t, logPath+".inspect")); inspected >= len(entries) {
		t.Errorf("inspected %d entries, want fewer than %d", inspected, len(entries))
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "container-go: reaper budget expired with ") {
		t.Fatalf("stderr = %q, want one budget diagnostic", stderr.String())
	}
	var remaining int
	if _, err := fmt.Sscanf(lines[0], "container-go: reaper budget expired with %d entries remaining", &remaining); err != nil {
		t.Fatalf("stderr = %q: %v", lines[0], err)
	}
	if remaining != 3 {
		t.Fatalf("remaining = %d, want 3", remaining)
	}
}

func TestNativeReaperProcessesEntriesConcurrently(t *testing.T) {
	if testing.Short() || raceDetectorEnabled {
		t.Skip("timing-based")
	}
	cfg, logPath := nativeReaperBackend(t, "rm")
	cfg.timeout = 10 * time.Second
	t.Setenv(testNativeReaperDeleteSleepMS, "1000")
	t.Setenv("REAPER_BUDGET_SECONDS", "60")
	t.Setenv("REAPER_PARALLEL", "4")

	var entries []string
	for i := 1; i <= 8; i++ {
		entries = append(entries, fmt.Sprintf("%064x", i))
	}
	start := time.Now()
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader(strings.Join(entries, "\n")+"\n"), &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if elapsed > 6*time.Second {
		t.Errorf("8 one-second deletes took %v; entries are not running concurrently", elapsed)
	}
	if got := readNativeReaperLog(t, logPath); len(got) != len(entries) {
		t.Fatalf("deleted %d entries, want %d", len(got), len(entries))
	}
}

func TestNativeReaperStartsNextEntryWhenAnyWorkerFinishes(t *testing.T) {
	if testing.Short() || raceDetectorEnabled {
		t.Skip("timing-based")
	}
	cfg, logPath := nativeReaperBackend(t, "rm")
	cfg.timeout = 10 * time.Second
	rankDir := filepath.Join(t.TempDir(), "ranks")
	if err := os.Mkdir(rankDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(testNativeReaperRankDir, rankDir)
	t.Setenv(testNativeReaperRankSleepMS, "2000")
	t.Setenv("REAPER_BUDGET_SECONDS", "60")
	t.Setenv("REAPER_PARALLEL", "4")

	const count = 16
	var entries []string
	for i := 1; i <= count; i++ {
		entries = append(entries, fmt.Sprintf("%064x", i))
	}
	start := time.Now()
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader(strings.Join(entries, "\n")+"\n"), &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if elapsed > 6*time.Second {
		t.Errorf("16 entries with four 2s slow ones took %v; workers waited in batches", elapsed)
	}
	if elapsed < 2*time.Second {
		t.Errorf("reap finished in %v, before the slowest entry could finish", elapsed)
	}
	deleted := readNativeReaperLog(t, logPath)
	if len(deleted) != count {
		t.Fatalf("deleted %d entries, want %d", len(deleted), count)
	}
}

func TestNativeReaperFinishesWhenTheBackendNeverAnswers(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	cfg.timeout = 500 * time.Millisecond
	t.Setenv(testNativeReaperDeleteSleepMS, "60000")

	id := nativeReaperID('a')
	start := time.Now()
	var stderr strings.Builder
	code := runNativeReaper(cfg, strings.NewReader(id+"\n"), &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("reap returned in %v; the backend call was skipped rather than timed out", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("reap took %v against a backend that never answers", elapsed)
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 1 || got[0] != "--force "+id {
		t.Fatalf("backend calls = %v, want the timed-out delete to have started", got)
	}
}

func TestNativeReaperSelfExecDeletesRegisteredContainers(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	r := newNativeReaper(exe, "rm")
	uid := nativeReaperID('a')
	if err := r.register(uid, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForNativeReaperLog(t, logPath, "--force "+uid)
	waitForNativeReaperExit(t, r)
}

func TestNativeReaperSelfExecRespawnsAndReplays(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backend.log")
	t.Setenv(testNativeReaperBackend, "1")
	t.Setenv(testNativeReaperLog, logPath)
	r := newNativeReaper(exe, "rm")
	t.Cleanup(r.closeStdin)
	if err := r.register(nativeReaperID('b'), ""); err != nil {
		t.Fatalf("register before crash: %v", err)
	}
	r.killForTest()
	after := nativeReaperID('c')
	if err := r.register(after, ""); err != nil {
		t.Fatalf("register after crash: %v", err)
	}
	r.closeStdin()
	waitForNativeReaperLog(t, logPath, "--force "+nativeReaperID('b'), "--force "+after)
	waitForNativeReaperExit(t, r)
}

func TestIsInternalReaperInvocation(t *testing.T) {
	tests := []struct {
		marker string
		args   []string
		want   bool
	}{
		{marker: "1", args: []string{"prog", reaperInternalCommand}, want: true},
		{marker: "1", args: []string{"prog", "other"}, want: false},
		{marker: "1", args: []string{"prog"}, want: false},
		{marker: "0", args: []string{"prog", reaperInternalCommand}, want: false},
		{marker: "", args: []string{"prog", reaperInternalCommand}, want: false},
	}
	for _, tc := range tests {
		if got := isInternalReaperInvocation(tc.marker, tc.args); got != tc.want {
			t.Errorf("isInternalReaperInvocation(%q, %v) = %v, want %v", tc.marker, tc.args, got, tc.want)
		}
	}
}

func TestNativeReaperBackendEnvStripsMarker(t *testing.T) {
	t.Setenv(reaperInternalEnv, "1")
	t.Setenv("CONTAINERGO_TEST_NATIVE_REAPER_KEEP", "keep")
	var kept bool
	for _, entry := range nativeReaperBackendEnv() {
		if strings.HasPrefix(entry, reaperInternalEnv+"=") {
			t.Fatalf("backend environment retained %s", entry)
		}
		if entry == "CONTAINERGO_TEST_NATIVE_REAPER_KEEP=keep" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("backend environment dropped an unrelated variable")
	}
}

func TestNativeReaperBuildModeGuard(t *testing.T) {
	for _, mode := range []string{"", "exe", "pie"} {
		if !nativeReaperBuildModeSupported(mode) {
			t.Errorf("build mode %q should support self-execution", mode)
		}
	}
	for _, mode := range []string{"c-shared", "c-archive", "plugin", "shared", "unknown"} {
		if nativeReaperBuildModeSupported(mode) {
			t.Errorf("build mode %q must not self-execute the host process", mode)
		}
	}
	if !nativeReaperSupported() {
		t.Fatal("this test binary should be a supported self-execution host")
	}
}

func TestReaperUnavailableFor(t *testing.T) {
	tests := []struct {
		native bool
		shell  bool
		want   bool
	}{
		{native: true, shell: true, want: false},
		{native: true, shell: false, want: false},
		{native: false, shell: true, want: false},
		{native: false, shell: false, want: true},
	}
	for _, tc := range tests {
		if got := reaperUnavailableFor(tc.native, tc.shell); got != tc.want {
			t.Errorf("reaperUnavailableFor(native=%v, shell=%v) = %v, want %v", tc.native, tc.shell, got, tc.want)
		}
	}
}

func TestNativeReaperDeletesUnlockedDockerNameByInspectedID(t *testing.T) {
	creation := strings.Repeat("b", 16)
	name := "unlocked-docker-name"
	target := nativeReaperID('c')
	inspect := nativeReaperDockerInspect(target, creation)

	t.Run("completed", func(t *testing.T) {
		cfg, logPath := nativeReaperBackend(t, "rm")
		t.Setenv(testNativeReaperInspectJSON, inspect)
		stream := "+\t" + name + "\t" + creation + "\n"
		var stderr strings.Builder
		if code := runNativeReaper(cfg, strings.NewReader(stream), &stderr); code != 0 {
			t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
		}
		if got := readNativeReaperLog(t, logPath); len(got) != 1 || got[0] != "--force "+target {
			t.Fatalf("deleted %v, want %q by the inspected immutable ID", got, "--force "+target)
		}
	})

	t.Run("pending", func(t *testing.T) {
		if raceDetectorEnabled {
			t.Skip("subprocess retry; no shared state under test")
		}
		cfg, logPath := nativeReaperBackend(t, "rm")
		counter := filepath.Join(t.TempDir(), "inspect-count")
		t.Setenv(testNativeReaperInspectCounter, counter)
		t.Setenv(testNativeReaperInspectFailCount, "1")
		t.Setenv(testNativeReaperInspectJSON, inspect)
		stream := "P\t" + name + "\t" + creation + "\n"
		var stderr strings.Builder
		if code := runNativeReaper(cfg, strings.NewReader(stream), &stderr); code != 0 {
			t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
		}
		if got := readNativeReaperLog(t, logPath); len(got) != 1 || got[0] != "--force "+target {
			t.Fatalf("deleted %v, want %q after the create settled", got, "--force "+target)
		}
	})
}

func TestNativeReaperSkipsCompletedWhenInspectIsUnreadable(t *testing.T) {
	creation := strings.Repeat("b", 16)
	entry := "+\t" + fmt.Sprintf("%064x", 1) + "\t" + creation + "\n"
	tests := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "absent",
			setup: func(t *testing.T) {
				t.Setenv(testNativeReaperInspectJSON, "[]")
			},
		},
		{
			name: "malformed",
			setup: func(t *testing.T) {
				t.Setenv(testNativeReaperInspectJSON, "not json")
			},
		},
		{
			name: "failed",
			setup: func(t *testing.T) {
				counter := filepath.Join(t.TempDir(), "inspect-count")
				t.Setenv(testNativeReaperInspectCounter, counter)
				t.Setenv(testNativeReaperInspectFailCount, "1")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logPath := nativeReaperBackend(t, "rm")
			tc.setup(t)
			var stderr strings.Builder
			if code := runNativeReaper(cfg, strings.NewReader(entry), &stderr); code != 0 {
				t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
			}
			if got := readNativeReaperLog(t, logPath); len(got) != 0 {
				t.Fatalf("deleted %v, want no delete when inspect is unreadable", got)
			}
		})
	}
}

func TestNativeReaperSerializesSharedLock(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("subprocess timing; the lock path is not shared memory")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no name-lock namespace")
	}
	cfg, logPath := nativeReaperBackend(t, "rm")
	cfg.lockTimeout = 5 * time.Second
	creation := strings.Repeat("b", 16)
	target := nativeReaperID('c')
	t.Setenv(testNativeReaperInspectJSON, nativeReaperDockerInspect(target, creation))
	busyDir := t.TempDir()
	t.Setenv(testNativeReaperBusyDir, busyDir)
	t.Setenv(testNativeReaperDeleteSleepMS, "1000")
	t.Setenv("REAPER_PARALLEL", "4")

	// A real name-lock bucket, so the test covers the shared namespace the
	// Go generation-checked deletes also lock.
	lockPath, err := reaperNameLockPath(nativeReaperName("shared-bucket"))
	if err != nil {
		t.Fatalf("reaperNameLockPath: %v", err)
	}
	entries := []string{
		fmt.Sprintf("+\t%064x\t%s\t%s", 1, creation, lockPath),
		fmt.Sprintf("+\t%064x\t%s\t%s", 2, creation, lockPath),
	}
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(strings.Join(entries, "\n")+"\n"), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := readNativeReaperLog(t, logPath+".overlap"); len(got) != 0 {
		t.Fatalf("lock was not held across the delete: overlapping targets %v", got)
	}
	if got := readNativeReaperLog(t, logPath); len(got) != len(entries) {
		t.Fatalf("deleted %d entries, want %d", len(got), len(entries))
	}
}

func TestNativeReaperReapsAfterParentDeath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "backend.log")
	readyPath := filepath.Join(dir, "ready")
	uid := nativeReaperID('a')
	cmd := exec.Command(exe, testNativeReaperParentCommand)
	cmd.Env = append(os.Environ(),
		testNativeReaperBackend+"=1",
		testNativeReaperLog+"="+logPath,
		testNativeReaperParentID+"="+uid,
		testNativeReaperParentReady+"="+readyPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	waitForTestPath(t, readyPath)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitForNativeReaperLog(t, logPath, "--force "+uid)
}

func TestReaperBudgetDurationClampsOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("a duration overflow needs a 64-bit int")
	}
	t.Setenv("REAPER_BUDGET_SECONDS", "9223372036854775807")
	budget := reaperBudgetDuration()
	if budget <= 0 {
		t.Fatalf("budget = %v, want a positive clamped duration", budget)
	}
	const maxSeconds = int64(math.MaxInt64) / int64(time.Second)
	if want := time.Duration(maxSeconds) * time.Second; budget != want {
		t.Fatalf("budget = %v, want the clamped maximum %v", budget, want)
	}
	t.Setenv("REAPER_BUDGET_SECONDS", "0")
	if got := reaperBudgetDuration(); got != 120*time.Second {
		t.Fatalf("budget = %v, want the 120s default", got)
	}
}

func TestNativeReaperFeatureFlag(t *testing.T) {
	if !nativeReaperSupported() {
		t.Skip("self-execution is unavailable in this build")
	}
	t.Setenv(reaperNativeEnv, "1")
	if !nativeReaperEnabled() {
		t.Fatal("native reaper was not enabled by the feature flag")
	}
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv(reaperNativeEnv, "")
	if nativeReaperEnabled() {
		t.Fatal("native reaper was enabled without the feature flag on a Unix host")
	}
}

func TestNativeReaperRejectsInvalidConfiguration(t *testing.T) {
	var stderr strings.Builder
	if code := runNativeReaper(nativeReaperConfig{}, strings.NewReader(""), &stderr); code == 0 {
		t.Fatal("runNativeReaper accepted an empty configuration")
	}
	if !strings.Contains(stderr.String(), "invalid configuration") {
		t.Fatalf("stderr = %q, want an invalid-configuration diagnostic", stderr.String())
	}
}

func TestNativeReaperRejectsInvalidEntry(t *testing.T) {
	cfg, logPath := nativeReaperBackend(t, "rm")
	stream := "+\tbad id\t" + strings.Repeat("a", 16) + "\n"
	var stderr strings.Builder
	if code := runNativeReaper(cfg, strings.NewReader(stream), &stderr); code != 0 {
		t.Fatalf("runNativeReaper = %d; stderr: %s", code, stderr.String())
	}
	if got := readNativeReaperLog(t, logPath); len(got) != 0 {
		t.Fatalf("backend calls = %v, want none for an invalid ID", got)
	}
}

func TestNativeReaperSelfExecRejectsInvalidArguments(t *testing.T) {
	var stderr strings.Builder
	if code := runNativeReaperMain(nil, &stderr); code == 0 {
		t.Fatal("runNativeReaperMain accepted empty arguments")
	}
	if !strings.Contains(stderr.String(), "invalid arguments") {
		t.Fatalf("stderr = %q, want an invalid-arguments diagnostic", stderr.String())
	}
}

func TestParseNativeReaperInspect(t *testing.T) {
	creation := strings.Repeat("b", 16)
	uid := nativeReaperID('c')
	if gotUID, gotCreation := parseNativeReaperInspect([]byte(nativeReaperDockerInspect(uid, creation)), "rm", creationLabel); gotUID != uid || gotCreation != creation {
		t.Errorf("Docker inspect parse = (%q, %q), want (%q, %q)", gotUID, gotCreation, uid, creation)
	}
	apple := fmt.Sprintf(`[{"id":"ctr","configuration":{"labels":{%q:%q}}}]`, creationLabel, creation)
	if gotUID, gotCreation := parseNativeReaperInspect([]byte(apple), "delete", creationLabel); gotUID != "" || gotCreation != creation {
		t.Errorf("Apple inspect parse = (%q, %q), want (\"\", %q)", gotUID, gotCreation, creation)
	}
	if _, gotCreation := parseNativeReaperInspect([]byte("not json"), "rm", creationLabel); gotCreation != "" {
		t.Errorf("unreadable inspect parsed as creation %q", gotCreation)
	}
}
