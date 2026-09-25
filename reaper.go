package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated as an Apple Container name (or a Docker container's
// full 64-lowercase-hex ID), and the script itself disables globbing and
// quotes every expansion the IDs reach.
// Each backend call runs with a per-entry timeout implemented with
// background jobs and kill (timeout(1) is not standard on macOS), so a
// hung daemon cannot wedge deletion of later entries. Failures stay
// silent (|| true) by design: the reaper is last-resort insurance.
// When a creation generation is known, the script inspects first and
// reads the creation label as a structural JSON field: the match is
// anchored at line start on the quoted key, so label values or other
// text containing the same characters cannot satisfy it. When inspect
// also reports an immutable "Id" (Docker), the delete targets that ID
// instead of the name, so a same-name replacement created after the
// check is simply not found. Apple Container has no such ID; there the
// delete necessarily goes by name.
//
// The pipe protocol has +id and -id records. awk removes a completed
// record as soon as it reads the cancellation, and emits only the
// remaining active records after EOF. Thus the shell-side state and
// the pipe backlog are bounded by the number of live registrations,
// rather than by the number of containers created during the process
// lifetime.
//
// SHELLOPTS may enable monitor mode. Disable it before any background
// helpers so they remain in the reaper's process group.
const reaperScript = `set -f
set +m
bin="$1"
sub="$2"
key="$3"
run_with_timeout() {
  "$@" >/dev/null 2>&1 & pid=$!
  (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!
  wait "$pid" 2>/dev/null
  rc=$?
  kill "$killer" 2>/dev/null
  wait "$killer" 2>/dev/null
  return $rc
}
awk '
  substr($0, 1, 2) == "+ " {
    active["x" substr($0, 3)] = 1
    next
  }
  substr($0, 1, 2) == "- " {
    delete active["x" substr($0, 3)]
    next
  }
  END {
    for (entry in active) print substr(entry, 2)
  }
' | while IFS= read -r line; do
  [ -z "$line" ] && continue
  id=${line%% *}
  creation=${line#* }
  [ "$id" = "$line" ] && creation=""
  target="$id"
  if [ -n "$creation" ]; then
    tmp=$(mktemp 2>/dev/null) || continue
    ("$bin" inspect "$id" >"$tmp" 2>/dev/null & pid=$!; (sleep 10; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || { rm -f "$tmp"; continue; }
    got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
    uid=$(sed -n 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/\1/p' "$tmp" 2>/dev/null | head -n 1)
    rm -f "$tmp"
    [ "$got" = "$creation" ] || continue
    [ -n "$uid" ] && target="$uid"
  fi
  run_with_timeout "$bin" "$sub" --force "$target" || true
done
`

const (
	maxReaperSpawnFailures = 3
	// Keep a short completion history for diagnostics without retaining
	// one record for every container created by a long-lived process.
	maxReaperCompletedEntries = 1024
	initialReaperSpawnBackoff = time.Second
	maxReaperSpawnBackoff     = 30 * time.Second
)

// Reaper writes are deliberately short-lived operations. A reaper reader
// can stop draining its stdin while a backend call is stalled; the parent
// must not turn that condition into a process-wide lock convoy.
var (
	reaperWriteTimeout       = 500 * time.Millisecond
	reaperProcessStopTimeout = time.Second
	reaperOperationTimeout   = 5 * time.Second
)

var (
	errReaperSpawnCooldown = errors.New("reaper: spawn retry cooldown active")
	errReaperSpawnFailed   = errors.New("reaper: giving up after repeated spawn failures")
	errReaperWriteTimeout  = errors.New("reaper: pipe write timed out")
)

// breQuote escapes a literal for use inside the reaper's sed basic
// regular expression, so the label key's dots match only dots.
func breQuote(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`\.*[]^$/`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// creationRE validates the hex generation ID passed to the reaper.
var creationRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

type reaperEntry struct {
	id       string
	creation string
}

type reaperRegistration struct {
	reaper *reaper
	entry  reaperEntry
}

// reaperProcess owns one child and the identity used to stop it. The
// identity is cleared as soon as Wait reaps the child. In particular, a
// later register or unregister must never derive a process-group signal
// from a PID that the operating system may already have recycled.
type reaperProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}

	mu     sync.Mutex
	killMu sync.Mutex
	pid    int
	pgid   int
	reaped bool
}

func (p *reaperProcess) markReaped() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.reaped = true
	p.pid = 0
	p.pgid = 0
	p.mu.Unlock()
}

