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

	// Capture and recheck the root's process start identity around the
	// traversal and kill. A numeric PID can be recycled after Wait; an
	// unverifiable or changed identity is left alone rather than signaled.
	rootPID := cmd.Process.Pid
	rootOwner, owned := reaperProcessStartIdentity(cleanupCtx, rootPID)
	if !owned {
		return fmt.Errorf("reaper: root process %d ownership could not be verified", rootPID)
	}
	if current, ok := reaperProcessStartIdentity(cleanupCtx, rootPID); !ok || current != rootOwner {
		return fmt.Errorf("reaper: root process %d changed before cleanup", rootPID)
	}

	// Enumerate and stop descendants while the owned root is still alive,
	// but signal only positive PIDs whose start identity is verified twice.
	// The traversal records each PID before signaling it and never
	// re-expands a successfully signaled branch.
	traversalErr := reaperDescendants(cleanupCtx, rootPID, func(pid int) error {
		return killReaperProcessVerified(cleanupCtx, pid)
	})
	if current, ok := reaperProcessStartIdentity(cleanupCtx, rootPID); !ok || current != rootOwner {
		return errors.Join(traversalErr, fmt.Errorf("reaper: root process %d changed before root kill", rootPID))
	}
	rootErr := killReaperRootOwned(cleanupCtx, cmd, rootOwner)
	return errors.Join(traversalErr, rootErr)
}

func killReaperRootOwned(ctx context.Context, cmd *exec.Cmd, owner string) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if owner == "" {
		var ok bool
		owner, ok = reaperProcessStartIdentity(ctx, cmd.Process.Pid)
		if !ok {
			return fmt.Errorf("reaper: root process %d ownership could not be verified", cmd.Process.Pid)
		}
	}
	current, ok := reaperProcessStartIdentity(ctx, cmd.Process.Pid)
	if !ok || current != owner {
		return fmt.Errorf("reaper: root process %d ownership changed", cmd.Process.Pid)
	}
	return signalReaperRoot(cmd)
}

//nolint:unused // retained as a safe compatibility entry point for diagnostics
func killReaperRoot(cmd *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cancel()
	return killReaperRootOwned(ctx, cmd, "")
}

func signalReaperRoot(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func reaperProcessStartIdentity(ctx context.Context, pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	psPath, err := exec.LookPath("ps")
	if err != nil || !filepath.IsAbs(psPath) {
		return "", false
	}
	cmd := exec.CommandContext(ctx, psPath, "-o", "lstart=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	identity := strings.TrimSpace(string(out))
	return identity, identity != ""
}

func killReaperProcessVerified(ctx context.Context, pid int) error {
	expected, ok := reaperProcessStartIdentity(ctx, pid)
	if !ok {
		// An unverifiable process is left alone; cleanup must fail closed
		// rather than risk signaling a recycled PID.
		return nil
	}
	current, ok := reaperProcessStartIdentity(ctx, pid)
	if !ok || current != expected {
		return nil
	}
	return killReaperProcess(pid)
}

func killReaperProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("reaper: invalid descendant pid %d", pid)
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
	pgrepPath, err := exec.LookPath("pgrep")
	if err != nil || !filepath.IsAbs(pgrepPath) {
		return nil, errors.New("reaper: pgrep is unavailable or not an absolute executable")
	}
	info, err := os.Stat(pgrepPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("reaper: pgrep is not a trusted executable")
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
