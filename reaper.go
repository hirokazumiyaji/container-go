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
	"strconv"
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
// SHELLOPTS may enable monitor mode. Each backend command is launched by a
// short-lived supervisor that owns a fresh process group; monitor mode is
// disabled inside that supervisor before it starts the command and timeout
// helper, so all descendants remain in the owned group.
const reaperScript = `set -f
set +m
bin="$1"
sub="$2"
key="$3"
timeout_seconds="${4:-30}"
case "$timeout_seconds" in
  ''|*[!0-9]*) timeout_seconds=30 ;;
esac
[ "$timeout_seconds" -gt 0 ] 2>/dev/null || timeout_seconds=30
# Never resolve a process-tree helper through PATH: a shadowed pgrep could
# otherwise make the cleanup signal an unrelated PID.
pgrep_bin=
for pgrep_candidate in /usr/bin/pgrep /bin/pgrep; do
  if [ -f "$pgrep_candidate" ] && [ -x "$pgrep_candidate" ]; then
    pgrep_bin=$pgrep_candidate
    break
  fi
done
kill_descendants() {
  local kill_parent="$1" kill_depth="$2" kill_skip="$3" kill_children kill_child
  [ "$kill_depth" -ge 32 ] 2>/dev/null && return 0
  [ -n "$pgrep_bin" ] || return 0
  kill_children=$("$pgrep_bin" -P "$kill_parent" 2>/dev/null) || kill_children=
  for kill_child in $kill_children; do
    case "$kill_child" in
      ''|*[!0-9]*) continue ;;
    esac
    [ "$kill_child" = "$kill_skip" ] && continue
    # Run the recursive frame in a subshell as well as declaring locals;
    # this keeps the outer loop variable intact on headless dash.
    (
      kill_descendants "$kill_child" "$((kill_depth + 1))" "$kill_skip"
      kill -KILL "$kill_child" 2>/dev/null || true
    )
  done
}
kill_pipeline() {
  local pipeline_group="$1" pipeline_root="$2"
  # The supervisor is still alive while this runs, so its process-group ID
  # is owned and cannot be recycled. Visit the command's descendants first
  # to catch a child that created a nested group, then signal the owned
  # supervisor group once.
  #
  # The explicit positive-PID kill is required because set -m does not
  # always create a group for the command: a headless dash supervisor keeps
  # it in the supervisor's own group, so the group signal alone would leave
  # the command running. The supervisor has not reaped command_pid yet
  # (it waits only after the timer is done), so the number is still ours to
  # signal and cannot have been recycled.
  kill_descendants "$pipeline_root" 0 "$pipeline_root"
  kill -KILL "$pipeline_root" 2>/dev/null || true
  kill -KILL -"$pipeline_group" 2>/dev/null || true
}
start_killer() {
  killer_target="$1"
  killer_delay="$2"
  killer_root="$3"
  killer_ready=$(mktemp "${TMPDIR:-/tmp}/containergo-reaper.XXXXXX") || return 1
  (
    killer_sleeper=""
    cleanup_killer() {
      if [ -n "$killer_sleeper" ]; then
        kill -KILL "$killer_sleeper" 2>/dev/null || true
        wait "$killer_sleeper" 2>/dev/null || true
      fi
      killer_sleeper=""
      rm -f "$killer_ready" 2>/dev/null || true
    }
    trap 'cleanup_killer; exit 0' 0 1 2 15
    sleep "$killer_delay" &
    killer_sleeper="$!"
    printf '%s\n' "$killer_sleeper" >"$killer_ready" || exit 1
    wait "$killer_sleeper"
    killer_sleeper=""
    rm -f "$killer_ready" 2>/dev/null || true
    kill_pipeline "$killer_target" "$killer_root"
  ) &
  killer="$!"
  killer_attempts=0
  while [ ! -s "$killer_ready" ]; do
    killer_attempts=$((killer_attempts + 1))
    if ! kill -0 "$killer" 2>/dev/null; then
      wait "$killer" 2>/dev/null || true
      rm -f "$killer_ready"
      return 1
    fi
    if [ "$killer_attempts" -ge 1000 ]; then
      kill "$killer" 2>/dev/null || true
      wait "$killer" 2>/dev/null || true
      rm -f "$killer_ready"
      return 1
    fi
    /bin/sleep 0.001
  done
  if ! IFS= read -r killer_sleeper <"$killer_ready"; then
    kill "$killer" 2>/dev/null || true
    wait "$killer" 2>/dev/null || true
    rm -f "$killer_ready"
    return 1
  fi
  rm -f "$killer_ready"
}
stop_killer() {
  [ -n "${1:-}" ] || return 0
  kill "$1" 2>/dev/null || true
  wait "$1" 2>/dev/null || true
}
run_with_timeout() {
  timeout_seconds="$1"
  shift
  group_file=$(mktemp "${TMPDIR:-/tmp}/containergo-reaper-group.XXXXXX") || return 1
  group_value="$group_file.value"
  trap 'rm -f "$group_file" "$group_value" 2>/dev/null || true' 0 1 2 15
  # With monitor mode enabled only for this launch, the supervisor is a
  # process-group leader. It remains alive while the command is reaped and
  # the timer is canceled, so the PGID cannot be recycled underneath us.
  set -m
  (
    set +m
    trap 'rm -f "$group_file" "$group_value" 2>/dev/null || true' 0 1 2 15
    group_attempts=0
    while [ ! -s "$group_file" ]; do
      group_attempts=$((group_attempts + 1))
      [ "$group_attempts" -lt 2000 ] || exit 1
      /bin/sleep 0.001
    done
    supervisor_group=$(sed -n '1p' "$group_file")
    case "$supervisor_group" in
      ''|*[!0-9]*) exit 1 ;;
    esac
    [ "$supervisor_group" -gt 0 ] 2>/dev/null || exit 1
    SHELLOPTS= "$@" &
    command_pid=$!
    if ! start_killer "$supervisor_group" "$timeout_seconds" "$command_pid"; then
      kill_pipeline "$supervisor_group" "$command_pid"
      wait "$command_pid" 2>/dev/null || true
      exit 1
    fi
    wait "$command_pid" 2>/dev/null
    command_rc=$?
    stop_killer "$killer"
    exit "$command_rc"
  ) &
  supervisor_pid=$!
  printf '%s\n' "$supervisor_pid" >"$group_value" || true
  mv "$group_value" "$group_file" 2>/dev/null || true
  wait "$supervisor_pid" 2>/dev/null
  command_rc=$?
  set +m
  rm -f "$group_file" "$group_value" 2>/dev/null || true
  return "$command_rc"
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
    if ! run_with_timeout "$timeout_seconds" "$bin" inspect "$id" >"$tmp"; then
      rm -f "$tmp"
      continue
    fi
    got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
    uid=$(sed -n 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/\1/p' "$tmp" 2>/dev/null | head -n 1)
    rm -f "$tmp"
    [ "$got" = "$creation" ] || continue
    [ -n "$uid" ] && target="$uid"
  fi
  run_with_timeout "$timeout_seconds" "$bin" "$sub" --force "$target" || true
done
`