func (p *reaperProcess) identity() (cmd *exec.Cmd, pid, pgid int, live bool) {
	if p == nil {
		return nil, 0, 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd, p.pid, p.pgid, !p.reaped
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	// opMu serializes lifecycle transitions while mu protects the
	// in-memory state. Pipe writes and child shutdown happen with opMu
	// held but never with mu held, so a stalled reader cannot block
	// unrelated state inspection or state-mutex users indefinitely.
	opMu sync.Mutex
	mu   sync.Mutex

	// process is the current child. The cmd/stdin/exited aliases are kept
	// for the small test helpers and for callers that inspect the current
	// process while it is alive.
	process *reaperProcess
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	exited  chan struct{}
	pid     int
	pgid    int

	// entries is the active replay set. completed is a bounded recent
	// history; neither completed entries nor their cancellation records
	// are replayed into a replacement process.
	entries       []reaperEntry
	completed     []reaperEntry
	spawnFailures int
	gaveUp        bool
	gaveUpLogged  bool
	retryAt       time.Time
	retryLevel    int
	now           func() time.Time
	command       func() *exec.Cmd
	backoff       func(int) time.Duration
	killProcess   func(*reaperProcess)
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:      binary,
		subcommand:  subcommand,
		now:         time.Now,
		killProcess: killReaperProcess,
	}
}

// register adds a container ID to the reaper's active kill list,
// spawning or respawning the reaper process as needed. Apple targets
// are names; Docker targets may be a full 64-hex ID. creation is the
// generation ID from creationLabel; empty skips the generation check
// for backward compatibility.
func (r *reaper) register(id, creation string) error {
	if !nameRE.MatchString(id) && !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reaperOperationTimeout)
	defer cancel()
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.registerContext(ctx, reaperEntry{id: id, creation: creation})
}

func (r *reaper) registerContext(ctx context.Context, entry reaperEntry) error {
	r.mu.Lock()
	duplicate := r.containsActiveLocked(entry)
	if !duplicate {
		r.removeCompletedLocked(entry)
		r.entries = append(r.entries, entry)
	}
	if !r.retryReadyLocked() {
		err := r.spawnCooldownErrorLocked()
		r.mu.Unlock()
		return err
	}
	process := r.processLocked()
	r.mu.Unlock()

	// Do not grow active state when the same registration is repeated.
	// Still verify the pipe, because a repeated registration can be the
	// first observation that an old reaper child has exited.
	if process != nil && processLive(process) {
		if err := writeReaperRecord(ctx, process.stdin, reaperRecord("+", entry)); err == nil {
			r.markWriteSuccess(process)
			return nil
		}
	}
	return r.recoverAndReplay(ctx, process)
}

func (r *reaper) markWriteSuccess(process *reaperProcess) {
	r.mu.Lock()
	if r.process == process {
		r.clearSpawnFailureLocked()
	}
	r.mu.Unlock()
}

func reaperRecord(operation string, e reaperEntry) string {
	line := operation + " " + e.id
	if e.creation != "" {
		line += " " + e.creation
	}
	return line + "\n"
}

func processLive(process *reaperProcess) bool {
	if process == nil || process.stdin == nil || channelClosed(process.exited) {
		return false
	}
	_, _, _, live := process.identity()
	return live
}

func (r *reaper) processLocked() *reaperProcess {
	if r.process != nil {
		return r.process
	}
	if r.cmd == nil && r.stdin == nil && r.exited == nil {
		return nil
	}
	// Keep the compatibility aliases coherent for package tests and for
	// a process observed between assignment and the next state transition.
	r.process = &reaperProcess{
		cmd:    r.cmd,
		stdin:  r.stdin,
		exited: r.exited,
		pid:    r.pid,
		pgid:   r.pgid,
		reaped: channelClosed(r.exited),
	}
	return r.process
}

