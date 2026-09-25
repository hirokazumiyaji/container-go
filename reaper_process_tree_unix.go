//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
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
	reaperTreeCleanupTimeout = time.Second
	reaperTreeLookupTTL      = 100 * time.Millisecond
)

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
		var walk func(int, int)
		walk = func(parent, depth int) {
			if ctx.Err() != nil || depth > maxReaperTreeDepth || len(signaled) >= maxReaperTreePIDs {
				return
			}
			for _, child := range reaperTreeChildren(ctx, pgrep, parent) {
				if child <= 0 {
					continue
				}
				walk(child, depth+1)
				if _, already := signaled[child]; already {
					continue
				}
				// Capture the branch before stopping its leader. Do not use
				// a liveness probe followed by a signal: the traversal is
				// intentionally one-shot for each numeric PID.
				_ = syscall.Kill(child, syscall.SIGKILL)
				signaled[child] = struct{}{}
			}
		}
		walk(root, 0)
		if len(signaled) == before {
			return
		}
	}
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

func reaperTreeChildren(parentCtx context.Context, pgrep string, parent int) []int {
	ctx, cancel := context.WithTimeout(parentCtx, reaperTreeLookupTTL)
	defer cancel()
	cmd := exec.CommandContext(ctx, pgrep, "-P", strconv.Itoa(parent))
	cmd.WaitDelay = 50 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return nil
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
	return children
}
