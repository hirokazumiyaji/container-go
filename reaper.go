package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
// If the child exits unexpectedly while the parent is alive, the parent
// starts a replacement and replays every entry into it. Replacements first
// wait for the old process group's descendants to be terminated and reaped.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated as an Apple Container name or a full Docker ID, and
// the script itself disables globbing and quotes every expansion the
// IDs reach.
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
const reaperScript = `set -f
set +m
bin="$1"
sub="$2"
key="$3"
timeout="${4:-30}"
pending_attempts="${5:-30}"
awk_bin="$6"
sleep_bin="$7"
lockf_bin="$8"
lock_dir="$9"
rm_bin="${10:-}"
run_prefix="${11:-}"
mode="${12:-main}"
if [ "${12:-}" = locked ]; then
  ps_bin="${16:-}"
  tr_bin="${17:-}"
  status_dir="${18:-}"
  lock_kind="${19:-lockf}"
  pgrep_bin="${20:-}"
  setsid_bin="${21:-}"
else
  ps_bin="${13:-}"
  tr_bin="${14:-}"
  status_dir="${15:-}"
  lock_kind="${16:-lockf}"
  pgrep_bin="${17:-}"
  setsid_bin="${18:-}"
fi
inspect_marker=__containergo_reaper_inspect_rc__
case "$timeout" in ''|*[!0-9]*) timeout=30;; esac
case "$pending_attempts" in ''|*[!0-9]*) pending_attempts=30;; esac
[ "$timeout" -gt 0 ] 2>/dev/null || timeout=30
[ "$pending_attempts" -gt 0 ] 2>/dev/null || pending_attempts=1
[ -n "$awk_bin" ] && [ -x "$awk_bin" ] || exit 0
[ -n "$sleep_bin" ] && [ -x "$sleep_bin" ] || exit 0

# The Go parent holds fd 3 open. This sentinel stays in the reaper's
# session/group after the shell exits, so a post-Wait group signal cannot
# race PGID reuse. It is started before any backend helper can detach.
(
  4>&-
  IFS= read -r _ <&3
) &
sentinel_pid=$!
printf '%s\n' "$sentinel_pid" >&4

trim_leading_space() {
  trimmed="$1"
  while :; do
    case "$trimmed" in
      [[:space:]]*) trimmed=${trimmed#?};;
      *) break;;
    esac
  done
}

field_value() {
  trim_leading_space "$1"
  expected="\"$2\""
  case "$trimmed" in
    "$expected"*) field_rest=${trimmed#"$expected"};;
    *) return 1;;
  esac
  trim_leading_space "$field_rest"
  case "$trimmed" in
    :*) field_rest=${trimmed#?};;
    *) return 1;;
  esac
  trim_leading_space "$field_rest"
  case "$trimmed" in
    \"*) field_rest=${trimmed#?};;
    *) return 1;;
  esac
  field_result=${field_rest%%\"*}
  case "$field_result" in
    ''|*[!0-9a-f]*) return 1;;
  esac
  [ "${#field_result}" -eq "$3" ] || return 1
}

run_entry() {
  3>&- 4>&-
  set +m
  "$@"
  entry_rc=$?
  printf '%s\n' "$entry_rc" >"$entry_status_path"
  # Keep the entry process group alive until the caller has observed the
  # status and signaled the group. This makes a negative group signal safe
  # even when the backend itself has already exited and been reparented.
  while :; do
    "$sleep_bin" 3600
  done
}

# Some /bin/sh implementations cannot create a job process group without a
# tty. Use a trusted setsid helper when available; the wrapper publishes its
# own PID so an implementation that forks is still addressable.
entry_script='
entry_status_path=$1
entry_pid_path=$2
entry_sleep_bin=$3
shift 3
printf "%s\n" "$$" >"$entry_pid_path" || exit 1
"$@"
entry_rc=$?
printf "%s\n" "$entry_rc" >"$entry_status_path"
while :; do
  "$entry_sleep_bin" 3600
done
'

kill_descendants() {
  descendant_children=
  if [ -n "$pgrep_bin" ] && [ -x "$pgrep_bin" ]; then
    descendant_children=$("$pgrep_bin" -P "$1" 2>/dev/null) || descendant_children=
  fi
  for descendant_pid in $descendant_children; do
    kill_descendants "$descendant_pid"
    kill -KILL "$descendant_pid" 2>/dev/null || true
  done
}

kill_entry_target() {
  target_pgid=
  if [ -n "$ps_bin" ] && [ -x "$ps_bin" ] && [ -n "$tr_bin" ] && [ -x "$tr_bin" ]; then
    target_pgid=$("$ps_bin" -o pgid= -p "$timeout_command_pid" 2>/dev/null | "$tr_bin" -d '[:space:]') || target_pgid=
  fi
  case "$target_pgid" in
    ''|*[!0-9]*) target_pgid=;;
  esac
  # set -m cannot create a job process group on every /bin/sh (dash
  # reports "can't access tty"). Only signal a negative PGID that the
  # target actually owns; otherwise the reaper's own group would be killed.
  if [ -n "$target_pgid" ] && [ "$target_pgid" = "$timeout_command_pid" ] && kill -KILL -"$target_pgid" 2>/dev/null; then
    return
  fi
  kill_descendants "$timeout_command_pid"
  kill -KILL "$timeout_command_pid" 2>/dev/null || true
}

run_with_timeout() (
  3>&- 4>&-
  timeout_seconds="$1"
  entry_label="$2"
  shift 2
  entry_status_path="$status_dir/$run_prefix-$entry_label"
  entry_pid_path="$status_dir/$run_prefix-$entry_label.pid"
  : >"$entry_status_path" 2>/dev/null || exit 1
  : >"$entry_pid_path" 2>/dev/null || exit 1
  if [ "$entry_label" = delete ] && [ -n "$setsid_bin" ] && [ -x "$setsid_bin" ]; then
    "$setsid_bin" /bin/sh -c "$entry_script" containergo-entry \
      "$entry_status_path" "$entry_pid_path" "$sleep_bin" "$@" 3>&- &
    launched_pid=$!
    pid_tries=0
    while [ ! -s "$entry_pid_path" ] && [ "$pid_tries" -lt 20 ]; do
      "$sleep_bin" 0.01
      pid_tries=$((pid_tries + 1))
    done
    entry_pid=
    IFS= read -r entry_pid <"$entry_pid_path" 2>/dev/null || entry_pid=
    case "$entry_pid" in
      ''|*[!0-9]*) timeout_command_pid=$launched_pid;;
      *) timeout_command_pid=$entry_pid;;
    esac
  else
    set -m
    entry_status_path="$entry_status_path"
    run_entry "$@" 3>&- &
    timeout_command_pid=$!
  fi
  (
    timeout_sleep_pid=
    cleanup_timeout_killer() {
      if [ -n "$timeout_sleep_pid" ]; then
        kill -KILL "$timeout_sleep_pid" 2>/dev/null || true
        wait "$timeout_sleep_pid" 2>/dev/null || true
      fi
    }
    trap 'cleanup_timeout_killer; exit 0' 0 1 2 15
    "$sleep_bin" "$timeout_seconds" 3>&- 4>&- &
    timeout_sleep_pid=$!
    wait "$timeout_sleep_pid" || true
    timeout_sleep_pid=
    kill_entry_target
  ) &
  timeout_killer_pid=$!
  entry_rc=
  ticks=0
  max_ticks=$((timeout_seconds * 20))
  while :; do
    if IFS= read -r entry_rc <"$entry_status_path" 2>/dev/null; then
      break
    fi
    if [ "$ticks" -ge "$max_ticks" ]; then
      entry_rc=124
      break
    fi
    "$sleep_bin" 0.05
    ticks=$((ticks + 1))
  done
  kill "$timeout_killer_pid" 2>/dev/null || true
  wait "$timeout_killer_pid" 2>/dev/null || true
  kill_entry_target
  wait "$timeout_command_pid" 2>/dev/null || true
  if [ -n "$rm_bin" ] && [ -x "$rm_bin" ]; then
    "$rm_bin" -f "$entry_status_path" "$entry_pid_path" 2>/dev/null || true
  fi
  exit "$entry_rc"
)

inspect_projection() {
  inspect_id="$1"
  {
    "$bin" inspect "$inspect_id" 2>/dev/null
    inspect_rc=$?
    printf '\n%s%s\n' "$inspect_marker" "$inspect_rc"
  } | while IFS= read -r inspect_line || [ -n "$inspect_line" ]; do
    trim_leading_space "$inspect_line"
    case "$trimmed" in
      "$inspect_marker"*)
        inspect_status=${trimmed#"$inspect_marker"}
        case "$inspect_status" in
          ''|*[!0-9]*) ;;
          *) printf 'rc=%s\n' "$inspect_status";;
        esac
        continue
        ;;
    esac
    if field_value "$trimmed" "$key" 16; then
      printf 'generation=%s\n' "$field_result"
    elif field_value "$trimmed" Id 64; then
      printf 'id=%s\n' "$field_result"
    fi
  done
}

process_entry() {
  entry_id="$1"
  entry_creation="$2"
  entry_pending="$3"
  target="$entry_id"
  if [ -n "$entry_creation" ]; then
    tries=0
    while :; do
      inspect_fields=$(run_with_timeout 10 inspect inspect_projection "$entry_id") || inspect_fields=
      got=
      uid=
      inspect_rc=
      for field in $inspect_fields; do
        case "$field" in
          generation=*) got=${field#generation=};;
          id=*) uid=${field#id=};;
          rc=*) inspect_rc=${field#rc=};;
        esac
      done
      if [ "$inspect_rc" = 0 ]; then
        if [ "$got" != "$entry_creation" ]; then
          [ "$entry_pending" = 1 ] && [ -z "$got" ] || return 0
        else
          [ -n "$uid" ] && target="$uid"
          break
        fi
      fi
      [ "$entry_pending" = 1 ] || return 0
      tries=$((tries + 1))
      [ "$tries" -lt "$pending_attempts" ] || return 0
      "$sleep_bin" 1
    done
  fi
  run_with_timeout "$timeout" delete "$bin" "$sub" --force "$target" || true
}

run_locked() {
  locked_id="$1"
  locked_creation="$2"
  locked_state="$3"
  [ "$sub" = delete ] || return 0
  [ -n "$lockf_bin" ] && [ -x "$lockf_bin" ] || return 0
  [ -n "$lock_dir" ] || return 0
  locked_path="$lock_dir/containergo-$locked_id.lock"
  [ -L "$locked_path" ] && return 0
  [ -f "$locked_path" ] || return 0
  [ -n "${CONTAINERGO_REAPER_SCRIPT:-}" ] || return 0
  if [ "$lock_kind" = flock ]; then
    "$lockf_bin" -x -w 30 "$locked_path" /bin/sh -c "$CONTAINERGO_REAPER_SCRIPT" \
      containergo-reaper-locked "$bin" "$sub" "$key" "$timeout" "$pending_attempts" \
      "$awk_bin" "$sleep_bin" "$lockf_bin" "$lock_dir" "$rm_bin" "$run_prefix" locked "$locked_id" "$locked_creation" "$locked_state" "$ps_bin" "$tr_bin" "$status_dir" "$lock_kind" "$pgrep_bin" "$setsid_bin" \
      >/dev/null 2>&1 || true
  else
    "$lockf_bin" -k -w -t 30 "$locked_path" /bin/sh -c "$CONTAINERGO_REAPER_SCRIPT" \
      containergo-reaper-locked "$bin" "$sub" "$key" "$timeout" "$pending_attempts" \
      "$awk_bin" "$sleep_bin" "$lockf_bin" "$lock_dir" "$rm_bin" "$run_prefix" locked "$locked_id" "$locked_creation" "$locked_state" "$ps_bin" "$tr_bin" "$status_dir" "$lock_kind" "$pgrep_bin" "$setsid_bin" \
      >/dev/null 2>&1 || true
  fi
}

if [ "$mode" = locked ]; then
  locked_pending=0
  [ "${15:-}" = P ] && locked_pending=1
  process_entry "${13:-}" "${14:-}" "$locked_pending"
  exit 0
fi

"$awk_bin" 3>&- 4>&- '
function key(id, gen) { return id SUBSEP gen }
$1 == "P" { active[key($2, $3)] = "P"; next }
$1 == "C" { k = key($2, $3); if (k in active) active[k] = "A"; next }
$1 == "+" { active[key($2, $3)] = "A"; next }
$1 == "-" { delete active[key($2, $3)]; next }
NF >= 1 && $1 !~ /^[+PC-]$/ { active[key($1, $2)] = "A"; next }
END {
  for (k in active) {
    split(k, fields, SUBSEP)
    print active[k], fields[1], fields[2]
  }
}
' | while IFS=' ' read -r state id creation; do
  [ -n "$id" ] || continue
  if [ "$sub" = delete ] && [ -n "$creation" ]; then
    run_with_timeout "$timeout" locked run_locked "$id" "$creation" "$state" >/dev/null 2>&1 || true
  else
    pending=0
    [ "$state" = P ] && pending=1
    run_with_timeout "$timeout" entry process_entry "$id" "$creation" "$pending" >/dev/null 2>&1 || true
  fi
done
`

