//go:build darwin || linux

package container

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func reaperTestShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func reaperTestCreateFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func reaperTestOpenFIFO(t *testing.T, path string, flag int) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func waitForReaperTestFIFOEOF(t *testing.T, file *os.File) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, err := file.Read(buf[:])
		result <- err
	}()
	select {
	case err := <-result:
		if err != io.EOF {
			t.Fatalf("timeout helper lifetime FIFO read = %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout helper lifetime FIFO remained open")
	}
}

func waitForReaperTestReadyFiles(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		count := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ready-") {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, _ := os.ReadDir(dir)
	t.Fatalf("timeout helper readiness files = %v, want at least %d", entries, want)
}

func TestReaperSetsidHelper(t *testing.T) {
	if os.Getenv("CONTAINERGO_REAPER_SETSID_HELPER") != "1" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		os.Exit(2)
	}
	path := os.Getenv("CONTAINERGO_REAPER_SETSID_PID_PATH")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestReaperHeadlessDashKillsLiveNestedCommand(t *testing.T) {
	if _, err := os.Stat("/bin/dash"); err != nil {
		t.Skipf("/bin/dash unavailable: %v", err)
	}
	dir := t.TempDir()
	nestedPIDPath := filepath.Join(dir, "nested.pid")
	siblingPIDPath := filepath.Join(dir, "sibling.pid")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = delete ]; then\n" +
		"  CONTAINERGO_REAPER_SETSID_HELPER=1 CONTAINERGO_REAPER_SETSID_PID_PATH=" + reaperTestShellQuote(nestedPIDPath) + " " + reaperTestShellQuote(os.Args[0]) + " -test.run '^TestReaperSetsidHelper$' >/dev/null 2>&1 &\n" +
		"  nested=$!\n" +
		"  /bin/sleep 30 &\n" +
		"  sibling=$!\n" +
		"  printf '%s\\n' \"$sibling\" > " + reaperTestShellQuote(siblingPIDPath) + "\n" +
		"  wait \"$nested\"\n" +
		"  wait \"$sibling\"\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.timeoutSeconds = 1
	r.command = func() *exec.Cmd {
		return exec.Command("/bin/dash", "-c", reaperScript, "containergo-reaper", binPath, "delete", breQuote(creationLabel), "1")
	}
	if err := r.register("headless", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	nestedPID := waitForReaperTestPIDFile(t, nestedPIDPath)
	siblingPID := waitForReaperTestPIDFile(t, siblingPIDPath)
	waitForReaperTestProcessGone(t, nestedPID)
	waitForReaperTestProcessGone(t, siblingPID)
	waitForReaperExit(t, r)
}

func TestReaperTimeoutIgnoresPathShadowedPgrep(t *testing.T) {
	if _, err := os.Stat("/bin/dash"); err != nil {
		t.Skipf("/bin/dash unavailable: %v", err)
	}
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "pgrep-invoked")
	pgrepPath := filepath.Join(dir, "pgrep")
	pidPath := filepath.Join(dir, "backend.pid")
	if err := os.WriteFile(pgrepPath, []byte("#!/bin/sh\nprintf invoked > "+reaperTestShellQuote(markerPath)+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "container")
	backend := "#!/bin/sh\nprintf '%s\\n' \"$$\" > " + reaperTestShellQuote(pidPath) + "\n/bin/sleep 30\n"
	if err := os.WriteFile(binPath, []byte(backend), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.timeoutSeconds = 1
	r.command = func() *exec.Cmd {
		cmd := exec.Command("/bin/dash", "-c", reaperScript, "containergo-reaper", binPath, "delete", breQuote(creationLabel), "1")
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		return cmd
	}
	if err := r.register("shadowed-pgrep", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	pid := waitForReaperTestPIDFile(t, pidPath)
	waitForReaperTestProcessGone(t, pid)
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatal("timeout cleanup trusted a PATH-shadowed pgrep")
	}
	waitForReaperExit(t, r)
}

// startReaperTestTree starts root -> middle -> leaf, where leaf is a long
// running sleep, and returns the three PIDs. Each script reports its child
// before waiting, so the whole branch is live when the walk starts.
func startReaperTestTree(t *testing.T) (root, middle, leaf int) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	dir := t.TempDir()
	leafPath := filepath.Join(dir, "leaf")
	middlePath := filepath.Join(dir, "middle")
	rootPath := filepath.Join(dir, "root")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(leafPath, "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s\\n' \"$!\"\nwait\n")
	write(middlePath, "#!/bin/sh\n"+reaperTestShellQuote(leafPath)+" &\nprintf '%s\\n' \"$!\"\nwait\n")
	write(rootPath, "#!/bin/sh\n"+reaperTestShellQuote(middlePath)+" &\nprintf '%s\\n' \"$!\"\nwait\n")

	cmd := exec.Command("/bin/sh", rootPath)
	prepareReaperCommand(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Unconditional: a failed assertion must not leave the tree running.
		for _, pid := range []int{leaf, middle, cmd.Process.Pid} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = cmd.Wait()
	})
	root = cmd.Process.Pid
	middle = readReaperTestPID(t, stdout)
	leaf = readReaperTestPID(t, stdout)
	return root, middle, leaf
}

