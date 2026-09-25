//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxReaperDescendantSnapshotPasses = 8
	maxReaperDescendantPIDs           = 1024
	maxReaperDescendantDepth          = 64
	maxReaperDescendantLookups        = 4096
	reaperPgrepWaitDelay              = 100 * time.Millisecond
)

var errReaperDescendantLimit = errors.New("reaper descendant traversal limit exceeded")

type reaperDescendantLookup func(context.Context, int) ([]int, error)
type reaperDescendantIdentity func(int) (int, bool)

func prepareReaperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func reaperCommandCancel(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Process.Kill()
}

func killReaperCommand(ctx context.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, reaperCleanupTimeout)
	defer cancel()

	// A timed entry can have its own process group. Quiesce the complete
	// process table before any KILL so a shell cannot fork a nested child
	// between one pgrep row and the next. The snapshot and every later
	// signal retain process identity (pidfd on Linux where available).
	table, quiesceErr := quiesceReaperTree(cleanupCtx, cmd.Process.Pid)
	var traversalErr error
	if table != nil {
		traversalErr = reaperDescendantsWithTable(cleanupCtx, cmd.Process.Pid, table, killReaperProcessRef)
	} else {
		traversalErr = reaperDescendantsWithRefs(cleanupCtx, cmd.Process.Pid, reaperValidatedPgrepLookup, captureReaperProcessIdentity, killReaperProcessRef)
	}

	// Always try the root group even when pgrep is unavailable or the
	// bounded traversal was interrupted. The root child is still owned by
	// this process while cleanup runs, so its group ID cannot be recycled.
	rootErr := killReaperRoot(cmd)
	if quiesceErr != nil {
		quiesceErr = fmt.Errorf("quiesce snapshot: %w", quiesceErr)
	}
	if traversalErr != nil {
		traversalErr = fmt.Errorf("descendant snapshot: %w", traversalErr)
	}
	if rootErr != nil {
		rootErr = fmt.Errorf("root cleanup: %w", rootErr)
	}
	return errors.Join(quiesceErr, traversalErr, rootErr)
}

func killReaperRoot(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	pgid, groupErr := reaperProcessGroupID(pid)
	if groupErr == nil && pgid == pid {
		// The root is a direct child and has not been reaped while this
		// cleanup is running. Check its handle immediately before using
		// the group ID, then fall back to the pidfd/handle signal below.
		if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
				return nil
			} else if !errors.Is(err, syscall.ESRCH) {
				return err
			}
		} else if !reaperProcessRefGone(err) {
			return err
		}
	}
	err := cmd.Process.Signal(syscall.SIGKILL)
	if err != nil && !reaperProcessRefGone(err) {
		return err
	}
	return nil
}

func killReaperProcess(pid int) error {
	ref, err := captureReaperProcessIdentity(pid)
	if err != nil {
		if reaperProcessRefGone(err) {
			return nil
		}
		return fmt.Errorf("reaper: identify process %d: %w", pid, err)
	}
	return killReaperProcessRef(ref)
}

func reaperValidatedPgrepLookup(ctx context.Context, parent int) ([]int, error) {
	first, err := reaperPgrepChildren(ctx, parent)
	if err != nil {
		return nil, err
	}
	second, err := reaperPgrepChildren(ctx, parent)
	if err != nil {
		return nil, err
	}
	allowed := make(map[int]struct{}, len(second))
	for _, pid := range second {
		allowed[pid] = struct{}{}
	}
	children := make([]int, 0, len(first))
	for _, pid := range first {
		if _, ok := allowed[pid]; ok {
			children = append(children, pid)
		}
	}
	return children, nil
}

