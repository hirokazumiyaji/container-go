//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxReaperDescendantPasses = 8
	maxReaperDescendantPIDs   = 1024
	maxReaperDescendantDepth  = 64
	reaperPgrepWaitDelay      = 100 * time.Millisecond
)

var errReaperDescendantLimit = errors.New("reaper descendant traversal limit exceeded")

type reaperDescendantLookup func(context.Context, int) ([]int, error)

func prepareReaperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killReaperCommand(ctx context.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, reaperCleanupTimeout)
	defer cancel()

	// A timed entry can have its own process group. Kill those groups
	// before the reaper's group so a test or an operator terminating the
	// reaper cannot strand a descendant in a nested group. The traversal
	// signals each branch as it is found, rather than building one stale
	// snapshot before killing anything.
	traversalErr := reaperDescendants(cleanupCtx, cmd.Process.Pid, func(pid int) error {
		return killReaperProcess(pid)
	})

	// Always try the root group even when pgrep is unavailable or the
	// bounded traversal was interrupted. This is the best containment
	// available for descendants that stayed in the root group.
	rootErr := killReaperRoot(cmd)
	return errors.Join(traversalErr, rootErr)
}

func killReaperRoot(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// The process may have exited between the group signal and this
	// fallback. Let the normal Wait path reap it when possible.
	return killReaperProcess(cmd.Process.Pid)
}

func killReaperProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("reaper: invalid descendant pid %d", pid)
	}

	// A negative kill is only safe when this PID is the leader of its
	// process group. Otherwise the numeric PID may name an unrelated
	// process group after it exits and is reused.
	pgid, err := reaperProcessGroupID(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("reaper: getpgid %d: %w", pid, err)
	}
	if pgid == pid {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("reaper: kill process group %d: %w", pid, err)
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("reaper: kill process %d: %w", pid, err)
	}
	return nil
}

func reaperDescendants(ctx context.Context, root int, visit func(int) error) error {
	return reaperDescendantsWithLookup(ctx, root, visit, reaperPgrepChildren)
}

func reaperDescendantsWithLookup(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup) error {
	boundedCtx, cancel := context.WithTimeout(ctx, reaperCleanupTimeout)
	defer cancel()
	ctx = boundedCtx

	if root <= 0 {
		return fmt.Errorf("reaper: invalid root pid %d", root)
	}

	// Keep the PID for the whole bounded cleanup. A PID is signaled at
	// most once: retaining this set prevents a later traversal from
	// signaling a recycled PID after the original process has exited.
	seen := map[int]struct{}{root: {}}
	visited := 0
	var visitErrors []error
	var walk func(int, int, []int) error
	walk = func(parent, depth int, children []int) error {
		if depth > maxReaperDescendantDepth {
			return fmt.Errorf("%w: depth exceeds %d", errReaperDescendantLimit, maxReaperDescendantDepth)
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if child <= 0 {
				return fmt.Errorf("reaper: pgrep returned invalid descendant pid %d for %d", child, parent)
			}
			if _, ok := seen[child]; ok {
				continue
			}
			if visited >= maxReaperDescendantPIDs {
				return fmt.Errorf("%w: more than %d descendants", errReaperDescendantLimit, maxReaperDescendantPIDs)
			}

			// Capture one branch before stopping its leader. Without this
			// small hand-off window, a child that is reparented while the
			// leader is being killed can escape the next lookup. We never
			// look up this PID again after signaling it, so a recycled
			// numeric PID cannot be mistaken for the original process.
			childChildren, childErr := lookup(ctx, child)
			seen[child] = struct{}{}
			visited++
			// Signal before descending so a running branch cannot create
			// another child while an earlier subtree is being enumerated.
			if err := visit(child); err != nil {
				visitErrors = append(visitErrors, fmt.Errorf("reaper: signal descendant %d: %w", child, err))
			}
			if childErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				visitErrors = append(visitErrors, childErr)
				continue
			}
			if err := walk(child, depth+1, childChildren); err != nil {
				return err
			}
		}
		return nil
	}

	for pass := 0; pass < maxReaperDescendantPasses; pass++ {
		before := visited
		if err := ctx.Err(); err != nil {
			visitErrors = append(visitErrors, err)
			return errors.Join(visitErrors...)
		}
		children, err := lookup(ctx, root)
		if err != nil {
			visitErrors = append(visitErrors, err)
			return errors.Join(visitErrors...)
		}
		if err := walk(root, 0, children); err != nil {
			visitErrors = append(visitErrors, err)
			return errors.Join(visitErrors...)
		}
		if visited == before {
			return errors.Join(visitErrors...)
		}
	}
	return errors.Join(append(visitErrors, errReaperDescendantLimit)...)
}

func reaperPgrepChildren(ctx context.Context, parent int) ([]int, error) {
	cmd := exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(parent))
	// A pgrep replacement should not be able to keep the cleanup stuck
	// by retaining stdout after its context is canceled.
	cmd.WaitDelay = reaperPgrepWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if exitErr.ExitCode() == 1 && stderr == "" {
				return nil, nil
			}
			if stderr != "" {
				return nil, fmt.Errorf("reaper: pgrep -P %d failed: %w: %s", parent, err, stderr)
			}
			return nil, fmt.Errorf("reaper: pgrep -P %d failed: %w", parent, err)
		}
		return nil, fmt.Errorf("reaper: pgrep -P %d failed: %w", parent, err)
	}
	return parseReaperPIDs(parent, out)
}

func parseReaperPIDs(parent int, out []byte) ([]int, error) {
	fields := strings.Fields(string(out))
	if len(fields) > maxReaperDescendantPIDs {
		return nil, fmt.Errorf("%w: pgrep returned more than %d descendants for %d", errReaperDescendantLimit, maxReaperDescendantPIDs, parent)
	}
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("reaper: pgrep returned invalid descendant pid %q for %d", field, parent)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}