func writeReaperRecord(ctx context.Context, stdin io.WriteCloser, line string) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	writeCtx, cancel := context.WithTimeout(ctx, reaperWriteTimeout)
	defer cancel()
	ctx = writeCtx
	if err := ctx.Err(); err != nil {
		return err
	}

	// StdinPipe returns an *os.File, whose write deadline normally
	// interrupts a blocked pipe write. The goroutine/select is still used
	// so a test double (or a writer with a broken deadline implementation)
	// cannot make the lifecycle operation unbounded. On cancellation the
	// caller closes the writer only after detaching the child, preventing
	// EOF from racing the completed-entry cancellation.
	deadlineWriter, hasDeadline := stdin.(interface {
		SetWriteDeadline(time.Time) error
	})
	if hasDeadline {
		deadline := time.Now().Add(reaperWriteTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if err := deadlineWriter.SetWriteDeadline(deadline); err != nil {
			hasDeadline = false
		}
	}
	clearDeadline := func() {
		if hasDeadline {
			_ = deadlineWriter.SetWriteDeadline(time.Time{})
		}
	}

	type writeResult struct {
		n   int
		err error
	}
	done := make(chan writeResult, 1)
	go func() {
		n, err := io.WriteString(stdin, line)
		done <- writeResult{n: n, err: err}
	}()
	select {
	case result := <-done:
		clearDeadline()
		if result.err == nil && result.n != len(line) {
			return io.ErrShortWrite
		}
		if result.err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", errReaperWriteTimeout, ctx.Err())
			}
			return fmt.Errorf("%w: %v", errReaperWriteTimeout, result.err)
		}
		return nil
	case <-ctx.Done():
		// Do not close the pipe here. EOF would make the reaper process
		// its active set before the caller has detached and killed this
		// child. The lifecycle owner closes the writer as part of that
		// atomic recovery step instead.
		clearDeadline()
		return fmt.Errorf("%w: %w", errReaperWriteTimeout, ctx.Err())
	}
}

// unregister removes a successfully cleaned entry from the active
// replay set. The cancellation is sent to the existing child so it
// cannot delete an already-completed container if the parent later
// exits. If the child is gone, only the still-active entries are
// replayed into its replacement.
func (r *reaper) unregister(id, creation string) error {
	if !nameRE.MatchString(id) && !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reaperOperationTimeout)
	defer cancel()
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.unregisterContext(ctx, reaperEntry{id: id, creation: creation})
}

func (r *reaper) unregisterContext(ctx context.Context, entry reaperEntry) error {
	r.mu.Lock()
	if !r.removeActiveLocked(entry) {
		r.mu.Unlock()
		return nil
	}
	r.rememberCompletedLocked(entry)
	process := r.processLocked()
	r.mu.Unlock()

	if process != nil && processLive(process) {
		if err := writeReaperRecord(ctx, process.stdin, reaperRecord("-", entry)); err == nil {
			r.markWriteSuccess(process)
			// Keep the child alive with an empty active set. This avoids
			// a process spawn for every sequential create/terminate
			// cycle; EOF still lets the child exit without deletes.
			return nil
		}
	}

	// A failed or bounded-out cancellation cannot be trusted to reach the
	// old child. Detach and stop it before replaying; otherwise it could
	// still delete the completed entry after the parent exits.
	return r.recoverAndReplay(ctx, process)
}

func (r *reaper) containsActiveLocked(entry reaperEntry) bool {
	for _, active := range r.entries {
		if active == entry {
			return true
		}
	}
	return false
}

func (r *reaper) removeActiveLocked(entry reaperEntry) bool {
	for i, active := range r.entries {
		if active == entry {
			r.entries = slices.Delete(r.entries, i, i+1)
			if len(r.entries) == 0 {
				r.entries = nil
			} else if len(r.entries)*2 < cap(r.entries) {
				r.entries = slices.Clone(r.entries)
			}
			return true
		}
	}
	return false
}

func (r *reaper) rememberCompletedLocked(entry reaperEntry) {
	if len(r.completed) == maxReaperCompletedEntries {
		copy(r.completed, r.completed[1:])
		r.completed = r.completed[:maxReaperCompletedEntries-1]
	}
	r.completed = append(r.completed, entry)
}

func (r *reaper) removeCompletedLocked(entry reaperEntry) {
	for i, completed := range r.completed {
		if completed == entry {
			r.completed = slices.Delete(r.completed, i, i+1)
			return
		}
	}
}

// recoverAndReplay detaches the failed process before doing any signal
// work. Once detached, a later lifecycle operation cannot accidentally
// signal the old PID/PGID, even if shutdown itself takes time.
func (r *reaper) recoverAndReplay(ctx context.Context, process *reaperProcess) error {
	if detached := r.detachProcess(process); detached != nil {
		if err := r.stopProcess(detached, ctx); err != nil {
			return err
		}
	}
	return r.respawnAndReplay(ctx)
}