const (
	maxReaperSpawnFailures       = 3
	defaultReaperTimeoutSeconds  = 30
	defaultReaperPendingAttempts = 30
)

var reaperInstanceCounter atomic.Uint64

var (
	reaperWriteTimeout         = 500 * time.Millisecond
	reaperOperationLockTimeout = 30 * time.Second
	reaperReconcileLockTimeout = 30 * time.Second
	reaperOperationTimeout     = 5 * time.Second
	reaperRecoveryTimeout      = 5 * time.Second
	initialReaperSpawnBackoff  = time.Second
	maxReaperSpawnBackoff      = 30 * time.Second
)

var (
	errReaperWriteTimeout = errors.New("reaper: pipe write timed out")
	errReaperClosed       = errors.New("reaper: closed")
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
	// pending is set between pre-registration and the backend create
	// completing. A pending entry rechecks a missing container for a
	// bounded settling window after parent EOF.
	pending bool
}

type reaperProcess struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	exited    chan struct{}
	pgid      int
	group     *reaperGroupOwner
	statusDir string

	stateMu sync.Mutex
	reaped  bool
	// stopOnce makes the process-group signal happen exactly once, before
	// either the recovery path or the wait path calls cmd.Wait. stateMu
	// closes the small hand-off window between Wait returning and marking
	// the identity reaped, so a saved group ID can never be signaled then.
	stopOnce sync.Once
}