func reaperDescendantsWithTable(ctx context.Context, root int, table map[int][]int, visit reaperProcessRefVisit) error {
	if root <= 0 {
		return fmt.Errorf("reaper: invalid root pid %d", root)
	}
	if table == nil {
		return fmt.Errorf("reaper: missing quiesced process table")
	}
	type tableState struct {
		ref        reaperProcessRef
		tombstoned bool
	}
	states := make(map[int]*tableState)
	order := make([]int, 0, maxReaperDescendantPIDs)
	type tableQueueItem struct {
		pid   int
		depth int
	}
	queue := []tableQueueItem{{pid: root}}
	queued := map[int]struct{}{root: {}}
	var errs []error
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		item := queue[0]
		queue = queue[1:]
		if item.depth > maxReaperDescendantDepth {
			errs = append(errs, errReaperDescendantLimit)
			break
		}
		parent := item.pid
		state, ok := states[parent]
		if !ok {
			ref, err := captureReaperProcessIdentity(parent)
			if err != nil {
				if !reaperProcessRefGone(err) && !errors.Is(err, errReaperProcessIdentityUnavailable) {
					errs = append(errs, fmt.Errorf("reaper: identify snapshot pid %d: %w", parent, err))
				}
				states[parent] = &tableState{ref: reaperProcessRef{pid: parent}, tombstoned: true}
				continue
			}
			state = &tableState{ref: ref}
			states[parent] = state
		}
		if state.tombstoned {
			continue
		}
		matches, err := reaperProcessRefMatches(state.ref)
		if err != nil || !matches {
			if reaperProcessRefGone(err) || errors.Is(err, errReaperProcessIdentityChanged) || errors.Is(err, errReaperProcessIdentityUnavailable) {
				state.tombstoned = true
			} else {
				errs = append(errs, fmt.Errorf("reaper: revalidate snapshot pid %d: %w", parent, err))
			}
			continue
		}
		for _, child := range table[parent] {
			if child <= 0 {
				errs = append(errs, fmt.Errorf("reaper: invalid snapshot child pid %d", child))
				continue
			}
			if _, seen := queued[child]; seen {
				continue
			}
			if len(queued) >= maxReaperDescendantPIDs {
				errs = append(errs, errReaperDescendantLimit)
				break
			}
			queued[child] = struct{}{}
			order = append(order, child)
			queue = append(queue, tableQueueItem{pid: child, depth: item.depth + 1})
		}
	}
	// The table is quiesced, so this is already a complete snapshot. Still
	// revalidate each identity immediately before the direct signal.
	failedSignals := make(map[int]error)
	for attempt := 0; attempt < 2; attempt++ {
		for i := len(order) - 1; i >= 0; i-- {
			pid := order[i]
			if attempt > 0 && failedSignals[pid] == nil {
				continue
			}
			state := states[pid]
			if state == nil || state.tombstoned {
				continue
			}
			matches, err := reaperProcessRefMatches(state.ref)
			if err != nil || !matches {
				if reaperProcessRefGone(err) || errors.Is(err, errReaperProcessIdentityChanged) || errors.Is(err, errReaperProcessIdentityUnavailable) {
					state.tombstoned = true
				} else if err != nil {
					errs = append(errs, fmt.Errorf("reaper: signal pid %d identity: %w", pid, err))
				}
				continue
			}
			if err := visit(state.ref); err != nil {
				failedSignals[pid] = err
				if attempt == 0 {
					errs = append(errs, fmt.Errorf("reaper: signal snapshot pid %d: %w", pid, err))
				}
			} else {
				delete(failedSignals, pid)
			}
		}
	}
	return errors.Join(errs...)
}

func reaperDescendantsWithLookup(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup) error {
	return reaperDescendantsWithRefs(ctx, root, lookup, func(pid int) (reaperProcessRef, error) {
		return reaperProcessRef{pid: pid}, nil
	}, func(ref reaperProcessRef) error { return visit(ref.pid) })
}

var (
	errReaperProcessIdentityChanged     = errors.New("reaper process identity changed")
	errReaperProcessIdentityUnavailable = errors.New("reaper process identity unavailable")
)

type reaperProcessRef struct {
	pid        int
	pgid       int
	startTime  string
	process    *os.Process
	identified bool
	check      func() (bool, error)
}

type reaperProcessRefLookup func(int) (reaperProcessRef, error)
type reaperProcessRefVisit func(reaperProcessRef) error

func captureReaperProcessIdentity(pid int) (reaperProcessRef, error) {
	if pid <= 0 {
		return reaperProcessRef{}, fmt.Errorf("reaper: invalid process pid %d", pid)
	}
	pgid, err := reaperProcessGroupID(pid)
	if err != nil {
		return reaperProcessRef{}, err
	}
	// On Linux, os.FindProcess retains a pidfd when the kernel supports it;
	// retaining this Process makes the eventual signal target the original
	// process rather than a recycled numeric PID.
	process, _ := os.FindProcess(pid)
	if process == nil {
		return reaperProcessRef{}, syscall.ESRCH
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return reaperProcessRef{}, err
	}
	startTime, startErr := reaperProcessStartTime(pid)
	if startErr != nil {
		if reaperProcessRefGone(startErr) {
			return reaperProcessRef{}, startErr
		}
		return reaperProcessRef{}, fmt.Errorf("%w: %v", errReaperProcessIdentityUnavailable, startErr)
	}
	return reaperProcessRef{pid: pid, pgid: pgid, startTime: startTime, process: process, identified: true}, nil
}