func (r *reaper) detachProcess(process *reaperProcess) *reaperProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	if process == nil {
		process = r.process
	}
	if process == nil || r.process != process {
		return nil
	}
	// Clear every route to the numeric identity before releasing the
	// state lock. The caller now owns the detached process exclusively.
	r.process = nil
	r.cmd = nil
	r.stdin = nil
	r.exited = nil
	r.pid = 0
	r.pgid = 0
	return process
}

func (r *reaper) stopProcess(process *reaperProcess, ctx context.Context) error {
	if process == nil {
		return nil
	}
	if _, _, _, live := process.identity(); live {
		if r.killProcess != nil {
			r.killProcess(process)
		} else {
			killReaperProcess(process)
		}
	}
	if process.stdin != nil {
		_ = process.stdin.Close()
	}
	if process.cmd == nil || process.cmd.Process == nil || process.exited == nil {
		return nil
	}

	// Shutdown gets its own small budget even when the caller's pipe-write
	// context has just expired. This keeps cancellation bounded while still
	// giving SIGKILL a chance to settle the child before replay.
	stopCtx, cancel := context.WithTimeout(context.Background(), reaperProcessStopTimeout)
	defer cancel()
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) > 0 && time.Until(deadline) < reaperProcessStopTimeout {
			stopCtx, cancel = context.WithDeadline(context.Background(), deadline)
			defer cancel()
		}
	}
	select {
	case <-process.exited:
		return nil
	case <-stopCtx.Done():
		return fmt.Errorf("reaper: child shutdown: %w", stopCtx.Err())
	}
}

func (r *reaper) respawnAndReplay(ctx context.Context) error {
	// A recovery path may already have detached the failed child. This
	// also covers callers that enter replay after a child exited by itself.
	if current := r.detachProcess(nil); current != nil {
		if err := r.stopProcess(current, ctx); err != nil {
			return err
		}
	}

	r.mu.Lock()
	if len(r.entries) == 0 {
		r.clearSpawnFailureLocked()
		r.mu.Unlock()
		return nil
	}
	if !r.retryReadyLocked() {
		err := r.spawnCooldownErrorLocked()
		r.mu.Unlock()
		return err
	}
	entries := slices.Clone(r.entries)
	r.mu.Unlock()

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		r.mu.Lock()
		process, err := r.spawnProcessLocked()
		r.mu.Unlock()
		if err != nil {
			lastErr = err
			r.mu.Lock()
			r.recordSpawnFailureLocked()
			failed := r.spawnFailures >= maxReaperSpawnFailures
			r.mu.Unlock()
			if failed {
				break
			}
			continue
		}

		replayed := true
		for _, entry := range entries {
			if err := writeReaperRecord(ctx, process.stdin, reaperRecord("+", entry)); err != nil {
				lastErr = err
				replayed = false
				break
			}
		}
		if replayed {
			r.mu.Lock()
			if r.process == process {
				r.clearSpawnFailureLocked()
			}
			r.mu.Unlock()
			return nil
		}
		if detached := r.detachProcess(process); detached != nil {
			if stopErr := r.stopProcess(detached, ctx); stopErr != nil {
				lastErr = stopErr
				break
			}
		}
		r.mu.Lock()
		r.recordSpawnFailureLocked()
		failed := r.spawnFailures >= maxReaperSpawnFailures
		r.mu.Unlock()
		if failed {
			break
		}
	}
	r.mu.Lock()
	if !r.gaveUp {
		r.recordSpawnFailureLocked()
	}
	logGiveUp := !r.gaveUpLogged
	if logGiveUp {
		r.gaveUpLogged = true
	}
	r.mu.Unlock()
	if logGiveUp {
		log.Printf("container-go: reaper giving up temporarily after %d consecutive failures (binary=%q); retrying after cooldown", maxReaperSpawnFailures, r.binary)
	}
	if lastErr == nil {
		lastErr = errReaperSpawnFailed
	}
	return fmt.Errorf("%w: %v", errReaperSpawnFailed, lastErr)
}

func (r *reaper) spawnProcessLocked() (*reaperProcess, error) {
	var cmd *exec.Cmd
	if r.command != nil {
		cmd = r.command()
	} else {
		cmd = exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel))
	}
	if cmd == nil {
		return nil, errors.New("reaper: nil spawn command")
	}
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	process := &reaperProcess{
		cmd:    cmd,
		stdin:  stdin,
		exited: make(chan struct{}),
		pid:    cmd.Process.Pid,
		pgid:   reaperProcessGroupID(cmd),
	}
	r.process = process
	r.cmd, r.stdin, r.exited = cmd, stdin, process.exited
	r.pid, r.pgid = process.pid, process.pgid
	go func() {
		_ = cmd.Wait()
		process.markReaped()
		r.mu.Lock()
		if r.process == process {
			// Retain only the completion channel for waiters. Clearing the
			// command and pipe aliases prevents a delayed lifecycle call
			// from treating a reaped child as an active writer.
			r.cmd = nil
			r.stdin = nil
			r.pid = 0
			r.pgid = 0
		}
		r.mu.Unlock()
		close(process.exited)
	}()
	return process, nil
}

