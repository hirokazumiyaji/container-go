//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
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
)

var errReaperDescendantLimit = errors.New("reaper descendant traversal limit exceeded")

type reaperDescendantLookup func(context.Context, int) ([]int, error)
type reaperDescendantIdentity func(int) (int, bool)

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
	return reaperDescendantsWithLookupAndIdentity(ctx, root, visit, reaperPgrepChildren, reaperProcessIdentity)
}

func reaperDescendantsWithLookup(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup) error {
	return reaperDescendantsWithLookupAndIdentity(ctx, root, visit, lookup, func(int) (int, bool) { return 0, false })
}

func reaperProcessIdentity(pid int) (int, bool) {
	pgid, err := reaperProcessGroupID(pid)
	return pgid, err == nil
}

func reaperDescendantsWithLookupAndIdentity(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup, identify reaperDescendantIdentity) error {
	boundedCtx, cancel := context.WithTimeout(ctx, reaperCleanupTimeout)
	defer cancel()
	ctx = boundedCtx

	if root <= 0 {
		return fmt.Errorf("reaper: invalid root pid %d", root)
	}

	type descendantState struct {
		signalErr  error
		pgid       int
		identified bool
	}
	states := map[int]*descendantState{root: {}}
	parentOrder := []int{root}
	visited := 0
	lookups := 0
	lookupErrors := make(map[int]error)
	signalErrors := make(map[int]error)
	retryNeeded := false
	var errorOrder []int
	rememberError := func(kind string, pid int, err error) {
		if err == nil {
			return
		}
		if kind == "lookup" {
			if _, ok := lookupErrors[pid]; !ok {
				errorOrder = append(errorOrder, pid)
			}
			lookupErrors[pid] = err
			return
		}
		if _, ok := signalErrors[pid]; !ok {
			errorOrder = append(errorOrder, pid)
		}
		signalErrors[pid] = err
	}
	clearError := func(kind string, pid int) {
		if kind == "lookup" {
			delete(lookupErrors, pid)
			return
		}
		delete(signalErrors, pid)
	}
	remainingErrors := func() error {
		var errs []error
		for _, pid := range errorOrder {
			if err := lookupErrors[pid]; err != nil {
				errs = append(errs, fmt.Errorf("reaper: lookup pid %d: %w", pid, err))
			}
			if err := signalErrors[pid]; err != nil {
				errs = append(errs, fmt.Errorf("reaper: signal descendant %d: %w", pid, err))
			}
		}
		return errors.Join(errs...)
	}

	lookupBounded := func(parent int) ([]int, error) {
		if lookups >= maxReaperDescendantLookups {
			return nil, fmt.Errorf("%w: more than %d process-tree lookups", errReaperDescendantLimit, maxReaperDescendantLookups)
		}
		lookups++
		children, err := lookup(ctx, parent)
		if err != nil {
			retryNeeded = true
			rememberError("lookup", parent, err)
		} else {
			clearError("lookup", parent)
		}
		return children, err
	}

	// A branch is re-expanded on every pass. This catches a child that is
	// created after its parent was first scanned and retries a branch whose
	// first pgrep failed. The state maps still ensure that a successfully
	// signaled numeric PID is not signaled again, while a failed signal can
	// be retried. When a process identity is available, an expanded branch
	// is queried only while its process group still has the same identity.
	active := make(map[int]struct{})
	var walk func(int, int) error
	var walkChildren func(int, int, []int) error
	walkChildren = func(parent, depth int, children []int) error {
		if depth > maxReaperDescendantDepth {
			return fmt.Errorf("%w: depth exceeds %d", errReaperDescendantLimit, maxReaperDescendantDepth)
		}
		if _, ok := active[parent]; ok {
			return nil
		}
		active[parent] = struct{}{}
		defer delete(active, parent)

		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if child <= 0 {
				return fmt.Errorf("reaper: pgrep returned invalid descendant pid %d for %d", child, parent)
			}
			state, exists := states[child]
			if !exists {
				if visited >= maxReaperDescendantPIDs {
					return fmt.Errorf("%w: more than %d descendants", errReaperDescendantLimit, maxReaperDescendantPIDs)
				}
				// Capture one branch before stopping its leader so a child
				// reparented during the hand-off is still available below it.
				childChildren, childErr := lookupBounded(child)
				pgid, identified := identify(child)
				state = &descendantState{pgid: pgid, identified: identified}
				states[child] = state
				parentOrder = append(parentOrder, child)
				visited++
				if err := visit(child); err != nil {
					state.signalErr = err
					retryNeeded = true
					rememberError("signal", child, err)
				}
				if childErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return ctxErr
					}
					continue
				}
				if err := walkChildren(child, depth+1, childChildren); err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return ctxErr
					}
					retryNeeded = true
					rememberError("lookup", child, err)
				}
				continue
			}

			// Retry a failed signal, but do not signal a successful PID a
			// second time after it could have been recycled.
			if state.signalErr != nil {
				if err := visit(child); err != nil {
					state.signalErr = err
					retryNeeded = true
					rememberError("signal", child, err)
				} else {
					state.signalErr = nil
					clearError("signal", child)
				}
			}
			// The parentOrder frontier re-expands this branch on the next
			// pass, even if the root no longer reports it as a child.
		}
		return nil
	}
	walk = func(parent, depth int) error {
		if parent != root {
			if state, ok := states[parent]; ok && state.identified {
				pgid, identified := identify(parent)
				if !identified || pgid != state.pgid {
					return nil
				}
			}
		}
		if depth > maxReaperDescendantDepth {
			return fmt.Errorf("%w: depth exceeds %d", errReaperDescendantLimit, maxReaperDescendantDepth)
		}
		if _, ok := active[parent]; ok {
			return nil
		}
		children, err := lookupBounded(parent)
		if err != nil {
			return err
		}
		return walkChildren(parent, depth, children)
	}

	for pass := 0; pass < maxReaperDescendantPasses; pass++ {
		before := visited
		retryNeeded = false
		if err := ctx.Err(); err != nil {
			return errors.Join(remainingErrors(), err)
		}
		for _, parent := range parentOrder {
			if err := ctx.Err(); err != nil {
				return errors.Join(remainingErrors(), err)
			}
			if err := walk(parent, 0); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return errors.Join(remainingErrors(), ctxErr)
				}
				retryNeeded = true
				rememberError("lookup", parent, err)
			}
		}
		if visited == before && !retryNeeded {
			return remainingErrors()
		}
	}
	return errors.Join(remainingErrors(), errReaperDescendantLimit)
}

func trustedReaperPgrepPath() (string, error) {
	path, err := exec.LookPath("pgrep")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("reaper: pgrep path %q is not absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("reaper: pgrep path %q is not executable", path)
	}
	return path, nil
}

func reaperPgrepChildren(ctx context.Context, parent int) ([]int, error) {
	pgrepPath, err := trustedReaperPgrepPath()
	if err != nil {
		return nil, fmt.Errorf("reaper: locate pgrep: %w", err)
	}
	cmd := exec.CommandContext(ctx, pgrepPath, "-P", strconv.Itoa(parent))
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
