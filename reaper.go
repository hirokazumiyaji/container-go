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
case "$timeout" in ''|*[!0-9]*) timeout=30;; esac
case "$pending_attempts" in ''|*[!0-9]*) pending_attempts=30;; esac
[ "$timeout" -gt 0 ] 2>/dev/null || timeout=30
[ "$pending_attempts" -gt 0 ] 2>/dev/null || pending_attempts=1

# With the default timeout this is a sleep 30 bound; timeout(1) is not
# available on every supported Unix host.
kill_descendants() {
  kill_descendant_children=$(pgrep -P "$1" 2>/dev/null) || kill_descendant_children=
  for kill_descendant_pid in $kill_descendant_children; do
    kill_descendants "$kill_descendant_pid"
    kill -9 "$kill_descendant_pid" 2>/dev/null || true
  done
}
kill_pipeline() {
  kill_target_pid="$1"
  kill_descendants "$kill_target_pid"
  # A background target is not necessarily a process-group leader. Only
  # signal a negative PID after confirming that it owns that group; the
  # target is still owned by the waiting shell, so its PID cannot be reused.
  kill_target_pgid=$(ps -o pgid= -p "$kill_target_pid" 2>/dev/null | tr -d '[:space:]') || kill_target_pgid=
  if [ "$kill_target_pgid" = "$kill_target_pid" ]; then
    kill -9 -"$kill_target_pid" 2>/dev/null || true
  fi
  kill -9 "$kill_target_pid" 2>/dev/null || true
}
run_with_timeout() {
  timeout_seconds="$1"
  shift
  timeout_command_pid=
  timeout_killer_pid=
  "$@" &
  timeout_command_pid=$!
  (
    timeout_sleep_pid=
    cleanup_timeout_killer() {
      if [ -n "$timeout_sleep_pid" ]; then
        kill -9 "$timeout_sleep_pid" 2>/dev/null || true
        wait "$timeout_sleep_pid" 2>/dev/null || true
      fi
    }
    trap 'cleanup_timeout_killer; exit 0' 0 1 2 15
    sleep "$timeout_seconds" &
    timeout_sleep_pid=$!
    wait "$timeout_sleep_pid" || true
    timeout_sleep_pid=
    kill_pipeline "$timeout_command_pid"
  ) &
  timeout_killer_pid=$!
  wait "$timeout_command_pid" 2>/dev/null
  timeout_rc=$?
  kill "$timeout_killer_pid" 2>/dev/null || true
  wait "$timeout_killer_pid" 2>/dev/null || true
  return "$timeout_rc"
}
inspect_projection() {
  # Project only the fields needed for guarded deletion while the backend
  # response is streaming. The raw inspect response may contain credentials,
  # so never stage it in a temporary file.
  id="$1"
  {
    "$bin" inspect "$id" 2>/dev/null
    inspect_rc=$?
    printf '\n__containergo_reaper_inspect_rc__%s\n' "$inspect_rc"
  } | sed -n \
    -e "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/generation=\1/p" \
    -e 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/id=\1/p' \
    -e 's/^__containergo_reaper_inspect_rc__\([0-9][0-9]*\)$/rc=\1/p'
}
process_entry() {
  id="$1"
  creation="$2"
  pending="$3"
  target="$id"
  if [ -n "$creation" ]; then
    tries=0
    while :; do
      inspect_fields=$(run_with_timeout 10 inspect_projection "$id") || inspect_fields=
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
        # A missing generation is retried only for a pending create.
        if [ "$got" != "$creation" ]; then
          [ "$pending" = 1 ] && [ -z "$got" ] || return 0
        else
          [ -n "$uid" ] && target="$uid"
          break
        fi
      fi
      [ "$pending" = 1 ] || return 0
      tries=$((tries + 1))
      [ "$tries" -lt "$pending_attempts" ] || return 0
      sleep 1
    done
  fi
  run_with_timeout "$timeout" "$bin" "$sub" --force "$target" || true
}
awk '
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
  pending=0
  [ "$state" = "P" ] && pending=1
  run_with_timeout "$timeout" process_entry "$id" "$creation" "$pending" >/dev/null 2>&1 || true