// spawnLocked preserves the original package-test helper shape. New code
// uses spawnProcessLocked when it needs the ownership-bearing child.
func (r *reaper) spawnLocked() error {
	_, err := r.spawnProcessLocked()
	return err
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (r *reaper) nowLocked() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *reaper) backoffLocked(level int) time.Duration {
	if level < 1 {
		level = 1
	}
	if r.backoff != nil {
		delay := r.backoff(level)
		if delay < 0 {
			return 0
		}
		if delay > maxReaperSpawnBackoff {
			return maxReaperSpawnBackoff
		}
		return delay
	}
	delay := initialReaperSpawnBackoff
	for i := 1; i < level; i++ {
		if delay >= maxReaperSpawnBackoff/2 {
			return maxReaperSpawnBackoff
		}
		delay *= 2
	}
	if delay > maxReaperSpawnBackoff {
		return maxReaperSpawnBackoff
	}
	return delay
}

func (r *reaper) enterCooldownLocked() {
	if r.retryLevel < 32 {
		r.retryLevel++
	}
	r.gaveUp = true
	r.retryAt = r.nowLocked().Add(r.backoffLocked(r.retryLevel))
}

func (r *reaper) recordSpawnFailureLocked() {
	r.spawnFailures++
	if r.spawnFailures >= maxReaperSpawnFailures {
		r.enterCooldownLocked()
	}
}

func (r *reaper) clearSpawnFailureLocked() {
	r.spawnFailures = 0
	r.gaveUp = false
	r.gaveUpLogged = false
	r.retryAt = time.Time{}
	r.retryLevel = 0
}

func (r *reaper) retryReadyLocked() bool {
	if r.spawnFailures >= maxReaperSpawnFailures && !r.gaveUp {
		r.enterCooldownLocked()
	}
	if !r.gaveUp {
		return true
	}
	if !r.retryAt.IsZero() && r.nowLocked().Before(r.retryAt) {
		return false
	}
	// The cooldown is over, but retain retryLevel until a spawn actually
	// succeeds so repeated recovery failures back off progressively.
	r.spawnFailures = 0
	r.gaveUp = false
	r.gaveUpLogged = false
	r.retryAt = time.Time{}
	return true
}

func (r *reaper) spawnCooldownErrorLocked() error {
	if r.retryAt.IsZero() {
		return errReaperSpawnCooldown
	}
	return fmt.Errorf("%w until %s", errReaperSpawnCooldown, r.retryAt.Format(time.RFC3339Nano))
}

// closeStdin hands the reaper the same EOF it would see on parent
// death. Test hook and best-effort shutdown.
func (r *reaper) closeStdin() {
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

// killForTest kills the reaper process group and waits until the child
// is reaped, so the next write deterministically fails. It uses the same
// detach-before-signal path as production recovery.
func (r *reaper) killForTest() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	process := r.detachProcess(nil)
	if process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reaperProcessStopTimeout)
	defer cancel()
	_ = r.stopProcess(process, ctx)
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

// registerWithGlobalReaper best-effort registers a container with the
// process-wide reaper for its backend binary. Reaper trouble never
// fails container startup. The reaper needs /bin/sh, so on Windows
// this is a no-op and cleanup relies on the normal paths.
func registerWithGlobalReaper(binary, subcommand, id, creation string) *reaperRegistration {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	if !ok {
		r = newReaper(binary, subcommand)
		globalReapers[binary] = r
	}
	globalReapersMu.Unlock()
	_ = r.register(id, creation)
	return &reaperRegistration{reaper: r, entry: reaperEntry{id: id, creation: creation}}
}

func unregisterWithGlobalReaper(registration *reaperRegistration) {
	if registration == nil || registration.reaper == nil {
		return
	}
	if err := registration.reaper.unregister(registration.entry.id, registration.entry.creation); err != nil {
		log.Printf("container-go: reaper unregister %s: %v", registration.entry.id, err)
	}
}