func (p *reaperProcess) terminate() {
	if p == nil {
		return
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.reaped {
		return
	}
	p.stopOnce.Do(func() {
		terminateReaperProcess(p)
	})
}

func (p *reaperProcess) waitAndMarkReaped() {
	if p == nil {
		return
	}
	p.stateMu.Lock()
	cmd := p.cmd
	p.stateMu.Unlock()
	_ = cmd.Wait()
	p.stateMu.Lock()
	p.reaped = true
	p.stateMu.Unlock()
}

func closeReaperFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}

func removeReaperStatusDir(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

type reaperOperationLock struct {
	once sync.Once
	gate chan struct{}
}

func (l *reaperOperationLock) init() {
	l.once.Do(func() { l.gate = make(chan struct{}, 1) })
}

// Lock preserves the package-test helper shape; lifecycle operations use
// LockContext so a blocked lifecycle transition cannot wait forever.
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

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	// opMu serializes registration, completion, close, and respawn
	// transitions. r.mu protects the fields below; no process wait is
	// performed while r.mu is held.
	opMu                 reaperOperationLock
	mu                   sync.Mutex
	cmd                  *exec.Cmd
	stdin                io.WriteCloser
	exited               chan struct{}
	pgid                 int
	process              *reaperProcess
	closed               bool
	entries              []reaperEntry
	spawnFailures        int
	gaveUp               bool
	stateGeneration      uint64
	reconcilePending     bool
	reconcileRunning     bool
	retryAt              time.Time
	retryLevel           int
	watchers             sync.WaitGroup
	operationLockTimeout time.Duration

	command func() *exec.Cmd

	// timeoutSeconds and pendingAttempts are internal test seams.
	// Production reapers use the bounded defaults; a create that has not
	// appeared yet gets a final inspect window after parent EOF.
	timeoutSeconds  int
	pendingAttempts int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:               binary,
		subcommand:           subcommand,
		timeoutSeconds:       defaultReaperTimeoutSeconds,
		pendingAttempts:      defaultReaperPendingAttempts,
		operationLockTimeout: reaperOperationLockTimeout,
	}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. creation is the generation