const (
	maxReaperSpawnFailures = 3
	// Keep a short completion history for diagnostics without retaining
	// one record for every container created by a long-lived process.
	maxReaperCompletedEntries    = 1024
	maxReaperSpawnRetryLevel     = 32
	maxReaperReconcileRetryLevel = 32
	initialReaperSpawnBackoff    = time.Second
	maxReaperSpawnBackoff        = 30 * time.Second
)

// Reaper writes are deliberately short-lived operations. A reaper reader
// can stop draining its stdin while a backend call is stalled; the parent
// must not turn that condition into a process-wide lock convoy. Waiting for
// the operation gate has its own budget so queued lifecycle work does not
// consume the budget for the actual write or replay.
var (
	reaperWriteTimeout         = 500 * time.Millisecond
	reaperProcessStopTimeout   = time.Second
	reaperOperationLockTimeout = 30 * time.Second
	// Reconciliation must outlive a deliberately shortened public gate
	// timeout used while a lifecycle intent is being recorded.
	reaperReconcileLockTimeout = 30 * time.Second
	reaperOperationTimeout     = 5 * time.Second
	reaperRecoveryTimeout      = 5 * time.Second
)

var (
	errReaperSpawnCooldown = errors.New("reaper: spawn retry cooldown active")
	errReaperSpawnFailed   = errors.New("reaper: giving up after repeated spawn failures")
	errReaperWriteTimeout  = errors.New("reaper: pipe write timed out")
)

type reaperOperationLock struct {
	once sync.Once
	gate chan struct{}
}

func (l *reaperOperationLock) init() {
	l.once.Do(func() { l.gate = make(chan struct{}, 1) })
}

