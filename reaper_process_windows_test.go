//go:build windows

package container

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The reaper must not inherit the parent's console process group: Ctrl+C or
// closing that console would kill the watchdog together with the parent. The
// flags are observable without starting a process.
func TestConfigureReaperProcessUsesHiddenProcessGroup(t *testing.T) {
	cmd := &exec.Cmd{}
	configureReaperProcess(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("configureReaperProcess left SysProcAttr nil")
	}
	want := uint32(syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	if cmd.SysProcAttr.CreationFlags&want != want {
		t.Fatalf("CreationFlags = %#x, want %#x", cmd.SysProcAttr.CreationFlags, want)
	}
}

// The job tests use this test binary as a small process tree: a parent helper
// waits for a release file, starts a child that would outlive a direct-child
// kill, and reports its PID. Both helpers stay alive, so the test can observe
// whether the reaper's stop path reached the descendant.
const (
	reaperJobParentHelperEnv = "CONTAINERGO_TEST_REAPER_JOB_PARENT"
	reaperJobChildHelperEnv  = "CONTAINERGO_TEST_REAPER_JOB_CHILD"
	reaperJobReleaseEnv      = "CONTAINERGO_TEST_REAPER_JOB_RELEASE"
	reaperJobChildPIDEnv     = "CONTAINERGO_TEST_REAPER_JOB_CHILD_PID"

	// reaperJobStartTimeout is generous because a loaded Windows CI runner can
	// be slow to start the test binary twice.
	reaperJobStartTimeout = 20 * time.Second
	// reaperJobExitTimeout bounds the observation after a teardown call.
	reaperJobExitTimeout = 5 * time.Second
)

// TestReaperStopTerminatesJobDescendants is the Windows counterpart of the
// Unix process-group teardown test: a process the reaper started must die with
// it instead of lingering as an orphan. The helper starts its child only after
// the job was attached, matching the native reaper, which starts no backend
// command before EOF.
func TestReaperStopTerminatesJobDescendants(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	pidPath := filepath.Join(dir, "child.pid")

	r := newNativeReaper(os.Args[0], "rm")
	r.spawnCommand = func() (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReaperJobParentHelper$")
		cmd.Env = append(os.Environ(),
			reaperJobParentHelperEnv+"=1",
			reaperJobReleaseEnv+"="+release,
			reaperJobChildPIDEnv+"="+pidPath,
		)
		return cmd, nil
	}
	// A retained handle would keep this exact process object alive and
	// observable, so liveness checks and cleanup never resolve the PID again.
	// The handle is opened after the helper reports its child, which is only
	// done once the job was attached and the child is running.
	var (
		child    windows.Handle
		childPID int
	)
	// Stop whatever generation is current even when an assertion below fails
	// early: the job teardown is what keeps the helper tree from outliving the
	// test binary and holding the executable image on Windows.
	t.Cleanup(func() {
		r.killForTest()
		r.closeStdin()
		if child != 0 {
			_ = windows.TerminateProcess(child, 1)
			_ = windows.CloseHandle(child)
		}
	})
	if err := r.register(nativeReaperID('a'), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.mu.Lock()
	attached := r.job.handle != 0
	r.mu.Unlock()
	if !attached {
		t.Fatal("reaper did not attach a job object; the teardown cannot reach descendants")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	child, childPID = openReaperJobChild(t, pidPath)
	if exited, err := windowsProcessExited(child); err != nil {
		t.Fatalf("query descendant %d: %v", childPID, err)
	} else if exited {
		t.Fatalf("descendant %d exited before the reaper was stopped", childPID)
	}

	r.killForTest()
	waitForWindowsProcessExit(t, child, childPID)
}

// TestReaperJobHandleCloseLeavesChildRunning pins the
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE decision. The only job handle lives in
// the parent, and the kernel closes it when the parent dies; with that limit
// set, the close would kill the watchdog exactly when it has to survive to
// reap. Without it, closing the handle leaves the associated processes
// running and the job is destroyed once they exit. The direct kill must still
// work afterwards, which is the fallback used when a job could not be attached
// at all.
func TestReaperJobHandleCloseLeavesChildRunning(t *testing.T) {
	cmd, exited := startReaperJobChild(t)

	job := attachReaperJob(cmd)
	if job.handle == 0 {
		t.Fatal("attachReaperJob did not create a job object")
	}
	job.close() // what the kernel does to the parent's handles at parent exit
	select {
	case <-exited:
		t.Fatal("closing the last job handle terminated the reaper child")
	case <-time.After(300 * time.Millisecond):
	}

	killReaperProcess(cmd)
	select {
	case <-exited:
	case <-time.After(reaperJobExitTimeout):
		t.Fatal("direct kill did not terminate the child without a job")
	}
}

// TestReaperJobParentHelper is the parent of the helper tree. It acts only
// when the job test started it with the marker; an ordinary test run returns
// immediately so the suite is unaffected.
func TestReaperJobParentHelper(t *testing.T) {
	if os.Getenv(reaperJobParentHelperEnv) != "1" {
		return
	}
	waitForReaperJobPath(os.Getenv(reaperJobReleaseEnv))
	child := exec.Command(os.Args[0], "-test.run=^TestReaperJobChildHelper$")
	child.Env = append(os.Environ(), reaperJobChildHelperEnv+"=1")
	if err := child.Start(); err != nil {
		os.Exit(1)
	}
	// Publish the PID with a rename so the reader never sees a partial
	// number: the test opens a handle to that exact process, and a truncated
	// PID could name an unrelated one.
	pidPath := os.Getenv(reaperJobChildPIDEnv)
	partial := pidPath + ".partial"
	pid := []byte(strconv.Itoa(child.Process.Pid))
	if err := os.WriteFile(partial, pid, 0o600); err != nil {
		os.Exit(1)
	}
	if err := os.Rename(partial, pidPath); err != nil {
		os.Exit(1)
	}
	for {
		time.Sleep(time.Hour)
	}
}

// TestReaperJobChildHelper is the descendant that only the job can terminate.
func TestReaperJobChildHelper(t *testing.T) {
	if os.Getenv(reaperJobChildHelperEnv) != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func startReaperJobChild(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReaperJobChildHelper$")
	cmd.Env = append(os.Environ(), reaperJobChildHelperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child helper: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	return cmd, exited
}

// waitForReaperJobPath has no test handle to report through: it runs inside
// the helper process, whose output the test discards.
func waitForReaperJobPath(path string) {
	deadline := time.Now().Add(reaperJobStartTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(1)
}

// openReaperJobChild waits for the helper's published child PID and opens a
// handle to that process. The handle is retained for the rest of the test, so
// every later liveness check and the cleanup act on the process object itself
// instead of resolving a PID that the teardown may already have freed for
// reuse.
func openReaperJobChild(t *testing.T, path string) (windows.Handle, int) {
	t.Helper()
	deadline := time.Now().Add(reaperJobStartTimeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				process, openErr := windows.OpenProcess(
					windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
					false,
					uint32(pid),
				)
				if openErr != nil {
					t.Fatalf("open descendant %d: %v", pid, openErr)
				}
				return process, pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read helper child pid: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reaper helper tree did not start")
	return 0, 0
}

// windowsProcessExited reports whether the retained handle refers to an
// already exited process.
func windowsProcessExited(process windows.Handle) (bool, error) {
	event, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		return false, err
	}
	return event == uint32(windows.WAIT_OBJECT_0), nil
}

func waitForWindowsProcessExit(t *testing.T, process windows.Handle, pid int) {
	t.Helper()
	event, err := windows.WaitForSingleObject(process, uint32(reaperJobExitTimeout/time.Millisecond))
	if err != nil {
		t.Fatalf("wait for descendant %d: %v", pid, err)
	}
	if event != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("descendant %d survived the job teardown", pid)
	}
}