// ID from creationLabel; empty is reserved for immutable container IDs
// that do not need a generation check.
func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

// registerPending records a name/generation before the backend create
// starts. If the parent dies while that child is still creating, the
// reaper gets a bounded settling window to observe the eventual result.
func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

func (r *reaper) recordRegisterIntent(entry reaperEntry) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	duplicate := false
	for _, active := range r.entries {
		if active == entry {
			duplicate = true
			break
		}
	}
	if !duplicate {
		r.entries = append(r.entries, entry)
	}
	r.stateGeneration++
	r.mu.Unlock()
}

func (r *reaper) recordCompletionIntent(id, creation string) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	for i, entry := range r.entries {
		if entry.pending && entry.id == id && entry.creation == creation {
			r.entries[i].pending = false
			r.stateGeneration++
			break
		}
	}
	r.mu.Unlock()
}

func (r *reaper) requestReconcile() {
	r.mu.Lock()
	if r.closed {
		r.reconcilePending = false
		r.mu.Unlock()
		return
	}
	r.reconcilePending = true
	if r.reconcileRunning || len(r.entries) == 0 {
		r.mu.Unlock()
		return
	}
	r.reconcileRunning = true
	r.mu.Unlock()
	go r.reconcileLoop()
}

func (r *reaper) reconcileLoop() {
	for {
		r.mu.Lock()
		generation := r.stateGeneration
		r.mu.Unlock()
		err := r.reconcileOnce()
		r.mu.Lock()
		if r.closed || !r.reconcilePending || len(r.entries) == 0 {
			r.reconcilePending = false
			r.reconcileRunning = false
			r.mu.Unlock()
			return
		}
		if err == nil && r.stateGeneration == generation {
			r.reconcilePending = false
			r.reconcileRunning = false
			r.mu.Unlock()
			return
		}
		delay := r.reconcileRetryDelayLocked()
		if err == nil {
			delay = 0
		}
		r.mu.Unlock()
		if delay == 0 {
			continue
		}
		timer := time.NewTimer(delay)
		<-timer.C
	}
}