// Lock preserves the package-test helper shape while using the same
// context-aware gate as lifecycle operations.
func (l *reaperOperationLock) Lock() {
	l.init()
	l.gate <- struct{}{}
}

func (l *reaperOperationLock) Unlock() {
	<-l.gate
}

func (l *reaperOperationLock) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.init()
	select {
	case l.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-l.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *reaper) lockOperation(ctx context.Context) error {
	return r.opMu.LockContext(ctx)
}

func (r *reaper) unlockOperation() {
	r.opMu.Unlock()
}

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
// identity is cleared only after Wait reaps the child. In particular, a
// later register or unregister must never derive a process-group signal
// from a PID that the operating system may already have recycled.
type reaperProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}

	// killMu serializes the one group signal with the hand-off to Wait.
	// The direct child is deliberately left waitable while the signal is
	// decided, so a saved PGID cannot be recycled underneath the signal.
	killMu     sync.Mutex
	mu         sync.Mutex
	pid        int
	pgid       int
	groupOwned bool
	reaped     bool
	stop       sync.Once
}

// terminate claims the process-group signal before Wait is allowed to reap
// the direct child. The platform implementation may use the numeric PGID
// only while this process object still owns the unreaped child.
func (p *reaperProcess) terminate() {
	if p == nil {
		return
	}
	p.killMu.Lock()
	defer p.killMu.Unlock()

	p.mu.Lock()
	if p.reaped {
		p.mu.Unlock()
		return
	}
	cmd := p.cmd
	pgid := 0
	if p.groupOwned && cmd != nil && cmd.Process != nil {
		// Setpgid makes the direct child the group leader. Deriving the
		// value from the still-owned handle avoids ever signaling a stale
		// numeric field left on the process object.
		pgid = cmd.Process.Pid
	}
	p.mu.Unlock()

	p.stop.Do(func() {
		killReaperProcess(cmd, pgid)
	})
}

// waitAndMarkReaped is the only path that calls Cmd.Wait. It holds killMu
// across the hand-off so terminate cannot use a PGID after Wait has made the
// numeric identity reusable.
func (p *reaperProcess) waitAndMarkReaped() {
	if p == nil || p.cmd == nil {
		return
	}
	p.killMu.Lock()
	defer p.killMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return
	}
	_ = p.cmd.Wait()
	p.reaped = true
	p.pid = 0
	p.pgid = 0
}

func (p *reaperProcess) markReaped() {
	if p == nil {
		return
	}
	p.killMu.Lock()
	defer p.killMu.Unlock()
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
	// in-memory state. Acquisition is context-aware; pipe writes and child
	// shutdown happen with the gate held but never with mu held.
	opMu reaperOperationLock
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
	// reconcileRetryLevel advances for every failed worker cycle, including
	// gate and stop failures that do not reach spawnProcessLocked.
	reconcileRetryLevel int
	now                 func() time.Time
	command             func() *exec.Cmd
	backoff             func(int) time.Duration
	// timeoutSeconds is an internal test seam; production uses 30 seconds.
	timeoutSeconds int
	killProcess    func(*reaperProcess)

	// stateGeneration advances for every durable register/unregister
	// intent, even when the operation gate cannot yet be acquired.
	stateGeneration  uint64
	reconcilePending bool
	reconcileRunning bool
	reconcileWake    chan struct{}
	reconcileSeen    uint64
	closed           bool
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:         binary,
		subcommand:     subcommand,
		now:            time.Now,
		timeoutSeconds: 30,
		killProcess: func(process *reaperProcess) {
			process.terminate()
		},
	}
}

func (r *reaper) recordRegisterIntent(entry reaperEntry) {
	r.mu.Lock()
	if !r.containsActiveLocked(entry) {
		r.removeCompletedLocked(entry)
		r.entries = append(r.entries, entry)
	}
	r.stateGeneration++
	r.mu.Unlock()
}

func (r *reaper) recordUnregisterIntent(entry reaperEntry) bool {
	r.mu.Lock()
	removed := r.removeActiveLocked(entry)
	if removed {
		r.rememberCompletedLocked(entry)
	}
	r.stateGeneration++
	r.mu.Unlock()
	return removed
}

