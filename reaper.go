package container

import (
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
  children=$(pgrep -P "$1" 2>/dev/null) || children=
  for child in $children; do
    kill_descendants "$child"
    kill -9 "$child" 2>/dev/null || true
  done
}
kill_pipeline() {
  pid="$1"
  kill_descendants "$pid"
  kill -9 "$pid" 2>/dev/null || true
  kill -9 -"$pid" 2>/dev/null || true
}
run_with_timeout() {
  seconds="$1"
  shift
  "$@" & pid=$!
  (sleep "$seconds"; kill_pipeline "$pid") & killer=$!
  wait "$pid" 2>/dev/null
  rc=$?
  kill "$killer" 2>/dev/null || true
  wait "$killer" 2>/dev/null || true
  return "$rc"
}
process_entry() {
  id="$1"
  creation="$2"
  pending="$3"
  target="$id"
  if [ -n "$creation" ]; then
    tries=0
    while :; do
      tmp=$(mktemp 2>/dev/null) || return 0
      if run_with_timeout 10 "$bin" inspect "$id" >"$tmp" 2>/dev/null; then
        got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
        uid=$(sed -n 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/\1/p' "$tmp" 2>/dev/null | head -n 1)
        rm -f "$tmp"
        # A missing generation is retried only for a pending create.
        if [ "$got" != "$creation" ]; then
          [ "$pending" = 1 ] && [ -z "$got" ] || return 0
        else
          [ -n "$uid" ] && target="$uid"
          break
        fi
      fi
      rm -f "$tmp"
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
	// stopOnce makes the process-group signal happen exactly once, before
	// either the recovery path or the wait path calls cmd.Wait. This keeps
	// a saved numeric group ID from being used after PID reuse.
	stopOnce sync.Once
}

func (p *reaperProcess) terminate() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		killReaperProcess(p.cmd, p.pgid)
	})
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	// opMu serializes registration, completion, close, and respawn
	// transitions. r.mu protects the fields below; no process wait is
	// performed while r.mu is held.
	opMu          sync.Mutex
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

	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
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
	stdin, exited, failures := r.stdin, r.exited, r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && stdin != nil && !channelClosed(exited) {
		if err := writeReaperEntry(stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.respawnAndReplayLocked()
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

func writeReaperEntry(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	prefix := "+"
	if entry.pending {
		prefix = "P"
	}
	line := prefix + " " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) writeLocked(entry reaperEntry) error {
	return writeReaperEntry(r.stdin, entry)
}

func (r *reaper) writeCurrentEntry(entry reaperEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeLocked(entry)
}

func writeReaperCompletion(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "C " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

// completePending marks a pre-registered generation as having completed
// the backend create. The entry remains in the replay set: if the parent
// dies later, EOF still asks the reaper to delete that exact generation.
func (r *reaper) completePending(id, creation string) error {
	entry := reaperEntry{id: id, creation: creation, pending: true}
	if err := validateReaperEntry(entry); err != nil {
		return err
	}

	r.opMu.Lock()
	defer r.opMu.Unlock()
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
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return errors.New("reaper: closed")
		}
		return nil
	}
	entry.pending = false
	r.entries[index] = entry
	stdin, exited, failures := r.stdin, r.exited, r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && stdin != nil && !channelClosed(exited) {
		if err := writeReaperCompletion(stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.respawnAndReplayLocked()
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
	r.stopCurrentProcess()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
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
		_, _, _, err := r.spawnProcess()
		if err != nil {
			r.recordSpawnFailure()
			continue
		}
		replayed := true
		for _, entry := range entries {
			if err := r.writeCurrentEntry(entry); err != nil {
				replayed = false
				break
			}
		}
		if replayed {
			r.clearSpawnFailure()
			return nil
		}
		r.stopCurrentProcess()
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
	// On Unix, observe the child with WNOWAIT first. The process group is
	// terminated while the direct child is still owned, before Wait can
	// recycle its PID. Other platforms have no watchdog process to recover.
	if waitForReaperTermination(process.cmd) {
		process.terminate()
	}
	_ = process.cmd.Wait()
	finishReaperProcess(process.cmd, process.pgid)
	close(process.exited)
}

func (r *reaper) stopCurrentProcess() {
	r.mu.Lock()
	process := r.process
	r.process = nil
	r.cmd, r.stdin, r.exited, r.pgid = nil, nil, nil, 0
	r.mu.Unlock()
	if process != nil {
		r.stopProcess(process)
	}
}

func (r *reaper) stopProcess(process *reaperProcess) {
	// Claim the process-group signal before closing the registration pipe.
	// Closing first would intentionally release the old script to run
	// deletes while a replacement is already being prepared.
	process.terminate()
	if process.stdin != nil {
		_ = process.stdin.Close()
	}
	if process.exited != nil {
		<-process.exited
	}
}

// respawnAfterUnexpectedExit keeps the insurance alive while the parent is
// still running. closeStdin marks the intentional EOF used by tests and
// best-effort shutdown; an actual child exit is replayed.
func (r *reaper) respawnAfterUnexpectedExit(cmd *exec.Cmd, exited <-chan struct{}) {
	<-exited
	r.opMu.Lock()
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
	r.opMu.Lock()
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
func (r *reaper) killForTest() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	process := r.process
	r.mu.Unlock()
	if process == nil {
		return
	}
	process.terminate()
	if process.exited != nil {
		<-process.exited
	}
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
