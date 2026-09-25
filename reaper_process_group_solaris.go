//go:build solaris

package container

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type reaperGroupOwner struct {
	pid           int
	sentinelPID   int
	sentinelWrite *os.File
	readyRead     *os.File
}

func newReaperGroupOwner() (*reaperGroupOwner, error) { return &reaperGroupOwner{}, nil }

func (g *reaperGroupOwner) prepare(cmd *exec.Cmd) {
	if g != nil && cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}

func (g *reaperGroupOwner) attach(pid int, sentinelWrite, readyRead *os.File) {
	if g != nil {
		g.pid = pid
		g.sentinelWrite = sentinelWrite
		g.readyRead = readyRead
	}
}

func (g *reaperGroupOwner) setSentinelPID(pid int) {
	if g != nil && pid > 0 {
		g.sentinelPID = pid
	}
}

func (g *reaperGroupOwner) signal() bool {
	if g == nil || g.pid <= 0 {
		return false
	}
	return syscall.Kill(-g.pid, syscall.SIGKILL) == nil
}

func (g *reaperGroupOwner) finish() {
	if g == nil {
		return
	}
	if g.sentinelWrite != nil {
		_ = g.sentinelWrite.Close()
	}
	if g.readyRead != nil {
		_ = g.readyRead.Close()
	}
}

func waitForReaperSentinelReady(ready *os.File) (int, error) {
	if ready == nil {
		return 0, nil
	}
	type result struct {
		pid int
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(ready).ReadString('\n')
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 {
			resultCh <- result{err: fmt.Errorf("reaper: invalid sentinel pid %q", strings.TrimSpace(line))}
			return
		}
		resultCh <- result{pid: pid}
	}()
	select {
	case result := <-resultCh:
		if result.err != nil {
			return 0, fmt.Errorf("reaper: sentinel readiness: %w", result.err)
		}
		return result.pid, nil
	case <-time.After(2 * time.Second):
		return 0, errors.New("reaper: sentinel readiness timed out")
	}
}

func prepareReaperCommand(cmd *exec.Cmd, group *reaperGroupOwner) { group.prepare(cmd) }

func reaperProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func terminateReaperProcess(process *reaperProcess) {
	if process == nil {
		return
	}
	if process.group != nil && process.group.signal() {
		return
	}
	if process.cmd != nil && process.cmd.Process != nil {
		_ = process.cmd.Process.Kill()
	}
}

func finishReaperProcess(process *reaperProcess) {
	if process == nil {
		return
	}
	if process.group != nil {
		process.group.finish()
	}
	removeReaperStatusDir(process.statusDir)
}

func waitForReaperGroupExit(pgid int) bool {
	if pgid <= 0 {
		return true
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pgid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func cleanupReaperDescendants(*reaperProcess) {}

func trustedReaperTool(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("reaper: trusted %s not found", name)
}