// requestReconcile makes a failed lifecycle intent durable. The worker owns
// process replacement and replays the current active set, so a completed
// cancellation can never be replayed merely because the gate was busy.
func (r *reaper) requestReconcile() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.reconcilePending = true
	process := r.processLocked()
	if len(r.entries) == 0 && process == nil {
		r.reconcilePending = false
		r.clearReconcileRetryLocked()
		r.mu.Unlock()
		return
	}
	if r.reconcileRunning {
		// A new durable intent should interrupt a backoff wait. A retry
		// requested by the worker itself does not change stateGeneration and
		// therefore does not bypass the cooldown.
		if r.reconcileSeen != r.stateGeneration {
			if r.reconcileWake == nil {
				r.reconcileWake = make(chan struct{}, 1)
			}
			select {
			case r.reconcileWake <- struct{}{}:
			default:
			}
		}
		r.mu.Unlock()
		return
	}
	if r.reconcileWake == nil {
		r.reconcileWake = make(chan struct{}, 1)
	}
	r.reconcileRunning = true
	r.reconcileSeen = r.stateGeneration
	r.mu.Unlock()
	go r.reconcileLoop()
}

func (r *reaper) clearReconcileRequestLocked() {
	r.reconcilePending = false
	if r.reconcileRunning && r.reconcileWake != nil {
		select {
		case r.reconcileWake <- struct{}{}:
		default:
		}
	}
}

