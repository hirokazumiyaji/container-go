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
	reaperSignalTimeout               = time.Second
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

	// A timed entry can have its own process group. Capture the root identity
	// before quiescing and retain its pidfd/process handle through signaling.
	rootRef, rootIdentityErr := captureReaperProcessIdentityContext(cleanupCtx, cmd.Process.Pid)
	quiesced, quiesceErr := quiesceReaperTreeWithRefs(cleanupCtx, cmd.Process.Pid)
	if rootIdentityErr != nil && !reaperProcessRefGone(rootIdentityErr) {
		quiesceErr = errors.Join(quiesceErr, rootIdentityErr)
	}

	// Traversal and identity checks get a fresh budget. A timeout while
	// quiescing must not consume the only budget needed to kill processes
	// that were already stopped successfully.
	signalCtx, signalCancel := context.WithTimeout(context.Background(), reaperSignalTimeout)
	defer signalCancel()
	var traversalErr error
	if quiesced.table != nil {
		traversalErr = reaperDescendantsWithTable(signalCtx, cmd.Process.Pid, quiesced.table, killReaperProcessRefContext)
	} else {
		lookup := reaperValidatedPgrepLookup
		if _, err := trustedReaperPgrepPath(); err != nil {
			lookup = reaperValidatedPsLookup
		}
		traversalErr = reaperDescendantsWithRefs(signalCtx, cmd.Process.Pid, lookup, captureReaperProcessIdentityContext, killReaperProcessRefContext)
	}

	// A stopped process cannot execute or create descendants, but it can
	// still be left stopped forever if the identity context expires before
	// the normal traversal reaches it. Kill every retained stopped reference
	// directly as a final, bounded safety net.
	stoppedErr := killReaperStoppedRefs(quiesced.stopped)
	// Always try the root group even when lookup/traversal was interrupted.
	rootErr := killReaperRoot(signalCtx, cmd, rootRef)
	if quiesceErr != nil {
		quiesceErr = fmt.Errorf("quiesce snapshot: %w", quiesceErr)
	}
	if traversalErr != nil {
		traversalErr = fmt.Errorf("descendant snapshot: %w", traversalErr)
	}
	if stoppedErr != nil {
		stoppedErr = fmt.Errorf("stopped process cleanup: %w", stoppedErr)
	}
	if rootErr != nil {
		rootErr = fmt.Errorf("root cleanup: %w", rootErr)
	}
	return errors.Join(quiesceErr, traversalErr, stoppedErr, rootErr)
}

func killReaperRoot(ctx context.Context, cmd *exec.Cmd, ref reaperProcessRef) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	identityOK := false
	var identityErr error
	if ref.identified {
		matches, err := reaperProcessRefMatchesContext(ctx, ref)
		if err != nil {
			if reaperProcessRefGone(err) {
				return nil
			}
			identityErr = err
		} else {
			identityOK = matches
		}
	}
	// A negative group signal is numeric, so only use it while the retained
	// root identity still matches. If identity was lost, use the direct
	// process handle (pidfd where available) and never fall back to a number.
	if identityOK && ref.pgid == pid {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
			return nil
		} else if !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	err := cmd.Process.Signal(syscall.SIGKILL)
	if err != nil && !reaperProcessRefGone(err) {
		return err
	}
	return identityErr
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