func (r *reaper) reconcileOnce() error {
	lockCtx, cancel := context.WithTimeout(context.Background(), reaperReconcileLockTimeout)
	defer cancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		return err
	}
	defer r.opMu.Unlock()
	ctx, cancelRecovery := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
	defer cancelRecovery()
	return r.respawnAndReplayContext(ctx)
}

func (r *reaper) reconcileRetryDelayLocked() time.Duration {
	if !r.retryAt.IsZero() {
		if delay := time.Until(r.retryAt); delay > 0 {
			return delay
		}
	}
	if r.retryLevel > 0 {
		if delay := r.backoffLocked(r.retryLevel); delay > 0 {
			return delay
		}
	}
	return initialReaperSpawnBackoff
}

func (r *reaper) backoffLocked(level int) time.Duration {
	if level < 1 {
		level = 1
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

func (r *reaper) registerEntry(entry reaperEntry) error {
	if err := validateReaperEntry(entry); err != nil {
		return err
	}
	if entry.pending && entry.creation == "" {
		return errors.New("reaper: pending entry requires a creation id")
	}
	if entry.creation != "" && r.subcommand == "delete" {
		if err := ensureReaperNameLock(entry.id); err != nil {
			return fmt.Errorf("reaper: prepare name lock: %w", err)
		}
	}

	// Record the durable intent before waiting for the lifecycle gate. A
	// timeout must not allow a backend create to start unregistered.
	r.recordRegisterIntent(entry)
	lockCtx, lockCancel := context.WithTimeout(context.Background(), r.operationLockTimeout)
	defer lockCancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		r.requestReconcile()
		return err
	}
	defer r.opMu.Unlock()
	opCtx, opCancel := context.WithTimeout(context.Background(), reaperOperationTimeout)
	defer opCancel()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errReaperClosed
	}
	process := r.process
	failures := r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && processLive(process) {
		if err := writeReaperEntry(opCtx, process.stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	if err := r.recoverAndReplay(opCtx, process); err != nil {
		r.requestReconcile()
		return err
	}
	return nil
}

func validateReaperEntry(entry reaperEntry) error {
	if !nameRE.MatchString(entry.id) && !dockerIDRE.MatchString(entry.id) {
		return fmt.Errorf("reaper: invalid container id %q", entry.id)
	}
	if entry.creation != "" && !creationRE.MatchString(entry.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", entry.creation)
	}
	return nil
}

func writeReaperEntry(ctx context.Context, stdin io.Writer, entry reaperEntry) error {
	prefix := "+"
	if entry.pending {
		prefix = "P"
	}
	line := prefix + " " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
	}
	return writeReaperRecord(ctx, stdin, line+"\n")
}

func (r *reaper) writeCurrentEntry(ctx context.Context, entry reaperEntry) error {
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	return writeReaperEntry(ctx, stdin, entry)
}

func writeReaperCompletion(ctx context.Context, stdin io.Writer, entry reaperEntry) error {
	line := "C " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
	}
	return writeReaperRecord(ctx, stdin, line+"\n")
}

func writeReaperRecord(ctx context.Context, stdin io.Writer, line string) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writeCtx, cancel := context.WithTimeout(ctx, reaperWriteTimeout)
	defer cancel()
	if err := writeCtx.Err(); err != nil {
		return err
	}

	// StdinPipe returns an *os.File whose write deadline interrupts a
	// blocked pipe write. Keep a goroutine fallback for test doubles and
	// writers without deadline support.
	if deadlineWriter, ok := stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		deadline := time.Now().Add(reaperWriteTimeout)
		if ctxDeadline, ok := writeCtx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		_ = deadlineWriter.SetWriteDeadline(deadline)
		defer func() { _ = deadlineWriter.SetWriteDeadline(time.Time{}) }()
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := io.WriteString(stdin, line)
		done <- result{n: n, err: err}
	}()
	select {
	case result := <-done:
		if result.err == nil && result.n != len(line) {
			return io.ErrShortWrite
		}
		if result.err != nil {
			if writeCtx.Err() != nil {
				return fmt.Errorf("%w: %w", errReaperWriteTimeout, writeCtx.Err())
			}
			return result.err
		}
		return nil
	case <-writeCtx.Done():
		return fmt.Errorf("%w: %w", errReaperWriteTimeout, writeCtx.Err())
	}
}