func (r *reaper) reconcileLoop() {
	// Keep retrying after a failed replacement. Active entries are the
	// watchdog's source of truth, so a transient backend or gate failure
	// must not permanently discard the pending reconciliation.
	for {
		r.mu.Lock()
		if r.closed {
			r.reconcilePending = false
			r.reconcileRunning = false
			r.clearReconcileRetryLocked()
			r.mu.Unlock()
			return
		}
		observed := r.stateGeneration
		r.reconcileSeen = observed
		wake := r.reconcileWake
		r.mu.Unlock()

		err := r.reconcileOnce()
		r.mu.Lock()
		if r.closed {
			r.reconcilePending = false
			r.reconcileRunning = false
			r.clearReconcileRetryLocked()
			r.mu.Unlock()
			return
		}
		if err != nil {
			r.recordReconcileFailureLocked()
		}
		if r.stateGeneration != observed {
			r.reconcileSeen = r.stateGeneration
			r.mu.Unlock()
			continue
		}
		if err == nil {
			r.reconcilePending = false
			r.reconcileRunning = false
			r.clearReconcileRetryLocked()
			r.mu.Unlock()
			return
		}

		delay := r.reconcileRetryDelayLocked()
		r.mu.Unlock()
		timer := time.NewTimer(delay)
		if wake == nil {
			<-timer.C
		} else {
			select {
			case <-timer.C:
			case <-wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
		}
	}
}

func (r *reaper) reconcileOnce() error {
	lockCtx, lockCancel := context.WithTimeout(context.Background(), reaperReconcileLockTimeout)
	defer lockCancel()
	if err := r.lockOperation(lockCtx); err != nil {
		return err
	}
	defer r.unlockOperation()
	r.mu.Lock()
	pending := r.reconcilePending
	r.mu.Unlock()
	if !pending {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
	defer cancel()
	return r.respawnAndReplay(ctx)
}

func (r *reaper) recordReconcileFailureLocked() {
	if r.reconcileRetryLevel < maxReaperReconcileRetryLevel {
		r.reconcileRetryLevel++
	}
}

func (r *reaper) clearReconcileRetryLocked() {
	r.reconcileRetryLevel = 0
}

func (r *reaper) reconcileRetryDelayLocked() time.Duration {
	delay := time.Duration(0)
	if !r.retryAt.IsZero() {
		delay = r.retryAt.Sub(r.nowLocked())
	}
	if delay <= 0 && r.reconcileRetryLevel > 0 {
		delay = r.backoffLocked(r.reconcileRetryLevel)
	}
	if delay <= 0 && r.retryLevel > 0 {
		delay = r.backoffLocked(r.retryLevel)
	}
	if delay <= 0 {
		delay = initialReaperSpawnBackoff
	}
	if delay > maxReaperSpawnBackoff {
		return maxReaperSpawnBackoff
	}
	return delay
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

	// Record the active entry before waiting for the operation gate. If the
	// gate is busy past its budget, the registration is still part of the
	// durable replay set and requestReconcile can replace the child later.
	entry := reaperEntry{id: id, creation: creation}
	r.recordRegisterIntent(entry)
	lockCtx, lockCancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
	defer lockCancel()
	if err := r.lockOperation(lockCtx); err != nil {
		r.requestReconcile()
		return err
	}
	defer r.unlockOperation()
	ctx, cancel := context.WithTimeout(context.Background(), reaperOperationTimeout)
	defer cancel()
	return r.registerContextReady(ctx, entry)
}

func (r *reaper) registerContext(ctx context.Context, entry reaperEntry) error {
	r.recordRegisterIntent(entry)
	return r.registerContextReady(ctx, entry)
}

func (r *reaper) registerContextReady(ctx context.Context, entry reaperEntry) error {
	r.mu.Lock()
	// A later unregister may have superseded this registration while it
	// waited for the operation gate. Let that lifecycle transition own the
	// pipe record instead of reintroducing a completed entry.
	if !r.containsActiveLocked(entry) {
		r.mu.Unlock()
		return nil
	}
	if !r.retryReadyLocked() {
		err := r.spawnCooldownErrorLocked()
		r.mu.Unlock()
		r.requestReconcile()
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
		// Alias-only observations do not carry an ownership proof for a
		// numeric group signal; the real process object is created by spawn.
		groupOwned: false,
		reaped:     channelClosed(r.exited),
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
	ctx, cancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
	defer cancel()
	return r.unregisterWithContext(ctx, id, creation)
}

// unregisterWithContext records the completion before acquiring the gate,
// but uses the caller's context for the gate itself. A canceled Terminate
// therefore returns without opening a fresh 30-second background wait; the
// durable intent is handed to the reconciliation worker instead.
func (r *reaper) unregisterWithContext(ctx context.Context, id, creation string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !nameRE.MatchString(id) && !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}

	// Complete the in-memory lifecycle transition before waiting for the
	// operation gate. The active set and completion history therefore stay
	// safe even when the child cannot be updated before the timeout.
	entry := reaperEntry{id: id, creation: creation}
	removed := r.recordUnregisterIntent(entry)
	if !removed {
		return nil
	}
	gateCtx, gateCancel := reaperGateContext(ctx)
	defer gateCancel()
	if err := r.lockOperation(gateCtx); err != nil {
		r.requestReconcile()
		return err
	}
	defer r.unlockOperation()
	if err := ctx.Err(); err != nil {
		r.requestReconcile()
		return err
	}
	opCtx, opCancel := context.WithTimeout(ctx, reaperOperationTimeout)
	defer opCancel()
	return r.unregisterContextReady(opCtx, entry)
}

func reaperGateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	// WithTimeout also preserves an earlier caller deadline, while the
	// package bound prevents a background Terminate from waiting forever.
	return context.WithTimeout(ctx, reaperOperationLockTimeout)
}

func (r *reaper) unregisterContext(ctx context.Context, entry reaperEntry) error {
	if !r.recordUnregisterIntent(entry) {
		return nil
	}
	return r.unregisterContextReady(ctx, entry)
}

func (r *reaper) unregisterContextReady(ctx context.Context, entry reaperEntry) error {
	r.mu.Lock()
	// A later registration may have superseded this cancellation while it
	// waited for the operation gate. Its + record must be allowed to win.
	if r.containsActiveLocked(entry) {
		r.mu.Unlock()
		return nil
	}
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
	if err := ctx.Err(); err != nil {
		// The caller has already paid its cancellation cost. Do not enter
		// a fresh synchronous recovery budget; the worker will stop the old
		// child and replay the durable active set.
		r.requestReconcile()
		return err
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

// recoverAndReplay stops the failed process before doing any replay work.
// The process stays published until the shutdown barrier completes, so a
// later recovery cannot overlap an old process tree or lose the only handle
// to it. Recovery gets a fresh budget: a caller cancellation (including
// expiry while waiting for the operation gate) must not leave active entries
// without a watchdog.
func (r *reaper) recoverAndReplay(_ context.Context, process *reaperProcess) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
	defer cancel()
	defer func() {
		if err != nil {
			r.requestReconcile()
		}
	}()
	if process == nil {
		r.mu.Lock()
		process = r.process
		r.mu.Unlock()
	}
	if process != nil {
		if stopErr := r.stopProcess(process, ctx); stopErr != nil {
			return stopErr
		}
		r.detachProcess(process)
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
			process.terminate()
		}
	}
	if process.stdin != nil {
		_ = process.stdin.Close()
	}
	if process.cmd == nil || process.cmd.Process == nil || process.exited == nil {
		return nil
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), reaperProcessStopTimeout)
		defer cancel()
	} else if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, reaperProcessStopTimeout)
		defer cancel()
	}
	select {
	case <-process.exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("reaper: child shutdown: %w", ctx.Err())
	}
}

