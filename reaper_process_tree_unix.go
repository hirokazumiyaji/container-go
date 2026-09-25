//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxReaperTreeDepth       = 64
	maxReaperTreePIDs        = 2048
	maxReaperTreePasses      = 4
	reaperTreeCleanupTimeout = 750 * time.Millisecond
	reaperTreeLookupTTL      = 250 * time.Millisecond
)

// reaperTreeLookup is the child enumeration seam. The bool result reports
// whether the answer is authoritative: a failed or timed-out lookup says
// nothing about the processes below it and must never be read as "this
// process has no children".
var reaperTreeLookup = reaperTreeChildren

// killReaperTree walks descendants before the root group is signaled. A
// timed command owns a nested process group, so a root-group signal alone
// would not contain a supervisor that is still between backend calls.
func killReaperTree(root int) {
	if root <= 0 {
		return
	}
	pgrep := trustedReaperPgrepPath()
	if pgrep == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reaperTreeCleanupTimeout)
	defer cancel()
	signaled := make(map[int]struct{})
	for pass := 0; pass < maxReaperTreePasses; pass++ {
		before := len(signaled)
		complete := signalReaperDescendants(ctx, pgrep, root, signaled)
		if ctx.Err() != nil {
			return
		}
		// Only a fully authoritative pass that found nothing new proves the
		// subtree is contained. Every other outcome is retried, because an
		// unenumerated branch would otherwise survive the root-group signal.
		if complete && len(signaled) == before {
			return
		}
	}
}

// signalReaperDescendants records one kill per discovered PID and reports
// whether every visited node was enumerated authoritatively. A node whose
// branch could not be enumerated is deliberately left running: killing it
// would reparent the unvisited processes and remove the only path a later
// pass could use to reach them.
func signalReaperDescendants(ctx context.Context, pgrep string, root int, signaled map[int]struct{}) bool {
	complete := true
	var walk func(parent, depth int) bool
	walk = func(parent, depth int) bool {
		if ctx.Err() != nil {
			complete = false
			return false
		}
		if depth > maxReaperTreeDepth || len(signaled) >= maxReaperTreePIDs {
			// The limits below are not retryable within this walk; the
			// remaining budget is better spent on the branches already found.
			return true
		}
		children, ok := reaperTreeLookup(ctx, pgrep, parent)
		if !ok {
			complete = false
			return false
		}
		for _, child := range children {
			if child <= 0 {
				continue
			}
			if !walk(child, depth+1) {
				complete = false
				continue
			}
			if _, already := signaled[child]; already {
				continue
			}
			// Capture the branch before stopping its leader. Do not use
			// a liveness probe followed by a signal: the traversal is
			// intentionally one-shot for each numeric PID.
			_ = syscall.Kill(child, syscall.SIGKILL)
			signaled[child] = struct{}{}
		}
		return true
	}
	walk(root, 0)
	return complete
}

func trustedReaperPgrepPath() string {
	for _, path := range []string{"/usr/bin/pgrep", "/bin/pgrep"} {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func reaperTreeChildren(parentCtx context.Context, pgrep string, parent int) ([]int, bool) {
	ctx, cancel := context.WithTimeout(parentCtx, reaperTreeLookupTTL)
	defer cancel()
	cmd := exec.CommandContext(ctx, pgrep, "-P", strconv.Itoa(parent))
	cmd.WaitDelay = 50 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		// pgrep exits 1 when nothing matched, which is an authoritative
		// empty answer. A canceled lookup or any other failure is not.
		var exitErr *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, true
		}
		return nil, false
	}
	fields := strings.Fields(string(out))
	if len(fields) > maxReaperTreePIDs {
		fields = fields[:maxReaperTreePIDs]
	}
	children := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err == nil && pid > 0 {
			children = append(children, pid)
		}
	}
	return children, true
}
