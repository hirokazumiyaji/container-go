//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"bufio"
	"context"
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

const (
	maxReaperDescendantPasses  = 8
	maxReaperDescendantPIDs    = 1024
	maxReaperDescendantDepth   = 64
	maxReaperDescendantLookups = 4096
	reaperPgrepWaitDelay       = 100 * time.Millisecond
	reaperDescendantTimeout    = 2 * time.Second
)

var errReaperDescendantLimit = errors.New("reaper descendant traversal limit exceeded")

// reaperGroupOwner is the parent-owned end of the reaper's supervision
// pipes. The reaper itself is a session/group leader; a shell sentinel holds
// that group after the reaper shell exits until the Go parent closes the
// sentinel pipe. Consequently a post-Wait group signal is still tied to an
// unreaped group member and cannot target a recycled numeric PGID.
type reaperGroupOwner struct {
	pid           int
	sentinelPID   int
	sentinelWrite *os.File
	readyRead     *os.File
}

func newReaperGroupOwner() (*reaperGroupOwner, error) {
	return &reaperGroupOwner{}, nil
}

func (g *reaperGroupOwner) prepare(cmd *exec.Cmd) {
	if g == nil || cmd == nil {
		return
	}
	// A fresh session makes the reaper a session and process-group leader;
	// this also lets the shell's bounded job-control groups behave
	// consistently on non-interactive Darwin shells.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func (g *reaperGroupOwner) attach(pid int, sentinelWrite, readyRead *os.File) {
	if g == nil {
		return
	}
	g.pid = pid
	g.sentinelWrite = sentinelWrite
	g.readyRead = readyRead
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
	if g.sentinelPID > 0 {
		pgid, err := syscall.Getpgid(g.sentinelPID)
		if err != nil || pgid != g.pid {
			return false
		}
	}
	// The shell sentinel remains waitable in the group until finish, so this
	// PGID cannot have been recycled. This is deliberately the only group
	// signal in the lifecycle.
	return syscall.Kill(-g.pid, syscall.SIGKILL) == nil
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
	g.sentinelWrite = nil
	g.readyRead = nil
}

func prepareReaperCommand(cmd *exec.Cmd, group *reaperGroupOwner) {
	group.prepare(cmd)
}

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
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func cleanupReaperDescendants(process *reaperProcess) {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reaperDescendantTimeout)
	defer cancel()
	if err := reaperDescendants(ctx, process.cmd.Process.Pid, killReaperDescendant); err != nil {
		// The root group signal remains the containment boundary. Traversal
		// errors are intentionally non-fatal so a broken pgrep cannot keep
		// the reaper from being reaped and replaced.
		return
	}
}

func killReaperDescendant(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("reaper: invalid descendant pid %d", pid)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if pgid == pid {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

type reaperDescendantLookup func(context.Context, int) ([]int, error)
type reaperDescendantIdentity func(int) (int, bool)

type reaperDescendantState struct {
	pgid       int
	identified bool
	signalErr  error
}

func reaperDescendants(ctx context.Context, root int, visit func(int) error) error {
	return reaperDescendantsWithLookupAndIdentity(ctx, root, visit, reaperPgrepChildren, func(pid int) (int, bool) {
		pgid, err := syscall.Getpgid(pid)
		return pgid, err == nil
	})
}

func reaperDescendantsWithLookup(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup) error {
	return reaperDescendantsWithLookupAndIdentity(ctx, root, visit, lookup, func(pid int) (int, bool) { return pid, true })
}

func reaperDescendantsWithLookupAndIdentity(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup, identify reaperDescendantIdentity) error {
	if root <= 0 {
		return fmt.Errorf("reaper: invalid root pid %d", root)
	}
	if visit == nil {
		visit = func(int) error { return nil }
	}
	if identify == nil {
		identify = func(pid int) (int, bool) { return pid, true }
	}
	states := map[int]*reaperDescendantState{}
	depths := map[int]int{root: 0}
	parents := []int{root}
	visited, lookups := 0, 0
	var errs []error
	retry := false

	lookupChildren := func(parent int) ([]int, error) {
		if lookups >= maxReaperDescendantLookups {
			return nil, fmt.Errorf("%w: more than %d process-tree lookups", errReaperDescendantLimit, maxReaperDescendantLookups)
		}
		lookups++
		children, err := lookup(ctx, parent)
		if err != nil {
			retry = true
			errs = append(errs, fmt.Errorf("reaper: lookup pid %d: %w", parent, err))
		}
		return children, err
	}

	var walk func(int, int) error //nolint:staticcheck // recursive traversal needs a named function value
	walk = func(parent, depth int) error {
		if depth > maxReaperDescendantDepth {
			return fmt.Errorf("%w: depth exceeds %d", errReaperDescendantLimit, maxReaperDescendantDepth)
		}
		if parent != root {
			state := states[parent]
			if state != nil && state.identified {
				pgid, ok := identify(parent)
				if !ok || pgid != state.pgid {
					return nil
				}
			}
		}
		children, err := lookupChildren(parent)
		if err != nil {
			return err
		}
		for _, child := range children {
			if child <= 0 {
				errs = append(errs, fmt.Errorf("reaper: lookup returned invalid pid %d for %d", child, parent))
				retry = true
				continue
			}
			state, exists := states[child]
			if !exists {
				if visited >= maxReaperDescendantPIDs {
					return fmt.Errorf("%w: more than %d descendants", errReaperDescendantLimit, maxReaperDescendantPIDs)
				}
				pgid, identified := identify(child)
				if !identified || pgid <= 0 {
					errs = append(errs, fmt.Errorf("reaper: descendant pid %d disappeared before signaling", child))
					retry = true
					continue
				}
				state = &reaperDescendantState{pgid: pgid, identified: identified}
				states[child] = state
				depths[child] = depth + 1
				parents = append(parents, child)
				visited++
				if err := visit(child); err != nil {
					state.signalErr = err
					retry = true
					errs = append(errs, fmt.Errorf("reaper: signal pid %d: %w", child, err))
				}
			} else if state.signalErr != nil {
				if err := visit(child); err != nil {
					state.signalErr = err
					retry = true
					errs = append(errs, fmt.Errorf("reaper: signal pid %d: %w", child, err))
				} else {
					state.signalErr = nil
				}
			}
		}
		return nil
	}

	for pass := 0; pass < maxReaperDescendantPasses; pass++ {
		before, hadRetry := visited, retry
		retry = false
		for _, parent := range parents {
			if err := walk(parent, depths[parent]); err != nil {
				retry = true
				errs = append(errs, err)
			}
		}
		if visited == before && !retry && !hadRetry {
			return errors.Join(errs...)
		}
	}
	return errors.Join(append(errs, errReaperDescendantLimit)...)
}

func trustedReaperTool(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("reaper: trusted %s not found", name)
}

func reaperPgrepChildren(ctx context.Context, parent int) ([]int, error) {
	path, err := trustedReaperTool("pgrep")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, "-P", strconv.Itoa(parent))
	cmd.WaitDelay = reaperPgrepWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if exitErr.ExitCode() == 1 && stderr == "" {
				return nil, nil
			}
			if stderr != "" {
				return nil, fmt.Errorf("pgrep -P %d: %w: %s", parent, err, stderr)
			}
		}
		return nil, fmt.Errorf("pgrep -P %d: %w", parent, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) > maxReaperDescendantPIDs {
		return nil, fmt.Errorf("%w: pgrep returned too many children for %d", errReaperDescendantLimit, parent)
	}
	children := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("reaper: pgrep returned invalid pid %q", field)
		}
		children = append(children, pid)
	}
	return children, nil
}