// completePending marks a pre-registered generation as having completed
// the backend create. The entry remains in the replay set: if the parent
// dies later, EOF still asks the reaper to delete that exact generation.
func (r *reaper) completePending(id, creation string) error {
	entry := reaperEntry{id: id, creation: creation, pending: true}
	if err := validateReaperEntry(entry); err != nil {
		return err
	}

	r.recordCompletionIntent(id, creation)
	lockCtx, lockCancel := context.WithTimeout(context.Background(), r.operationLockTimeout)
	defer lockCancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		r.requestReconcile()
		return err
	}
	defer r.opMu.Unlock()
	opCtx, opCancel := context.WithTimeout(context.Background(), reaperOperationTimeout)
	defer opCancel()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	completed := false
	for _, active := range r.entries {
		if active.id == id && active.creation == creation {
			completed = true
			break
		}
	}
	if !completed {
		r.mu.Unlock()
		return nil
	}
	entry.pending = false
	process := r.process
	failures := r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && processLive(process) {
		if err := writeReaperCompletion(opCtx, process.stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	if err := r.recoverAndReplay(opCtx, process); err != nil {
		r.requestReconcile()
		return err
	}
	return nil
}

func (r *reaper) clearSpawnFailure() {
	r.mu.Lock()
	r.spawnFailures = 0
	r.gaveUp = false
	r.retryAt = time.Time{}
	r.retryLevel = 0
	r.mu.Unlock()
}

func (r *reaper) respawnAndReplayContext(ctx context.Context) error {
	r.mu.Lock()
	current := r.process
	r.mu.Unlock()
	if current != nil {
		if err := r.stopProcessContext(ctx, current); err != nil {
			// Keep the process published when the barrier is not complete.
			// A later recovery must wait for this retiring child instead of
			// starting a replacement beside it.
			return err
		}
		r.detachProcess(current)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errReaperClosed
	}
	if len(r.entries) == 0 {
		r.spawnFailures = 0
		r.gaveUp = false
		r.mu.Unlock()
		return nil
	}
	// A completed retry cycle does not permanently discard retained
	// entries. A later registration gets a fresh set of attempts.
	if r.spawnFailures >= maxReaperSpawnFailures {
		r.spawnFailures = 0
	}
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()

	for r.spawnFailuresValue() < maxReaperSpawnFailures {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _, _, err := r.spawnProcess()
		if err != nil {
			r.recordSpawnFailure()
			continue
		}
		replayed := true
		for _, entry := range entries {
			if err := r.writeCurrentEntry(ctx, entry); err != nil {
				replayed = false
				break
			}
		}
		if replayed {
			r.clearSpawnFailure()
			return nil
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
		stopErr := r.stopCurrentProcessContext(cleanupCtx)
		cleanupCancel()
		if stopErr != nil {
			return stopErr
		}
		r.recordSpawnFailure()
	}
	if !r.gaveUpValue() {
		r.mu.Lock()
		r.gaveUp = true
		r.mu.Unlock()
		log.Printf("container-go: reaper spawn cycle failed after %d attempts; scheduling retry (binary=%q)", maxReaperSpawnFailures, r.binary)
	}
	return errors.New("reaper: giving up after repeated spawn failures")
}

// recoverAndReplay always gets a fresh recovery budget. A caller context
// may have expired while waiting for opMu or writing the old pipe; active
// entries must still be handed to a replacement rather than left without a
// watchdog.
func (r *reaper) recoverAndReplay(_ context.Context, process *reaperProcess) error {
	ctx, cancel := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
	defer cancel()
	if process == nil {
		r.mu.Lock()
		process = r.process
		r.mu.Unlock()
	}
	if process != nil {
		if err := r.stopProcessContext(ctx, process); err != nil {
			// Retain the published process until its strict shutdown
			// barrier completes. A later registration can then retry the
			// recovery without overlapping the old tree.
			return err
		}
		r.detachProcess(process)
	}
	return r.respawnAndReplayContext(ctx)
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
	r.process = nil
	r.cmd, r.stdin, r.exited, r.pgid = nil, nil, nil, 0
	return process
}

func (r *reaper) spawnFailuresValue() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spawnFailures
}

func (r *reaper) gaveUpValue() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gaveUp
}

func (r *reaper) recordSpawnFailure() {
	r.mu.Lock()
	r.spawnFailures++
	if r.spawnFailures >= maxReaperSpawnFailures {
		if r.retryLevel < 32 {
			r.retryLevel++
		}
		r.gaveUp = true
		r.retryAt = time.Now().Add(r.backoffLocked(r.retryLevel))
	}
	r.mu.Unlock()
}

// spawnProcess starts a reaper child and publishes its identity before a
// caller can replay records to it. The caller holds opMu.
func (r *reaper) spawnProcess() (*exec.Cmd, io.WriteCloser, <-chan struct{}, error) {
	r.mu.Lock()
	err := r.spawnLocked()
	cmd, stdin, exited := r.cmd, r.stdin, r.exited
	r.mu.Unlock()
	return cmd, stdin, exited, err
}