func reaperProcessRefMatches(ref reaperProcessRef) (bool, error) {
	if ref.check != nil {
		return ref.check()
	}
	if !ref.identified {
		return true, nil
	}
	if ref.startTime == "" {
		return false, errReaperProcessIdentityUnavailable
	}
	if ref.process != nil {
		if err := ref.process.Signal(syscall.Signal(0)); err != nil {
			return false, err
		}
	}
	pgid, err := reaperProcessGroupID(ref.pid)
	if err != nil {
		return false, err
	}
	if ref.pgid != 0 && pgid != ref.pgid {
		return false, errReaperProcessIdentityChanged
	}
	if ref.startTime != "" {
		startTime, err := reaperProcessStartTime(ref.pid)
		if err != nil {
			return false, err
		}
		if startTime != ref.startTime {
			return false, errReaperProcessIdentityChanged
		}
	}
	return true, nil
}

func reaperProcessRefGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) || errors.Is(err, os.ErrNotExist)
}

func killReaperProcessRef(ref reaperProcessRef) error {
	if !ref.identified {
		if ref.check != nil {
			return errReaperProcessIdentityUnavailable
		}
		return killReaperProcess(ref.pid)
	}
	ok, err := reaperProcessRefMatches(ref)
	if err != nil {
		if reaperProcessRefGone(err) {
			return nil
		}
		return err
	}
	if !ok {
		return nil
	}
	if ref.process != nil {
		err = ref.process.Signal(syscall.SIGKILL)
	} else {
		err = syscall.Kill(ref.pid, syscall.SIGKILL)
	}
	if err != nil && !reaperProcessRefGone(err) {
		return err
	}
	return nil
}

func reaperDescendantsWithLookupAndIdentity(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup, identify reaperDescendantIdentity) error {
	return reaperDescendantsWithRefs(ctx, root, lookup, func(pid int) (reaperProcessRef, error) {
		pgid, identified := identify(pid)
		if !identified && pid == root {
			return reaperProcessRef{pid: pid}, nil
		}
		if !identified {
			return reaperProcessRef{pid: pid, check: func() (bool, error) {
				return false, errReaperProcessIdentityUnavailable
			}}, nil
		}
		return reaperProcessRef{
			pid: pid, pgid: pgid, identified: true,
			check: func() (bool, error) {
				current, ok := identify(pid)
				if !ok || current != pgid {
					return false, errReaperProcessIdentityChanged
				}
				return true, nil
			},
		}, nil
	}, func(ref reaperProcessRef) error { return visit(ref.pid) })
}