func readReaperTestPID(t *testing.T, stdout io.Reader) int {
	t.Helper()
	type result struct {
		pid int
		err error
	}
	parsed := make(chan result, 1)
	go func() {
		var buf [64]byte
		n, err := stdout.Read(buf[:])
		if err != nil {
			parsed <- result{err: err}
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
		parsed <- result{pid: pid, err: err}
	}()
	select {
	case got := <-parsed:
		if got.err != nil {
			t.Fatalf("read child pid: %v", got.err)
		}
		if got.pid <= 0 {
			t.Fatalf("child pid = %d, want positive", got.pid)
		}
		return got.pid
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading child pid")
		return 0
	}
}

// killReaperTree treats an unavailable child listing as unknown rather than
// empty: reading a slow lookup as "no children" used to abandon the whole
// branch and let the timed command's nested group survive.
func TestKillReaperTreeRetriesUnlistedBranch(t *testing.T) {
	root, middle, leaf := startReaperTestTree(t)

	original := reaperTreeLookup
	t.Cleanup(func() { reaperTreeLookup = original })
	var failed atomic.Bool
	reaperTreeLookup = func(ctx context.Context, pgrep string, parent int) ([]int, bool) {
		if parent == root && failed.CompareAndSwap(false, true) {
			return nil, false
		}
		return original(ctx, pgrep, parent)
	}

	killReaperTree(root)
	waitForReaperTestProcessGone(t, leaf)
	waitForReaperTestProcessGone(t, middle)
}

// A branch that cannot be enumerated must be left running: killing its
// leader reparents the unvisited processes and removes the only path a
// later pass could use.
func TestKillReaperTreeKeepsUnenumeratedBranchReachable(t *testing.T) {
	root, middle, leaf := startReaperTestTree(t)

	original := reaperTreeLookup
	t.Cleanup(func() { reaperTreeLookup = original })
	var fail atomic.Bool
	fail.Store(true)
	reaperTreeLookup = func(ctx context.Context, pgrep string, parent int) ([]int, bool) {
		if parent == middle && fail.Load() {
			return nil, false
		}
		return original(ctx, pgrep, parent)
	}

	killReaperTree(root)
	if state, _ := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(leaf)).Output(); strings.TrimSpace(string(state)) == "" {
		t.Fatal("leaf was reparented even though its branch was not enumerated")
	}

	fail.Store(false)
	killReaperTree(root)
	waitForReaperTestProcessGone(t, leaf)
	waitForReaperTestProcessGone(t, middle)
}

// Repeatedly unavailable listings must not turn recovery into a stall: the
// walk stays inside its own budget and leaves the tree to the group signal.
func TestKillReaperTreeStaysBoundedWhenLookupsFail(t *testing.T) {
	original := reaperTreeLookup
	t.Cleanup(func() { reaperTreeLookup = original })
	reaperTreeLookup = func(context.Context, string, int) ([]int, bool) {
		return nil, false
	}

	start := time.Now()
	killReaperTree(os.Getpid())
	if elapsed := time.Since(start); elapsed > 4*reaperTreeCleanupTimeout {
		t.Fatalf("killReaperTree with failing lookups took %s, want <= %s", elapsed, 4*reaperTreeCleanupTimeout)
	}
}

func TestReaperProcessGroupIsStoppedBeforeWait(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & echo $! > "+reaperTestShellQuote(pidPath)+"; wait")
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	process := &reaperProcess{
		cmd:        cmd,
		stdin:      stdin,
		exited:     make(chan struct{}),
		pid:        cmd.Process.Pid,
		pgid:       reaperProcessGroupID(cmd),
		groupOwned: true,
	}
	r := newReaper("unused", "delete")
	go r.waitProcess(process)
	pid := waitForReaperTestPIDFile(t, pidPath)
	process.terminate()
	select {
	case <-process.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper process group did not finish before wait")
	}
	waitForReaperTestProcessGone(t, pid)
	_ = stdin.Close()
	if process.pid != 0 || process.pgid != 0 {
		t.Fatalf("reaped process identity = pid:%d pgid:%d, want 0/0", process.pid, process.pgid)
	}
	// A late caller must not turn the old numeric identity into a signal.
	process.terminate()
}

func TestReaperRecoveryKillsNestedCommandGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "nested.pid")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = delete ]; then\n" +
		"  /bin/sleep 30 &\n" +
		"  child=$!\n" +
		"  printf '%s\\n' \"$child\" > " + reaperTestShellQuote(pidPath) + "\n" +
		"  wait \"$child\"\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newReaper(binPath, "delete")
	r.timeoutSeconds = 30
	if err := r.register("nested", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	pid := waitForReaperTestPIDFile(t, pidPath)
	r.killForTest()
	waitForReaperTestProcessGone(t, pid)
}

