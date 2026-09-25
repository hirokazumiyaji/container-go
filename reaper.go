package container

import (
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
	"time"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper deletes every registered container.
// While this process lives, the reaper does nothing; deletion is the job
// of Terminate/Cleanup, the reaper is insurance.
//
// The child runs detached in its own process group so a signal aimed at
// this process's group does not remove the watchdog before it observes
// the pipe EOF, and a supervisor goroutine restarts and replays it if it
// exits unexpectedly while this process is still alive.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated as an Apple Container name or a full Docker ID, and the
// script itself disables globbing and quotes every expansion the IDs
// reach. Every backend call runs behind a bounded timeout implemented
// with background jobs and kill (timeout(1) is not standard on macOS),
// so a hung daemon cannot wedge deletion of later entries. Failures stay
// silent (|| true) by design: the reaper is last-resort insurance.
//
// Records are tab-separated: state, ID, creation, then the three name
// barriers and their device:inode:uid identities. The state is one of
// P (pending, written before a create starts), A (active), C (the
// pending record is confirmed, so a create that is still settling no
// longer needs the retry budget) or D (the record is withdrawn). The
// child keeps the last record per ID and generation, so a confirmed,
// withdrawn, or promoted entry replaces the pending one it supersedes.
//
// When a creation generation is known, the script captures inspect
// output in memory and reads the creation label as a structural JSON
// field: the match is anchored at line start on the quoted key, so label
// values or other text containing the same characters cannot satisfy it.
// Apple entries also require the managed marker, and a pending entry
// never removes a shared reuse generation, which a peer may have adopted
// while the create was still settling. Name-addressed entries carry the
// per-name lock barriers used by the library: the child acquires them in
// the fixed legacy-to-durable order, verifies each barrier's recorded
// device:inode:uid identity before and after the guarded work, and skips
// the delete when a barrier was replaced or is no longer private. The
// create, prune, and cleanup paths take the same barriers in the same
// order, so a cooperating process cannot replace the name in the middle
// of the guarded operation. Entries addressed by a full Docker ID need
// no barrier because a replacement never reuses that ID.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
managed_key="$4"
reuse_key="$5"
pending_attempts="${6:-15}"
case "$pending_attempts" in ''|*[!0-9]*) pending_attempts=15;; esac
[ "$pending_attempts" -gt 0 ] 2>/dev/null || pending_attempts=1
stat_bin=$(command -v stat 2>/dev/null) || stat_bin=
case "$stat_bin" in
  /*) [ -x "$stat_bin" ] || stat_bin= ;;
  *) stat_bin= ;;
esac
ids=""
tab=$(printf '\t')
while IFS= read -r line; do
  ids="$ids
$line"
done
run_with_timeout() {
  "$@" >/dev/null 2>&1 & pid=$!
  deadline=$(( $(date +%s) + 30 ))
  while kill -0 "$pid" 2>/dev/null; do
    now=$(date +%s)
    if [ "$now" -ge "$deadline" ]; then
      kill -9 "$pid" 2>/dev/null
      break
    fi
    sleep 0.05
  done
  wait "$pid" 2>/dev/null
  return $?
}
run_pending_timeout() {
  "$@" >/dev/null 2>&1 & pid=$!
  deadline=$(( $(date +%s) + 120 ))
  while kill -0 "$pid" 2>/dev/null; do
    now=$(date +%s)
    if [ "$now" -ge "$deadline" ]; then
      kill -9 "$pid" 2>/dev/null
      break
    fi
    sleep 0.05
  done
  wait "$pid" 2>/dev/null
  return $?
}
locked_command='
  stat_bin="$REAPER_STAT_BIN"
  stat_identity() {
    "$stat_bin" -c "%d:%i:%u" "$1" 2>/dev/null || "$stat_bin" -f "%d:%i:%u" "$1" 2>/dev/null
  }
  verify_lock_identity() {
    [ -n "$1" ] && [ -n "$2" ] || return 1
    [ -L "$1" ] && return 1
    [ -f "$1" ] || return 1
    actual=$(stat_identity "$1") || return 1
    [ "$actual" = "$2" ] || return 1
    return 0
  }
  capture() {
    "$@" 2>/dev/null & pid=$!
    deadline=$(( $(date +%s) + 10 ))
    while kill -0 "$pid" 2>/dev/null; do
      now=$(date +%s)
      if [ "$now" -ge "$deadline" ]; then
        kill -9 "$pid" 2>/dev/null
        break
      fi
      sleep 0.05
    done
    wait "$pid" 2>/dev/null
  }
  delete_bounded() {
    "$@" >/dev/null 2>&1 & pid=$!
    deadline=$(( $(date +%s) + 30 ))
    while kill -0 "$pid" 2>/dev/null; do
      now=$(date +%s)
      if [ "$now" -ge "$deadline" ]; then
        kill -9 "$pid" 2>/dev/null
        break
      fi
      sleep 0.05
    done
    wait "$pid" 2>/dev/null
    return $?
  }
  id="$1"
  creation="$2"
  pending="$3"
  lock1="$4"
  lockid1="$5"
  lock2="$6"
  lockid2="$7"
  lock3="$8"
  lockid3="$9"
  bin="$REAPER_BIN"
  sub="$REAPER_SUB"
  key="$REAPER_KEY"
  managed_key="$REAPER_MANAGED_KEY"
  reuse_key="$REAPER_REUSE_KEY"
  [ "$sub" = delete ] && [ -z "$creation" ] && exit 0
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  budget=5
  pause=0.1
  if [ "$pending" = 1 ]; then
    budget=$REAPER_PENDING_ATTEMPTS
    pause=1
  fi
  ready=0
  tries=0
  while :; do
    metadata=$(capture "$bin" inspect "$id") || metadata=
    got_managed=$(printf "%s\n" "$metadata" | sed -n "s/^[[:space:]]*\"$managed_key\"[[:space:]]*:[[:space:]]*\"true\".*/true/p" | head -n 1)
    if [ "$got_managed" = "true" ]; then
      got_reuse=$(printf "%s\n" "$metadata" | sed -n "s/^[[:space:]]*\"$reuse_key\"[[:space:]]*:[[:space:]]*\"true\".*/true/p" | head -n 1)
      if [ -n "$creation" ]; then
        got=$(printf "%s\n" "$metadata" | sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" | head -n 1)
        [ "$got" = "$creation" ] || exit 0
        # A pending record is a safety net for a create that may still be
        # settling, so it never removes a shared reuse generation a peer
        # may have adopted in the meantime.
        if [ "$pending" = 1 ] && [ "$got_reuse" = "true" ]; then
          exit 0
        fi
        ready=1
      else
        [ "$got_reuse" = "true" ] || ready=1
      fi
    fi
    [ "$ready" = 1 ] && break
    tries=$((tries + 1))
    [ "$tries" -lt "$budget" ] || exit 0
    sleep "$pause"
  done
  target="$id"
  if [ "$sub" = rm ]; then
    uid=$(printf "%s\n" "$metadata" | sed -n "s/^[[:space:]]*\"Id\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p" | head -n 1)
    [ -n "$uid" ] && [ "$uid" = "$id" ] || exit 0
    target="$uid"
  fi
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  delete_bounded "$bin" "$sub" --force "$target" || true
  exit 0
'
lock_two_command='
  stat_bin="$REAPER_STAT_BIN"
  stat_identity() {
    "$stat_bin" -c "%d:%i:%u" "$1" 2>/dev/null || "$stat_bin" -f "%d:%i:%u" "$1" 2>/dev/null
  }
  verify_lock_identity() {
    [ -n "$1" ] && [ -n "$2" ] || return 1
    [ -L "$1" ] && return 1
    [ -f "$1" ] || return 1
    actual=$(stat_identity "$1") || return 1
    [ "$actual" = "$2" ] || return 1
    return 0
  }
  lock1="$1"
  lockid1="$2"
  lock2="$3"
  lockid2="$4"
  lock3="$5"
  lockid3="$6"
  id="$7"
  creation="$8"
  pending="$9"
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  "$REAPER_LOCKF_BIN" -k -w -t 30 "$lock3" sh -c "$REAPER_LOCKED_COMMAND" \
    reaper-locked "$id" "$creation" "$pending" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" || true
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  exit 0
'
lock_one_command='
  stat_bin="$REAPER_STAT_BIN"
  stat_identity() {
    "$stat_bin" -c "%d:%i:%u" "$1" 2>/dev/null || "$stat_bin" -f "%d:%i:%u" "$1" 2>/dev/null
  }
  verify_lock_identity() {
    [ -n "$1" ] && [ -n "$2" ] || return 1
    [ -L "$1" ] && return 1
    [ -f "$1" ] || return 1
    actual=$(stat_identity "$1") || return 1
    [ "$actual" = "$2" ] || return 1
    return 0
  }
  lock1="$1"
  lockid1="$2"
  lock2="$3"
  lockid2="$4"
  lock3="$5"
  lockid3="$6"
  id="$7"
  creation="$8"
  pending="$9"
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  "$REAPER_LOCKF_BIN" -k -w -t 30 "$lock2" sh -c "$REAPER_LOCK_TWO_COMMAND" \
    reaper-lock-two "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" "$id" "$creation" "$pending" || true
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  exit 0
'
run_locked() {
  id="$1"
  creation="$2"
  pending="$3"
  lock1="$4"
  lockid1="$5"
  lock2="$6"
  lockid2="$7"
  lock3="$8"
  lockid3="$9"
  [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  case "$lockf_bin" in
    /*) [ -x "$lockf_bin" ] || return 0 ;;
    *) return 0 ;;
  esac
  REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
    REAPER_MANAGED_KEY="$managed_key" REAPER_REUSE_KEY="$reuse_key" \
    REAPER_PENDING_ATTEMPTS="$pending_attempts" REAPER_STAT_BIN="$stat_bin" \
    REAPER_LOCKF_BIN="$lockf_bin" REAPER_LOCKED_COMMAND="$locked_command" \
    REAPER_LOCK_TWO_COMMAND="$lock_two_command" \
    "$lockf_bin" -k -w -t 30 "$lock1" sh -c "$lock_one_command" \
      reaper-lock-one "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" \
      "$id" "$creation" "$pending" || true
}
records=$(printf "%s\n" "$ids" | awk -F '\t' '
  $1 == "P" { key=$2 SUBSEP $3; line[key]=$0; next }
  $1 == "A" { key=$2 SUBSEP $3; line[key]=$0; next }
  $1 == "C" { key=$2 SUBSEP $3; if (key in line) sub(/^P\t/, "A\t", line[key]); next }
  $1 == "D" { key=$2 SUBSEP $3; delete line[key]; next }
  NF >= 3 { key=$2 SUBSEP $3; line[key]=$0; next }
  END { for (key in line) print line[key] }
')
printf "%s\n" "$records" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  state=${line%%"$tab"*}
  rest=${line#*"$tab"}
  id=${rest%%"$tab"*}
  rest=${rest#*"$tab"}
  creation=""
  lock1=""
  lockid1=""
  lock2=""
  lockid2=""
  lock3=""
  lockid3=""
  case "$rest" in
    *"$tab"*)
      creation=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lock1=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lockid1=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lock2=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lockid2=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lock3=${rest%%"$tab"*}
      lockid3=${rest##*"$tab"}
      case "$lockid3" in *"$tab"*) continue ;; esac
      ;;
  esac
  pending=0
  case "$state" in
    P) pending=1 ;;
    A) ;;
    *) continue ;;
  esac
  [ -n "$id" ] || continue
  case "$id" in *[!0-9A-Za-z_.-]*) continue ;; esac
  case "$sub" in
    delete)
      [ -n "$creation" ] || continue
      [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] || continue
      if [ "$pending" = 1 ]; then
        run_pending_timeout run_locked "$id" "$creation" "$pending" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" || true
      else
        run_with_timeout run_locked "$id" "$creation" "$pending" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" || true
      fi
      ;;
    rm)
      [ -n "$creation" ] && continue
      case "$id" in *[!0-9a-f]*) continue ;; esac
      [ "${#id}" -eq 64 ] || continue
      run_with_timeout "$bin" "$sub" --force "$id" || true
      ;;
    *) continue ;;
  esac
done
`

const (
	maxReaperSpawnFailures       = 3
	defaultReaperPendingAttempts = 15
	defaultReaperWriteTimeout    = 500 * time.Millisecond
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

// reaperLockIdentityRE validates a recorded device:inode:uid barrier
// identity. It is data the child compares against a fresh stat, so only
// the digits and separators are accepted.
var reaperLockIdentityRE = regexp.MustCompile(`^[0-9]+:[0-9]+:[0-9]+$`)

// validReaperID keeps the backend's address space explicit. Docker's
// immutable IDs are 64 hex characters, while Apple Container addresses
// containers by its shorter name rule.
func validReaperID(subcommand, id string) bool {
	switch subcommand {
	case "delete":
		return nameRE.MatchString(id)
	case "rm":
		return dockerIDRE.MatchString(id)
	default:
		return false
	}
}

func validReaperLockPath(path string) bool {
	return path != "" && isAbsProtocolPath(path)
}

func isAbsProtocolPath(path string) bool {
	return len(path) > 0 && path[0] == '/' && !strings.ContainsAny(path, "\x00\n\r\t")
}

// reaper record states. A pending record is written before a create
// starts, an active record is a target the library has already verified,
// C confirms a pending record, and D withdraws it.
const (
	reaperStatePending = "P"
	reaperStateActive  = "A"
	reaperStateConfirm = "C"
	reaperStateDiscard = "D"
)

type reaperEntry struct {
	id       string
	creation string
	// pending marks a record written before the backend create ran. The
	// child retries its inspect for a pending record and never removes a
	// shared reuse generation for one.
	pending bool
	// lockPaths are the name barriers in the fixed acquisition order
	// used by lockName. Entries addressed by an immutable Docker ID leave
	// them empty.
	lockPaths []string
	lockIDs   []string
}

func (e reaperEntry) record(state string) reaperRecord {
	return reaperRecord{
		state:     state,
		id:        e.id,
		creation:  e.creation,
		lockPaths: e.lockPaths,
		lockIDs:   e.lockIDs,
	}
}

type reaperRecord struct {
	state     string
	id        string
	creation  string
	lockPaths []string
	lockIDs   []string
}

// encode renders one wire record: the state, the ID, the creation, and
// then each name barrier immediately followed by its recorded identity.
// Interleaving keeps the child's field parsing positional, so a
// truncated record fails the barrier-identity check instead of silently
// matching a neighbouring field.
func (record reaperRecord) encode() string {
	fields := make([]string, 0, 3+2*len(record.lockPaths))
	fields = append(fields, record.state, record.id, record.creation)
	for i, path := range record.lockPaths {
		fields = append(fields, path)
		if i < len(record.lockIDs) {
			fields = append(fields, record.lockIDs[i])
		}
	}
	return strings.Join(fields, "\t")
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu     sync.Mutex
	cmd    *exec.Cmd
	pgid   int
	stdin  io.WriteCloser
	exited chan struct{}
	// childExited records that the current child has been waited for, so
	// a replacement does not signal a process-group ID the kernel may
	// already have handed to an unrelated process. It is guarded by mu
	// because the child's exit state itself is written by the wait in
	// another goroutine.
	childExited   bool
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
	closed        bool
	// generation identifies the current child so a supervisor goroutine
	// from a replaced child cannot act on the new one.
	generation uint64
	// supervise restarts an unexpectedly exited child. Tests that kill
	// the child to exercise the registration respawn turn it off.
	supervise bool
	// writeTimeout bounds a single registration write so a wedged child
	// cannot block the caller indefinitely.
	writeTimeout time.Duration
	// pendingAttempts is the child's retry budget for a record written
	// before a create started.
	pendingAttempts int
	command         func() *exec.Cmd
}

func newReaper(binary, subcommand string) *reaper {
	r := &reaper{
		binary:          binary,
		subcommand:      subcommand,
		supervise:       true,
		writeTimeout:    defaultReaperWriteTimeout,
		pendingAttempts: defaultReaperPendingAttempts,
	}
	r.command = func() *exec.Cmd { return reaperCommand(binary, subcommand, r.pendingAttempts) }
	return r
}

// reaperCommand builds the watchdog child. pendingAttempts bounds how
// often the child re-inspects a record that was written before a create,
// so a create that is still settling can still be observed.
func reaperCommand(binary, subcommand string, pendingAttempts int) *exec.Cmd {
	if pendingAttempts <= 0 {
		pendingAttempts = defaultReaperPendingAttempts
	}
	return exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", binary, subcommand,
		breQuote(creationLabel), breQuote(managedLabel), breQuote(reuseLabel),
		strconv.Itoa(pendingAttempts))
}

// register adds a verified container target to the reaper's kill list,
// spawning or respawning the reaper process as needed. Apple targets are
// names and require one ownership generation: without a generation there
// is no safe ownership token for a name-addressed automatic delete. Docker
// targets are the full 64-hex ID printed by docker run and need no
// generation because a replacement never reuses that ID.
func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

// registerPending records a name and generation before the backend create
// starts. If this process dies while the create is still settling, the
// child keeps a bounded window to observe the eventual generation. A
// pending record never deletes a shared reuse generation, because a peer
// may have adopted it in the meantime.
func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

// confirm turns a pending record into an active one after the create
// returned and the target was verified.
func (r *reaper) confirm(id, creation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.cmd == nil {
		return nil
	}
	entry, ok := r.findLocked(id, creation)
	if !ok {
		return nil
	}
	entry.pending = false
	r.replaceLocked(entry)
	if err := r.writeRecordLocked(entry.record(reaperStateConfirm)); err != nil {
		return r.respawnAndReplayLocked()
	}
	r.spawnFailures = 0
	return nil
}

// discard withdraws a record whose create never ran, so the child does
// not spend its retry budget on a container that cannot exist.
func (r *reaper) discard(id, creation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	entry, ok := r.findLocked(id, creation)
	if !ok {
		return nil
	}
	r.removeLocked(entry)
	if r.cmd == nil {
		return nil
	}
	if err := r.writeRecordLocked(entry.record(reaperStateDiscard)); err != nil {
		return r.respawnAndReplayLocked()
	}
	r.spawnFailures = 0
	return nil
}

func (r *reaper) findLocked(id, creation string) (reaperEntry, bool) {
	for _, entry := range r.entries {
		if entry.id == id && entry.creation == creation {
			return entry, true
		}
	}
	return reaperEntry{}, false
}

func (r *reaper) replaceLocked(entry reaperEntry) {
	for i, existing := range r.entries {
		if existing.id == entry.id && existing.creation == entry.creation {
			r.entries[i] = entry
			return
		}
	}
	r.entries = append(r.entries, entry)
}

func (r *reaper) removeLocked(entry reaperEntry) {
	kept := r.entries[:0]
	for _, existing := range r.entries {
		if existing.id == entry.id && existing.creation == entry.creation {
			continue
		}
		kept = append(kept, existing)
	}
	r.entries = kept
}

func (r *reaper) registerEntry(entry reaperEntry) error {
	if err := r.validateEntry(entry); err != nil {
		return err
	}
	if r.subcommand != "delete" {
		// An immutable-ID entry is its own proof of identity.
		entry.creation = ""
		entry.lockPaths, entry.lockIDs = nil, nil
	}
	if r.subcommand == "delete" {
		paths, identities, err := reaperNameLockMetadata(entry.id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name barriers for %q: %w", entry.id, err)
		}
		entry.lockPaths, entry.lockIDs = paths, identities
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("reaper: registration after close")
	}
	if r.gaveUp {
		return errors.New("reaper: giving up after repeated spawn failures")
	}
	r.replaceLocked(entry)
	if r.stdin != nil {
		if err := r.writeRecordLocked(entry.record(entryState(entry))); err == nil {
			r.spawnFailures = 0
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func entryState(entry reaperEntry) string {
	if entry.pending {
		return reaperStatePending
	}
	return reaperStateActive
}

func (r *reaper) validateEntry(entry reaperEntry) error {
	if !validReaperID(r.subcommand, entry.id) {
		return fmt.Errorf("reaper: invalid container id %q", entry.id)
	}
	if entry.creation != "" && !creationRE.MatchString(entry.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", entry.creation)
	}
	if r.subcommand == "delete" && entry.creation == "" {
		return fmt.Errorf("reaper: Apple delete entry %q has no ownership generation", entry.id)
	}
	return nil
}

func (r *reaper) writeRecordLocked(record reaperRecord) error {
	if r.stdin == nil {
		return io.ErrClosedPipe
	}
	for _, path := range record.lockPaths {
		if !validReaperLockPath(path) {
			return fmt.Errorf("reaper: invalid name barrier %q for %q", path, record.id)
		}
	}
	for _, identity := range record.lockIDs {
		if !reaperLockIdentityRE.MatchString(identity) {
			return fmt.Errorf("reaper: invalid barrier identity %q for %q", identity, record.id)
		}
	}
	line := record.encode()
	// A wedged child must not block registration forever: a registration
	// is best effort insurance, and a failed write respawns the child and
	// replays the whole list.
	pipe, isPipe := r.stdin.(*os.File)
	if isPipe && r.writeTimeout > 0 {
		_ = pipe.SetWriteDeadline(time.Now().Add(r.writeTimeout))
	}
	n, err := io.WriteString(r.stdin, line+"\n")
	if isPipe && r.writeTimeout > 0 {
		_ = pipe.SetWriteDeadline(time.Time{})
	}
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

// respawnAndReplayLocked starts a fresh reaper process and re-registers
// every known record with it. Success resets the consecutive-failure
// count; giving up logs once so a permanently broken reaper is visible.
func (r *reaper) respawnAndReplayLocked() error {
	previous, previousPgid, previousExited := r.cmd, r.pgid, r.childExited
	r.cmd, r.stdin, r.exited, r.pgid = nil, nil, nil, 0
	r.childExited = false
	// Terminate whatever the replaced child left behind before a new child
	// can inherit the same pipe or compete for the same name barriers.
	terminateReaperProcess(previous, previousPgid, previousExited)
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			r.spawnFailures++
			continue
		}
		replayed := true
		for _, entry := range r.entries {
			if err := r.writeRecordLocked(entry.record(entryState(entry))); err != nil {
				replayed = false
				break
			}
		}
		if replayed {
			r.spawnFailures = 0
			return nil
		}
		r.spawnFailures++
	}
	if !r.gaveUp {
		r.gaveUp = true
		log.Printf("container-go: reaper giving up after %d consecutive spawn failures (binary=%q)", maxReaperSpawnFailures, r.binary)
	}
	return errors.New("reaper: giving up after repeated spawn failures")
}

func (r *reaper) spawnLocked() error {
	cmd := r.command()
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r.generation++
	generation := r.generation
	exited := make(chan struct{})
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
	r.pgid = reaperProcessGroupID(cmd)
	r.childExited = false
	if r.supervise {
		go r.superviseChild(cmd, exited, generation)
		return nil
	}
	// Without supervision nothing replaces the child, so a caller that
	// kills it to exercise the registration respawn owns that path.
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return nil
}

// superviseChild waits for the child and, unless this process is done
// with the reaper, replaces a child that exited unexpectedly and replays
// every record into it. The channel is closed before the mutex is taken
// so a caller already waiting on the child's exit is not blocked by the
// replacement.
func (r *reaper) superviseChild(cmd *exec.Cmd, exited chan struct{}, generation uint64) {
	_ = cmd.Wait()
	close(exited)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != cmd || r.generation != generation {
		return
	}
	r.childExited = true
	if r.closed {
		return
	}
	if err := r.respawnAndReplayLocked(); err != nil {
		log.Printf("container-go: reaper respawn failed (binary=%q): %v", r.binary, err)
	}
}

// closeStdin hands the reaper the same EOF it would see on parent death
// and stops supervision. Test hook and best-effort shutdown.
func (r *reaper) closeStdin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
}

// killForTest kills the reaper child and waits until it is reaped, so
// the next write deterministically fails. Supervision is disabled first
// so the registration path performs the respawn the caller wants to
// exercise.
func (r *reaper) killForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.supervise = false
	if r.cmd != nil {
		killReaperProcess(r.cmd, r.pgid)
		<-r.exited
	}
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

func withGlobalReaper(binary, subcommand string, fn func(*reaper) error) {
	// The reaper needs /bin/sh, so on Windows this is a no-op and cleanup
	// relies on the normal paths.
	if runtime.GOOS == "windows" {
		return
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	if !ok {
		r = newReaper(binary, subcommand)
		globalReapers[binary] = r
	}
	globalReapersMu.Unlock()
	if err := fn(r); err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
	}
}

// withExistingGlobalReaper runs fn only when a reaper for binary already
// exists, so withdrawing or confirming a record can never spawn a reaper
// on its own.
func withExistingGlobalReaper(binary, subcommand string, fn func(*reaper) error) {
	if runtime.GOOS == "windows" {
		return
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	globalReapersMu.Unlock()
	if !ok {
		return
	}
	if err := fn(r); err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
	}
}

// registerWithGlobalReaper best-effort registers a verified container
// target with the process-wide reaper for its backend binary. Reaper
// trouble is logged but never fails container startup.
func registerWithGlobalReaper(binary, subcommand, id, creation string) {
	withGlobalReaper(binary, subcommand, func(r *reaper) error {
		return r.register(id, creation)
	})
}

// registerPendingWithGlobalReaper records a name and generation before
// the backend create starts, so a crash during create still has a
// generation-guarded watchdog target.
func registerPendingWithGlobalReaper(binary, subcommand, id, creation string) {
	withGlobalReaper(binary, subcommand, func(r *reaper) error {
		return r.registerPending(id, creation)
	})
}

// confirmPendingWithGlobalReaper promotes a pending record to an active
// one after the create returned and the target was verified.
func confirmPendingWithGlobalReaper(binary, subcommand, id, creation string) {
	withExistingGlobalReaper(binary, subcommand, func(r *reaper) error {
		return r.confirm(id, creation)
	})
}

// discardPendingWithGlobalReaper withdraws a pending record whose create
// never ran.
func discardPendingWithGlobalReaper(binary, subcommand, id, creation string) {
	withExistingGlobalReaper(binary, subcommand, func(r *reaper) error {
		return r.discard(id, creation)
	})
}