func reaperDescendantsWithRefs(ctx context.Context, root int, lookup reaperDescendantLookup, lookupRef reaperProcessRefLookup, visitRef reaperProcessRefVisit) error {
	boundedCtx, cancel := context.WithTimeout(ctx, reaperCleanupTimeout)
	defer cancel()
	ctx = boundedCtx
	if root <= 0 {
		return fmt.Errorf("reaper: invalid root pid %d", root)
	}

	type descendantState struct {
		ref        reaperProcessRef
		tombstoned bool
	}
	rootRef, rootErr := lookupRef(root)
	if rootErr != nil {
		if reaperProcessRefGone(rootErr) {
			return nil
		}
		return fmt.Errorf("reaper: identify root pid %d: %w", root, rootErr)
	}
	states := map[int]*descendantState{root: {ref: rootRef}}
	parents := []int{root}
	queuedParents := map[int]struct{}{root: {}}
	order := make([]int, 0, maxReaperDescendantPIDs)
	visited := 0
	lookups := 0
	lookupErrors := make(map[int]error)
	signalErrors := make(map[int]error)
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
		} else {
			delete(signalErrors, pid)
		}
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
			rememberError("lookup", parent, err)
		} else {
			clearError("lookup", parent)
		}
		return children, err
	}
	addParent := func(pid int) {
		if _, ok := queuedParents[pid]; ok {
			return
		}
		queuedParents[pid] = struct{}{}
		parents = append(parents, pid)
	}

	stable := false
	stablePasses := 0
	for pass := 0; pass < maxReaperDescendantSnapshotPasses; pass++ {
		before := visited
		retry := false
		currentParents := append([]int(nil), parents...)
		for _, parentPID := range currentParents {
			if err := ctx.Err(); err != nil {
				return errors.Join(remainingErrors(), err)
			}
			state, ok := states[parentPID]
			if !ok || state.tombstoned {
				continue
			}
			if state.ref.identified || state.ref.check != nil {
				matches, err := reaperProcessRefMatches(state.ref)
				if err != nil || !matches {
					if reaperProcessRefGone(err) || errors.Is(err, errReaperProcessIdentityChanged) || errors.Is(err, errReaperProcessIdentityUnavailable) {
						state.tombstoned = true
					} else {
						retry = true
						if err != nil {
							rememberError("lookup", parentPID, err)
						}
					}
					continue
				}
			}
			children, err := lookupBounded(parentPID)
			if err != nil {
				retry = true
				continue
			}
			if state.ref.identified || state.ref.check != nil {
				matches, identityErr := reaperProcessRefMatches(state.ref)
				if identityErr != nil || !matches {
					if reaperProcessRefGone(identityErr) || errors.Is(identityErr, errReaperProcessIdentityChanged) || errors.Is(identityErr, errReaperProcessIdentityUnavailable) {
						state.tombstoned = true
					} else {
						retry = true
						if identityErr != nil {
							rememberError("lookup", parentPID, identityErr)
						}
					}
					continue
				}
			}
			for _, child := range children {
				if err := ctx.Err(); err != nil {
					return errors.Join(remainingErrors(), err)
				}
				if child <= 0 {
					retry = true
					rememberError("lookup", parentPID, fmt.Errorf("invalid descendant pid %d", child))
					continue
				}
				if _, exists := states[child]; exists {
					continue
				}
				if visited >= maxReaperDescendantPIDs {
					retry = true
					rememberError("lookup", parentPID, fmt.Errorf("%w: more than %d descendants", errReaperDescendantLimit, maxReaperDescendantPIDs))
					break
				}
				ref, refErr := lookupRef(child)
				if refErr != nil {
					if reaperProcessRefGone(refErr) || errors.Is(refErr, errReaperProcessIdentityUnavailable) {
						states[child] = &descendantState{ref: reaperProcessRef{pid: child}, tombstoned: true}
						addParent(child)
					} else {
						retry = true
						rememberError("lookup", child, refErr)
					}
					continue
				}
				states[child] = &descendantState{ref: ref}
				addParent(child)
				order = append(order, child)
				visited++
			}
		}
		if visited == before && !retry {
			stablePasses++
			if stablePasses >= 2 {
				stable = true
				break
			}
		} else {
			stablePasses = 0
		}
	}

	// The complete validated snapshot is now available. Revalidate each
	// stored identity immediately before signaling it; never expand a
	// tombstoned PID again.
	for attempt := 0; attempt < 2; attempt++ {
		for _, pid := range order {
			if attempt > 0 && signalErrors[pid] == nil {
				continue
			}
			state := states[pid]
			if state == nil || state.tombstoned {
				continue
			}
			matches, err := reaperProcessRefMatches(state.ref)
			if err != nil || !matches {
				if reaperProcessRefGone(err) || errors.Is(err, errReaperProcessIdentityChanged) {
					state.tombstoned = true
				} else if err != nil {
					rememberError("signal", pid, err)
				}
				continue
			}
			if err := visitRef(state.ref); err != nil {
				rememberError("signal", pid, err)
			} else {
				clearError("signal", pid)
			}
		}
	}
	if !stable {
		return errors.Join(remainingErrors(), errReaperDescendantLimit)
	}
	return remainingErrors()
}

const maxReaperProcessTableBytes = 1 << 20

func reaperProcessTableSnapshot(ctx context.Context) (map[int][]int, error) {
	psPath, err := trustedReaperHelperPath("ps")
	if err != nil {
		return nil, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(lookupCtx, psPath, "-e", "-o", "pid=", "-o", "ppid=", "-o", "pgid=")
	prepareReaperCommand(cmd)
	cmd.Cancel = func() error { return reaperCommandCancel(cmd) }
	cmd.WaitDelay = 50 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, maxReaperProcessTableBytes+1))
	if len(data) > maxReaperProcessTableBytes && cmd.Process != nil {
		_ = reaperCommandCancel(cmd)
	}
	waitErr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	if len(data) > maxReaperProcessTableBytes {
		return nil, fmt.Errorf("reaper: ps process table exceeded limit")
	}
	table := make(map[int][]int)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("reaper: malformed ps process row %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("reaper: invalid ps pid %q", fields[0])
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid < 0 {
			return nil, fmt.Errorf("reaper: invalid ps ppid %q", fields[1])
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil || pgid <= 0 {
			return nil, fmt.Errorf("reaper: invalid ps pgid %q", fields[2])
		}
		if ppid > 0 {
			table[ppid] = append(table[ppid], pid)
		}
	}
	return table, nil
}