done
`

const (
	maxReaperSpawnFailures       = 3
	defaultReaperTimeoutSeconds  = 30
	defaultReaperPendingAttempts = 30
)

var (
	reaperWriteTimeout         = 500 * time.Millisecond
	reaperOperationLockTimeout = 30 * time.Second
	reaperOperationTimeout     = 5 * time.Second
	reaperRecoveryTimeout      = 5 * time.Second
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
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
	pgid   int

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
		killReaperProcess(p.cmd, p.pgid)
	})
}

func (p *reaperProcess) waitAndMarkReaped() {
	if p == nil {
		return
	}
	p.stateMu.Lock()
	_ = p.cmd.Wait()
	p.reaped = true
	p.stateMu.Unlock()
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
	opMu          reaperOperationLock
	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	exited        chan struct{}
	pgid          int
	process       *reaperProcess
	closed        bool
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool

	// timeoutSeconds and pendingAttempts are internal test seams.
	// Production reapers use the bounded defaults; a create that has not
	// appeared yet gets a final inspect window after parent EOF.
	timeoutSeconds  int
	pendingAttempts int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:          binary,
		subcommand:      subcommand,
		timeoutSeconds:  defaultReaperTimeoutSeconds,
		pendingAttempts: defaultReaperPendingAttempts,
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

func (r *reaper) registerEntry(entry reaperEntry) error {
	if err := validateReaperEntry(entry); err != nil {
		return err
	}
	if entry.pending && entry.creation == "" {
		return errors.New("reaper: pending entry requires a creation id")
	}

	lockCtx, lockCancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
	defer lockCancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
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
	process := r.process
	failures := r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && processLive(process) {
		if err := writeReaperEntry(opCtx, process.stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.recoverAndReplay(opCtx, process)
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

	lockCtx, lockCancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
	defer lockCancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
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
	index := -1
	for i, active := range r.entries {
		if active.pending && active.id == id && active.creation == creation {
			index = i
			break
		}
	}
	if index < 0 {
		r.mu.Unlock()
		return nil
	}
	entry.pending = false
	r.entries[index] = entry
	process := r.process
	failures := r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && processLive(process) {
		if err := writeReaperCompletion(opCtx, process.stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.recoverAndReplay(opCtx, process)
}

func (r *reaper) clearSpawnFailure() {
	r.mu.Lock()
	r.spawnFailures = 0
	r.gaveUp = false
	r.mu.Unlock()
}

// respawnAndReplayLocked starts a replacement while the caller holds
// opMu. r.mu is only used for short state snapshots and never held while
// waiting for the old process tree.
func (r *reaper) respawnAndReplayLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), reaperRecoveryTimeout)
	defer cancel()
	return r.respawnAndReplayContext(ctx)
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
		log.Printf("container-go: reaper giving up after %d consecutive spawn failures (binary=%q)", maxReaperSpawnFailures, r.binary)
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
	cmd := exec.Command(
		"/bin/sh", "-c", reaperScript, "containergo-reaper",
		r.binary, r.subcommand, breQuote(creationLabel),
		strconv.Itoa(timeout), strconv.Itoa(pendingAttempts),
	)
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, nil, err
	}
	exited := make(chan struct{})
	pgid := reaperProcessGroupID(cmd)
	process := &reaperProcess{cmd: cmd, stdin: stdin, exited: exited, pgid: pgid}
	r.process = process
	r.cmd, r.stdin, r.exited, r.pgid = cmd, stdin, exited, pgid
	go r.waitProcess(process)
	go r.respawnAfterUnexpectedExit(cmd, exited)
	return cmd, stdin, exited, nil
}

// spawnLocked preserves the original helper shape for package tests. The
// caller holds r.mu while starting the child.
func (r *reaper) spawnLocked() error {
	_, _, _, err := r.startProcessLocked()
	return err
}

func (r *reaper) waitProcess(process *reaperProcess) {
	// Observe the direct child without reaping it, then terminate the
	// still-owned process group. The group barrier must complete before
	// os/exec reaps the child; otherwise a replacement could overlap old
	// descendants and a saved PGID could be recycled underneath us.
	_ = waitForReaperTermination(process.cmd)
	process.terminate()
	_ = waitForReaperProcessGroupExit(process.pgid)
	process.waitAndMarkReaped()
	// No signal is sent after Wait. finishReaperProcess is only a
	// platform-specific hook for code that needs to observe completion.
	finishReaperProcess(process.cmd, process.pgid)
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
	lockCtx, cancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
	defer cancel()
	if err := r.opMu.LockContext(lockCtx); err != nil {
		log.Printf("container-go: reaper recovery lock: %v", err)
		return
	}
	defer r.opMu.Unlock()
	r.mu.Lock()
	current := !r.closed && r.cmd == cmd
	r.mu.Unlock()
	if current {
		_ = r.respawnAndReplayLocked()
	}
}

// closeStdin hands the reaper the same EOF it would see on parent death.
// Test hook and best-effort shutdown. The process watcher remains
// responsible for reaping the child; callers that need a completion
// barrier can wait on r.exited.
func (r *reaper) closeStdin() {
	lockCtx, cancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
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
	lockCtx, cancel := context.WithTimeout(context.Background(), reaperOperationLockTimeout)
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