func TestReaperTimeoutKillsBackendDescendant(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "descendant.pid")
	binPath := filepath.Join(dir, "container")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = delete ]; then\n" +
		"  /bin/sleep 30 &\n" +
		"  child=$!\n" +
		"  printf '%s\\n' \"$child\" > " + reaperTestShellQuote(pidPath) + "\n" +
		"  wait \"$child\"\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.timeoutSeconds = 1
	if err := r.register("tree", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()

	pid := waitForReaperTestPIDFile(t, pidPath)
	waitForReaperTestProcessGone(t, pid)
	waitForReaperExit(t, r)
}

func waitForReaperTestPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for PID file %s", path)
	return 0
}

func waitForReaperTestProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(out))
		if err != nil || state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived reaper timeout cleanup", pid)
}

func TestReaperFastCallsReapTimeoutHelpers(t *testing.T) {
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls.log")
	readyDir := filepath.Join(dir, "sleep-ready")
	currentTokenPath := filepath.Join(dir, "sleep-token")
	holdPath := filepath.Join(dir, "sleep-hold")
	exitPath := filepath.Join(dir, "sleep-exit")
	binPath := filepath.Join(dir, "container")
	sleepPath := filepath.Join(dir, "sleep")
	creation := "0123456789abcdef"
	if err := os.Mkdir(readyDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Each fake sleeper owns a writer on exitPath and blocks on holdPath.
	// The test owns both descriptors, so EOF on exitPath proves that every
	// sleeper process closed its owned handle. Cleanup closes the handles
	// directly instead of signaling a possibly recycled bare PID.
	reaperTestCreateFIFO(t, holdPath)
	holdReader := reaperTestOpenFIFO(t, holdPath, os.O_RDONLY)
	holdWriter := reaperTestOpenFIFO(t, holdPath, os.O_WRONLY)
	reaperTestCreateFIFO(t, exitPath)
	exitReader := reaperTestOpenFIFO(t, exitPath, os.O_RDONLY)

	backendScript := "#!/bin/sh\n" +
		"token=\"backend-$$\"\n" +
		"printf '%s\\n' \"$token\" > \"$REAPER_TEST_CURRENT_TOKEN.tmp\"\n" +
		"mv \"$REAPER_TEST_CURRENT_TOKEN.tmp\" \"$REAPER_TEST_CURRENT_TOKEN\"\n" +
		"i=0\n" +
		"while [ ! -e \"$REAPER_TEST_READY_DIR/ready-$token\" ]; do\n" +
		"  /bin/sleep 0.001\n" +
		"  i=$((i + 1))\n" +
		"  [ \"$i\" -lt 5000 ] || exit 1\n" +
		"done\n" +
		"/bin/sleep 0.05\n" +
		"printf '%s\\n' \"$*\" >> " + reaperTestShellQuote(callsPath) + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  printf '  \"" + creationLabel + "\": \"" + creation + "\"\\n'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(backendScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// The backend waits for its own sleeper's readiness file before
	// returning. The token handoff also handles the scheduler case where
	// the timeout helper starts before the backend gets CPU.
	sleepScript := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ ! -s \"$REAPER_TEST_CURRENT_TOKEN\" ]; do\n" +
		"  /bin/sleep 0.001\n" +
		"  i=$((i + 1))\n" +
		"  [ \"$i\" -lt 5000 ] || exit 1\n" +
		"done\n" +
		"token=$(sed -n '1p' \"$REAPER_TEST_CURRENT_TOKEN\")\n" +
		"exec 3>\"$REAPER_TEST_EXIT_FIFO\"\n" +
		"printf '%s\\n' \"$$\" > \"$REAPER_TEST_READY_DIR/ready-$token\"\n" +
		"IFS= read -r _ < \"$REAPER_TEST_HOLD_FIFO\"\n"
	if err := os.WriteFile(sleepPath, []byte(sleepScript), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.command = func() *exec.Cmd {
		cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", binPath, "delete", breQuote(creationLabel))
		cmd.Env = append(os.Environ(),
			"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"REAPER_TEST_CURRENT_TOKEN="+currentTokenPath,
			"REAPER_TEST_READY_DIR="+readyDir,
			"REAPER_TEST_HOLD_FIFO="+holdPath,
			"REAPER_TEST_EXIT_FIFO="+exitPath,
		)
		return cmd
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})
	t.Cleanup(func() {
		_ = holdWriter.Close()
		_ = holdReader.Close()
		_ = exitReader.Close()
	})

	if err := r.register("guarded", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, callsPath, "inspect guarded", "delete --force guarded")
	waitForReaperExit(t, r)
	waitForReaperTestReadyFiles(t, readyDir, 2)
	waitForReaperTestFIFOEOF(t, exitReader)
}