func (r *reaper) startProcessLocked() (*exec.Cmd, io.WriteCloser, <-chan struct{}, error) {
	timeout := r.timeoutSeconds
	if timeout <= 0 {
		timeout = defaultReaperTimeoutSeconds
	}
	pendingAttempts := r.pendingAttempts
	if pendingAttempts <= 0 {
		pendingAttempts = 1
	}
	awkPath, _ := trustedReaperTool("awk")
	sleepPath, _ := trustedReaperTool("sleep")
	lockKind := "lockf"
	lockBin, _ := trustedReaperTool("lockf")
	if runtime.GOOS == "linux" {
		lockKind = "flock"
		lockBin, _ = trustedReaperTool("flock")
	}
	rmPath, _ := trustedReaperTool("rm")
	psPath, _ := trustedReaperTool("ps")
	trPath, _ := trustedReaperTool("tr")
	pgrepPath, _ := trustedReaperTool("pgrep")
	setsidPath, _ := trustedReaperTool("setsid")
	lockDir := os.TempDir()
	statusDir := ""
	var err error
	if r.command == nil {
		statusDir, err = os.MkdirTemp("", "containergo-reaper-")
		if err != nil {
			return nil, nil, nil, err
		}
	}
	runPrefix := fmt.Sprintf("%d-%d", os.Getpid(), reaperInstanceCounter.Add(1))
	var cmd *exec.Cmd
	if r.command != nil {
		cmd = r.command()
	} else {
		cmd = exec.Command(
			"/bin/sh", "-c", reaperScript, "containergo-reaper",
			r.binary, r.subcommand, creationLabel,
			strconv.Itoa(timeout), strconv.Itoa(pendingAttempts),
			awkPath, sleepPath, lockBin, lockDir, rmPath, runPrefix, "main", psPath, trPath, statusDir, lockKind, pgrepPath, setsidPath,
		)
		env := make([]string, 0, len(os.Environ())+1)
		for _, value := range os.Environ() {
			if strings.HasPrefix(value, "CONTAINERGO_REAPER_SCRIPT=") {
				continue
			}
			env = append(env, value)
		}
		cmd.Env = append(env, "CONTAINERGO_REAPER_SCRIPT="+reaperScript)
	}
	if cmd == nil {
		removeReaperStatusDir(statusDir)
		return nil, nil, nil, errors.New("reaper: nil spawn command")
	}
	group, err := newReaperGroupOwner()
	if err != nil {
		removeReaperStatusDir(statusDir)
		return nil, nil, nil, err
	}
	prepareReaperCommand(cmd, group)
	var sentinelRead, sentinelWrite, readyRead, readyWrite *os.File
	if r.command == nil {
		sentinelRead, sentinelWrite, err = os.Pipe()
		if err != nil {
			group.finish()
			removeReaperStatusDir(statusDir)
			return nil, nil, nil, err
		}
		readyRead, readyWrite, err = os.Pipe()
		if err != nil {
			closeReaperFile(sentinelRead)
			closeReaperFile(sentinelWrite)
			group.finish()
			removeReaperStatusDir(statusDir)
			return nil, nil, nil, err
		}
		cmd.ExtraFiles = []*os.File{sentinelRead, readyWrite}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeReaperFile(sentinelRead)
		closeReaperFile(sentinelWrite)
		closeReaperFile(readyRead)
		closeReaperFile(readyWrite)
		group.finish()
		removeReaperStatusDir(statusDir)
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		closeReaperFile(sentinelRead)
		closeReaperFile(sentinelWrite)
		closeReaperFile(readyRead)
		closeReaperFile(readyWrite)
		group.finish()
		removeReaperStatusDir(statusDir)
		return nil, nil, nil, err
	}
	closeReaperFile(sentinelRead)
	closeReaperFile(readyWrite)
	if r.command == nil {
		group.attach(cmd.Process.Pid, sentinelWrite, readyRead)
		sentinelPID, err := waitForReaperSentinelReady(readyRead)
		if err != nil {
			group.signal()
			_ = stdin.Close()
			closeReaperFile(sentinelWrite)
			closeReaperFile(readyRead)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			group.finish()
			removeReaperStatusDir(statusDir)
			return nil, nil, nil, err
		}
		group.setSentinelPID(sentinelPID)
	} else {
		group.attach(cmd.Process.Pid, nil, nil)
		// Test seams that provide their own command do not need the
		// production shell sentinel. Keep only the direct process handle
		// for ownership-safe termination.
		group.finish()
		group = nil
	}
	exited := make(chan struct{})
	pgid := reaperProcessGroupID(cmd)
	if group != nil && group.pid > 0 {
		pgid = group.pid
	}
	process := &reaperProcess{cmd: cmd, stdin: stdin, exited: exited, pgid: pgid, group: group, statusDir: statusDir}
	r.process = process
	r.cmd, r.stdin, r.exited, r.pgid = cmd, stdin, exited, pgid
	go r.waitProcess(process)
	r.watchers.Add(1)
	go func() {
		defer r.watchers.Done()
		r.respawnAfterUnexpectedExit(cmd, exited)
	}()
	return cmd, stdin, exited, nil
}