func reaperTableDescendants(table map[int][]int, root int) []int {
	seen := map[int]struct{}{root: {}}
	queue := []int{root}
	ordered := make([]int, 0, len(seen))
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		ordered = append(ordered, parent)
		for _, child := range table[parent] {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	return ordered
}

func sameReaperPIDSet(first, second []int) bool {
	if len(first) != len(second) {
		return false
	}
	seen := make(map[int]struct{}, len(first))
	for _, pid := range first {
		seen[pid] = struct{}{}
	}
	for _, pid := range second {
		if _, ok := seen[pid]; !ok {
			return false
		}
	}
	return true
}

func quiesceReaperTree(ctx context.Context, root int) (map[int][]int, error) {
	var lastErr error
	var table map[int][]int
	previous := []int(nil)
	for pass := 0; pass < maxReaperDescendantSnapshotPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		table, err = reaperProcessTableSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		pids := reaperTableDescendants(table, root)
		if len(pids) > maxReaperDescendantPIDs {
			return nil, fmt.Errorf("%w: quiesce snapshot exceeded %d processes", errReaperDescendantLimit, maxReaperDescendantPIDs)
		}
		if sameReaperPIDSet(previous, pids) {
			return table, lastErr
		}
		previous = append(previous[:0], pids...)
		for _, pid := range pids {
			ref, err := captureReaperProcessIdentity(pid)
			if err != nil {
				if !reaperProcessRefGone(err) {
					lastErr = err
				}
				continue
			}
			matches, matchErr := reaperProcessRefMatches(ref)
			if matchErr != nil || !matches {
				if !reaperProcessRefGone(matchErr) && !errors.Is(matchErr, errReaperProcessIdentityChanged) && !errors.Is(matchErr, errReaperProcessIdentityUnavailable) {
					lastErr = matchErr
				}
				continue
			}
			if err := ref.process.Signal(syscall.SIGSTOP); err != nil && !reaperProcessRefGone(err) {
				lastErr = err
			}
		}
	}
	return table, errors.Join(lastErr, errReaperDescendantLimit)
}

func trustedReaperPgrepPath() (string, error) {
	return trustedReaperHelperPath("pgrep")
}

func reaperPgrepChildren(ctx context.Context, parent int) ([]int, error) {
	pgrepPath, err := trustedReaperPgrepPath()
	if err != nil {
		return nil, fmt.Errorf("reaper: locate pgrep: %w", err)
	}
	return reaperPgrepChildrenWithPath(ctx, parent, pgrepPath)
}

type reaperLimitBuffer struct {
	data      []byte
	remaining int
}

func (b *reaperLimitBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	b.data = append(b.data, p...)
	b.remaining -= len(p)
	return original, nil
}

func (b *reaperLimitBuffer) String() string { return string(b.data) }

func reaperPgrepChildrenWithPath(ctx context.Context, parent int, pgrepPath string) ([]int, error) {
	cmd := exec.CommandContext(ctx, pgrepPath, "-P", strconv.Itoa(parent))
	prepareReaperCommand(cmd)
	cmd.WaitDelay = reaperPgrepWaitDelay
	cmd.Cancel = func() error { return reaperCommandCancel(cmd) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := reaperLimitBuffer{remaining: 4096}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	const maxOutput = 64 * 1024
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	if len(out) > maxOutput && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if readErr != nil {
		return nil, readErr
	}
	if len(out) > maxOutput {
		return nil, fmt.Errorf("reaper: pgrep output exceeded limit")
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			message := strings.TrimSpace(stderr.String())
			if exitErr.ExitCode() == 1 && message == "" {
				return nil, nil
			}
			if message != "" {
				return nil, fmt.Errorf("reaper: pgrep -P %d failed: %w: %s", parent, waitErr, message)
			}
			return nil, fmt.Errorf("reaper: pgrep -P %d failed: %w", parent, waitErr)
		}
		return nil, fmt.Errorf("reaper: pgrep -P %d failed: %w", parent, waitErr)
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
