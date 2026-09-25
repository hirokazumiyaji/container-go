package container

import (
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
// data validated as an Apple Container name or full Docker ID, and the
// script itself disables globbing and quotes every expansion the IDs reach.
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
const reaperScript = `set -f
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

var (
	errReaperSpawnCooldown = errors.New("reaper: spawn retry cooldown active")
	errReaperSpawnFailed   = errors.New("reaper: giving up after repeated spawn failures")
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

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
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
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{binary: binary, subcommand: subcommand, now: time.Now}
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
	entry := reaperEntry{id: id, creation: creation}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Do not grow active state when the same registration is repeated.
	// Still verify the pipe, because a repeated registration can be the
	// first observation that an old reaper child has exited.
	if r.containsActiveLocked(entry) {
		if !r.retryReadyLocked() {
			return r.spawnCooldownErrorLocked()
		}
		if r.stdin != nil && !channelClosed(r.exited) {
			if err := r.writeLocked(entry); err == nil {
				r.clearSpawnFailureLocked()
				return nil
			}
		}
		r.stopProcessLocked()
		return r.respawnAndReplayLocked()
	}

	r.removeCompletedLocked(entry)
	r.entries = append(r.entries, entry)
	if !r.retryReadyLocked() {
		return r.spawnCooldownErrorLocked()
	}
	if r.stdin != nil && !channelClosed(r.exited) {
		if err := r.writeLocked(entry); err == nil {
			r.clearSpawnFailureLocked()
			return nil
		}
	}
	r.stopProcessLocked()
	return r.respawnAndReplayLocked()
}

func (r *reaper) writeLocked(e reaperEntry) error {
	return r.writeRecordLocked("+", e)
}

func (r *reaper) writeRecordLocked(operation string, e reaperEntry) error {
	if r.stdin == nil {
		return io.ErrClosedPipe
	}
	line := operation + " " + e.id
	if e.creation != "" {
		line += " " + e.creation
	}
	line += "\n"
	n, err := io.WriteString(r.stdin, line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
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
	entry := reaperEntry{id: id, creation: creation}
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.removeActiveLocked(entry) {
		return nil
	}
	r.rememberCompletedLocked(entry)

	if r.stdin == nil || channelClosed(r.exited) {
		r.stopProcessLocked()
		if len(r.entries) == 0 {
			r.clearSpawnFailureLocked()
			return nil
		}
		return r.respawnAndReplayLocked()
	}
	if err := r.writeRecordLocked("-", entry); err == nil {
		r.clearSpawnFailureLocked()
		// Keep the child alive with an empty active set. This avoids a
		// process spawn for every sequential create/terminate cycle; EOF
		// still lets the child exit without issuing any deletes.
		return nil
	}

	// A failed cancellation cannot be trusted to reach the old child.
	// Stop it before replaying, otherwise it could still delete the
	// completed entry after the parent exits.
	r.stopProcessLocked()
	if len(r.entries) == 0 {
		r.clearSpawnFailureLocked()
		return nil
	}
	return r.respawnAndReplayLocked()
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

// respawnAndReplayLocked starts a fresh reaper process and re-registers
// every active ID with it. Completed entries are never replayed. Three
// consecutive failures enter a cooldown rather than permanently
// disabling the process; a later registration after the cooldown clears
// the failure state and tries again.
func (r *reaper) respawnAndReplayLocked() error {
	if len(r.entries) == 0 {
		r.clearSpawnFailureLocked()
		return nil
	}
	if !r.retryReadyLocked() {
		return r.spawnCooldownErrorLocked()
	}
	r.stopProcessLocked()

	var lastErr error
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			lastErr = err
			r.recordSpawnFailureLocked()
			continue
		}
		replayed := true
		for _, entry := range r.entries {
			if err := r.writeLocked(entry); err != nil {
				lastErr = err
				replayed = false
				break
			}
		}
		if replayed {
			r.clearSpawnFailureLocked()
			return nil
		}
		r.stopProcessLocked()
		r.recordSpawnFailureLocked()
	}
	if !r.gaveUp {
		r.recordSpawnFailureLocked()
	}
	if !r.gaveUpLogged {
		log.Printf("container-go: reaper giving up temporarily after %d consecutive failures (binary=%q); retrying after cooldown", maxReaperSpawnFailures, r.binary)
		r.gaveUpLogged = true
	}
	if lastErr == nil {
		lastErr = errReaperSpawnFailed
	}
	return fmt.Errorf("%w: %v", errReaperSpawnFailed, lastErr)
}

func (r *reaper) spawnLocked() error {
	var cmd *exec.Cmd
	if r.command != nil {
		cmd = r.command()
	} else {
		cmd = exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel))
	}
	if cmd == nil {
		return errors.New("reaper: nil spawn command")
	}
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
	return nil
}

func (r *reaper) stopProcessLocked() {
	stdin, cmd, exited := r.stdin, r.cmd, r.exited
	r.stdin, r.cmd, r.exited = nil, nil, nil
	if cmd != nil {
		killReaperCommand(cmd)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && exited != nil {
		<-exited
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
	defer r.mu.Unlock()
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
}

// killForTest kills the reaper process group and waits until the child
// is reaped, so the next write deterministically fails.
func (r *reaper) killForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		killReaperCommand(r.cmd)
		<-r.exited
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

func unregisterWithGlobalReaper(registration *reaperRegistration) {
	if registration == nil || registration.reaper == nil {
		return
	}
	if err := registration.reaper.unregister(registration.entry.id, registration.entry.creation); err != nil {
		log.Printf("container-go: reaper unregister %s: %v", registration.entry.id, err)
	}
}