func (r *reaper) respawnAndReplay(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			r.requestReconcile()
		}
	}()
	// A recovery path may already have stopped the failed child. This also
	// covers callers that enter replay after a child exited by itself.
	r.mu.Lock()
	if r.closed {
		r.reconcilePending = false
		r.mu.Unlock()
		return nil
	}
	current := r.process
	r.mu.Unlock()
	if current != nil {
		if stopErr := r.stopProcess(current, ctx); stopErr != nil {
			return stopErr
		}
		r.detachProcess(current)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if len(r.entries) == 0 {
		r.clearSpawnFailureLocked()
		r.clearReconcileRequestLocked()
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
				r.clearReconcileRequestLocked()
			}
			r.mu.Unlock()
			return nil
		}
		r.mu.Lock()
		current := r.process
		r.mu.Unlock()
		if current == process {
			if stopErr := r.stopProcess(process, ctx); stopErr != nil {
				lastErr = stopErr
				break
			}
			r.detachProcess(process)
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
		timeout := r.timeoutSeconds
		if timeout <= 0 {
			timeout = 30
		}
		cmd = exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel), strconv.Itoa(timeout))
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
	pgid := reaperProcessGroupID(cmd)
	process := &reaperProcess{
		cmd:        cmd,
		stdin:      stdin,
		exited:     make(chan struct{}),
		pid:        cmd.Process.Pid,
		pgid:       pgid,
		groupOwned: pgid > 0,
	}
	r.process = process
	r.cmd, r.stdin, r.exited = cmd, stdin, process.exited
	r.pid, r.pgid = process.pid, process.pgid
	go r.waitProcess(process)
	return process, nil
}

func (r *reaper) waitProcess(process *reaperProcess) {
	if process == nil {
		return
	}
	if process.cmd != nil && process.cmd.Process != nil {
		// Wait4(WNOWAIT) leaves the direct child waitable. Only after that
		// observation do we decide whether the process group needs a
		// signal, and Cmd.Wait is not called until the group is gone.
		_ = waitForReaperTermination(process.cmd)
		process.terminate()
		waitForReaperProcessGroupExit(process.pgid)
		process.waitAndMarkReaped()
	} else {
		process.markReaped()
	}
	r.mu.Lock()
	if r.process == process {
		// Retain only the completion channel for waiters. Clearing the
		// command and pipe aliases prevents a delayed lifecycle call from
		// treating a reaped child as an active writer.
		r.cmd = nil
		r.stdin = nil
		r.pid = 0
		r.pgid = 0
	}
	r.mu.Unlock()
	close(process.exited)
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
	if r.retryLevel < maxReaperSpawnRetryLevel {
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
	r.clearReconcileRetryLocked()
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

// closeStdin hands the reaper the same EOF it would see on parent death and
// stops any pending reconciliation from replacing it. Test hook and
// best-effort shutdown.
func (r *reaper) closeStdin() {
	lockCtx, lockCancel := context.WithTimeout(context.Background(), reaperProcessStopTimeout)
	defer lockCancel()
	if err := r.lockOperation(lockCtx); err == nil {
		defer r.unlockOperation()
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.reconcilePending = false
	if r.reconcileWake != nil {
		select {
		case r.reconcileWake <- struct{}{}:
		default:
		}
	}
	stdin := r.stdin
	r.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

// killForTest kills the reaper process group and waits until the child
// is reaped, so the next write deterministically fails. It uses the same
// stop-before-detach path as production recovery.
func (r *reaper) killForTest() {
	_ = r.lockOperation(context.Background())
	defer r.unlockOperation()
	r.mu.Lock()
	process := r.process
	r.mu.Unlock()
	if process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reaperProcessStopTimeout)
	defer cancel()
	if r.stopProcess(process, ctx) == nil {
		r.detachProcess(process)
	}
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

func unregisterWithGlobalReaper(registration *reaperRegistration, callers ...context.Context) {
	if registration == nil || registration.reaper == nil {
		return
	}
	ctx := context.Background()
	if len(callers) > 0 && callers[0] != nil {
		ctx = callers[0]
	}
	if err := registration.reaper.unregisterWithContext(ctx, registration.entry.id, registration.entry.creation); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("container-go: reaper unregister %s: %v", registration.entry.id, err)
	}
}