func reaperValidatedPsLookup(ctx context.Context, parent int) ([]int, error) {
	first, err := reaperProcessTableSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	second, err := reaperProcessTableSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[int]struct{}, len(second[parent]))
	for _, pid := range second[parent] {
		allowed[pid] = struct{}{}
	}
	children := make([]int, 0, len(first[parent]))
	for _, pid := range first[parent] {
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
	signalCtx, cancel := context.WithTimeout(ctx, reaperSignalTimeout)
	defer cancel()
	ctx = signalCtx
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
			ref, err := captureReaperProcessIdentityContext(ctx, parent)
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
		matches, err := reaperProcessRefMatchesContext(ctx, state.ref)
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
			matches, err := reaperProcessRefMatchesContext(ctx, state.ref)
			if err != nil || !matches {
				if reaperProcessRefGone(err) || errors.Is(err, errReaperProcessIdentityChanged) || errors.Is(err, errReaperProcessIdentityUnavailable) {
					state.tombstoned = true
				} else if err != nil {
					errs = append(errs, fmt.Errorf("reaper: signal pid %d identity: %w", pid, err))
				}
				continue
			}
			if err := visit(ctx, state.ref); err != nil {
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
	return reaperDescendantsWithRefs(ctx, root, lookup, func(_ context.Context, pid int) (reaperProcessRef, error) {
		return reaperProcessRef{pid: pid}, nil
	}, func(_ context.Context, ref reaperProcessRef) error { return visit(ref.pid) })
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

type reaperProcessRefLookup func(context.Context, int) (reaperProcessRef, error)
type reaperProcessRefVisit func(context.Context, reaperProcessRef) error

func captureReaperProcessIdentity(pid int) (reaperProcessRef, error) {
	return captureReaperProcessIdentityContext(context.Background(), pid)
}

func captureReaperProcessIdentityContext(ctx context.Context, pid int) (reaperProcessRef, error) {
	if err := ctx.Err(); err != nil {
		return reaperProcessRef{}, err
	}
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
	startTime, startErr := reaperProcessStartTime(ctx, pid)
	if startErr != nil {
		if reaperProcessRefGone(startErr) {
			return reaperProcessRef{}, startErr
		}
		return reaperProcessRef{}, fmt.Errorf("%w: %v", errReaperProcessIdentityUnavailable, startErr)
	}
	return reaperProcessRef{pid: pid, pgid: pgid, startTime: startTime, process: process, identified: true}, nil
}

func reaperProcessRefMatchesContext(ctx context.Context, ref reaperProcessRef) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
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
		startTime, err := reaperProcessStartTime(ctx, ref.pid)
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
	return killReaperProcessRefContext(context.Background(), ref)
}

func killReaperProcessRefContext(ctx context.Context, ref reaperProcessRef) error {
	if !ref.identified {
		return errReaperProcessIdentityUnavailable
	}
	ok, err := reaperProcessRefMatchesContext(ctx, ref)
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

// killReaperStoppedRefs is intentionally independent of the traversal
// context. A successful SIGSTOP reserves the PID until the process is
// resumed or exits, so a retained process handle (or, on platforms without
// pidfd, the still-reserved numeric PID) can be safely killed even when the
// identity lookup budget has already expired.
func killReaperStoppedRefs(refs []reaperProcessRef) error {
	var errs []error
	seen := make(map[int]struct{}, len(refs))
	for _, ref := range refs {
		if ref.pid <= 0 {
			errs = append(errs, fmt.Errorf("invalid stopped process pid %d", ref.pid))
			continue
		}
		if _, ok := seen[ref.pid]; ok {
			continue
		}
		seen[ref.pid] = struct{}{}
		var err error
		if ref.process != nil {
			err = ref.process.Signal(syscall.SIGKILL)
		} else {
			err = syscall.Kill(ref.pid, syscall.SIGKILL)
		}
		if err != nil && !reaperProcessRefGone(err) {
			errs = append(errs, fmt.Errorf("kill stopped process %d: %w", ref.pid, err))
		}
	}
	return errors.Join(errs...)
}

func reaperDescendantsWithLookupAndIdentity(ctx context.Context, root int, visit func(int) error, lookup reaperDescendantLookup, identify reaperDescendantIdentity) error {
	return reaperDescendantsWithRefs(ctx, root, lookup, func(_ context.Context, pid int) (reaperProcessRef, error) {
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
	}, func(_ context.Context, ref reaperProcessRef) error { return visit(ref.pid) })
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
	rootRef, rootErr := lookupRef(ctx, root)
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
				matches, err := reaperProcessRefMatchesContext(ctx, state.ref)
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
				matches, identityErr := reaperProcessRefMatchesContext(ctx, state.ref)
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
				ref, refErr := lookupRef(ctx, child)
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
			matches, err := reaperProcessRefMatchesContext(ctx, state.ref)
			if err != nil || !matches {
				if reaperProcessRefGone(err) || errors.Is(err, errReaperProcessIdentityChanged) {
					state.tombstoned = true
				} else if err != nil {
					rememberError("signal", pid, err)
				}
				continue
			}
			if err := visitRef(ctx, state.ref); err != nil {
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

func sameReaperProcessTable(first, second map[int][]int) bool {
	parents := make(map[int]struct{}, len(first)+len(second))
	for parent := range first {
		parents[parent] = struct{}{}
	}
	for parent := range second {
		parents[parent] = struct{}{}
	}
	for parent := range parents {
		if !sameReaperPIDSet(first[parent], second[parent]) {
			return false
		}
	}
	return true
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

type reaperQuiescedSnapshot struct {
	table   map[int][]int
	stopped []reaperProcessRef
}

// reaperProcessSubtree removes unrelated host edges before snapshots are
// compared. The process-table helper necessarily reports the whole host,
// but unrelated process churn must not prevent the target tree reaching a
// fixed point.
func reaperProcessSubtree(table map[int][]int, root int) map[int][]int {
	pids := reaperTableDescendants(table, root)
	inTree := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		inTree[pid] = struct{}{}
	}
	subtree := make(map[int][]int)
	for _, parent := range pids {
		for _, child := range table[parent] {
			if _, ok := inTree[child]; !ok || child <= 0 {
				continue
			}
			subtree[parent] = append(subtree[parent], child)
		}
	}
	return subtree
}

func quiesceReaperTreeWithRefs(ctx context.Context, root int) (reaperQuiescedSnapshot, error) {
	var result reaperQuiescedSnapshot
	var lastErr error
	var previous map[int][]int
	stablePasses := 0
	stoppedSeen := make(map[int]struct{})
	for pass := 0; pass < maxReaperDescendantSnapshotPasses; pass++ {
		if err := ctx.Err(); err != nil {
			result.table = previous
			return result, errors.Join(lastErr, err)
		}
		table, err := reaperProcessTableSnapshot(ctx)
		if err != nil {
			result.table = previous
			return result, errors.Join(lastErr, err)
		}
		subtree := reaperProcessSubtree(table, root)
		pids := reaperTableDescendants(subtree, root)
		if len(pids) > maxReaperDescendantPIDs {
			result.table = subtree
			return result, fmt.Errorf("%w: quiesce snapshot exceeded %d processes", errReaperDescendantLimit, maxReaperDescendantPIDs)
		}
		if sameReaperProcessTable(previous, subtree) {
			stablePasses++
			if stablePasses >= 1 {
				result.table = subtree
				return result, lastErr
			}
		} else {
			stablePasses = 0
		}
		previous = subtree
		result.table = subtree
		for _, pid := range pids {
			if err := ctx.Err(); err != nil {
				return result, errors.Join(lastErr, err)
			}
			ref, err := captureReaperProcessIdentityContext(ctx, pid)
			if err != nil {
				if !reaperProcessRefGone(err) {
					lastErr = err
				}
				continue
			}
			matches, matchErr := reaperProcessRefMatchesContext(ctx, ref)
			if matchErr != nil || !matches {
				if !reaperProcessRefGone(matchErr) && !errors.Is(matchErr, errReaperProcessIdentityChanged) && !errors.Is(matchErr, errReaperProcessIdentityUnavailable) {
					lastErr = matchErr
				}
				continue
			}
			if err := ref.process.Signal(syscall.SIGSTOP); err != nil {
				if !reaperProcessRefGone(err) {
					lastErr = err
				}
				continue
			}
			if _, seen := stoppedSeen[pid]; !seen {
				result.stopped = append(result.stopped, ref)
				stoppedSeen[pid] = struct{}{}
			}
		}
	}
	result.table = previous
	return result, errors.Join(lastErr, errReaperDescendantLimit)
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