// spawnLocked preserves the original helper shape for package tests. The
// caller holds r.mu while starting the child.
func (r *reaper) spawnLocked() error {
	_, _, _, err := r.startProcessLocked()
	return err
}

func (r *reaper) waitProcess(process *reaperProcess) {
	// The shell sentinel remains in the root group until finish, so the
	// direct child can be reaped first and the group can still be signaled
	// safely afterwards without a nonportable wait-status probe.
	process.waitAndMarkReaped()
	terminateReaperProcess(process)
	waitForReaperGroupExit(process.pgid)
	cleanupReaperDescendants(process)
	finishReaperProcess(process)
	close(process.exited)
}

func (r *reaper) stopCurrentProcessContext(ctx context.Context) error {
	r.mu.Lock()
	process := r.process
	r.mu.Unlock()
	if process == nil {
		return nil
	}
	if err := r.stopProcessContext(ctx, process); err != nil {
		return err
	}
	r.detachProcess(process)
	return nil
}

func (r *reaper) stopProcessContext(ctx context.Context, process *reaperProcess) error {
	if process == nil {
		return nil
	}
	// Claim the process-group signal before closing the registration pipe.
	// Closing first would intentionally release the old script to run
	// deletes while a replacement is already being prepared.
	process.terminate()
	if process.stdin != nil {
		_ = process.stdin.Close()
	}
	if process.exited == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-process.exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("reaper: child shutdown: %w", ctx.Err())
	}
}

// respawnAfterUnexpectedExit keeps the insurance alive while the parent is
// still running. closeStdin marks the intentional EOF used by tests and
// best-effort shutdown; an actual child exit is replayed.
func (r *reaper) respawnAfterUnexpectedExit(cmd *exec.Cmd, exited <-chan struct{}) {
	<-exited
	lockCtx, cancel := context.WithTimeout(context.Background(), r.operationLockTimeout)
	defer cancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		log.Printf("container-go: reaper recovery lock: %v", err)
		r.requestReconcile()
		return
	}
	defer r.opMu.Unlock()
	r.mu.Lock()
	current := !r.closed && r.cmd == cmd
	r.mu.Unlock()
	if current {
		r.requestReconcile()
	}
}

// closeStdin hands the reaper the same EOF it would see on parent death.
// Test hook and best-effort shutdown. The process watcher remains
// responsible for reaping the child; callers that need a completion
// barrier can wait on r.exited.
func (r *reaper) closeStdin() {
	lockCtx, cancel := context.WithTimeout(context.Background(), r.operationLockTimeout)
	defer cancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		log.Printf("container-go: reaper close lock: %v", err)
		return
	}
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	stdin := r.stdin
	r.stdin = nil
	r.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

// killForTest kills the current reaper process group and waits until the
// child and its descendants have been reaped before a replacement starts.
// Keep the process published while waiting: the exit watcher must still
// observe the old command and perform the normal recovery replay.
func (r *reaper) killForTest() {
	lockCtx, cancel := context.WithTimeout(context.Background(), r.operationLockTimeout)
	defer cancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		return
	}
	defer r.opMu.Unlock()
	r.mu.Lock()
	process := r.process
	r.mu.Unlock()
	if process == nil {
		return
	}
	process.terminate()
	if process.stdin != nil {
		_ = process.stdin.Close()
	}
	if process.exited == nil {
		return
	}
	processCtx, processCancel := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
	defer processCancel()
	select {
	case <-process.exited:
	case <-processCtx.Done():
	}
}

func processLive(process *reaperProcess) bool {
	return process != nil && process.stdin != nil && !channelClosed(process.exited)
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

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

// registerWithGlobalReaper best-effort registers a container with the
// process-wide reaper for its backend binary. Reaper trouble is logged
// but never fails container startup. The reaper needs /bin/sh, so on
// Windows this is a no-op and cleanup relies on the normal paths.
func registerWithGlobalReaper(binary, subcommand, id, creation string) error {
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
	if err := r.register(id, creation); err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func preRegisterWithGlobalReaper(binary, subcommand, id, creation string) error {
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
	if err := r.registerPending(id, creation); err != nil {
		log.Printf("container-go: reaper pre-registration failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func completePreRegistrationWithGlobalReaper(binary, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	globalReapersMu.Unlock()
	if !ok {
		return nil
	}
	if err := r.completePending(id, creation); err != nil {
		log.Printf("container-go: reaper create-completion update failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}
