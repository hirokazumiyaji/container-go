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
	"time"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
// If the child exits unexpectedly while the parent is alive, the parent
// starts a replacement and replays every entry into it.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
managed_key="$4"
session_key="$5"
timeout_seconds="${6:-30}"
pending_attempts="${7:-30}"
case "$timeout_seconds" in ''|*[!0-9]*) timeout_seconds=30;; esac
case "$pending_attempts" in ''|*[!0-9]*) pending_attempts=30;; esac
[ "$timeout_seconds" -gt 0 ] 2>/dev/null || timeout_seconds=30
[ "$pending_attempts" -gt 0 ] 2>/dev/null || pending_attempts=1
# The default bound is sleep 30; timeout(1) is not available everywhere.
helper=$(mktemp 2>/dev/null) || exit 0
trap 'rm -f "$helper"' EXIT
cat >"$helper" <<'REAPER_LOCKED_HELPER'
#!/bin/sh
set -f
bin="$REAPER_BIN"
sub="$REAPER_SUB"
key="$REAPER_KEY"
managed_key="$REAPER_MANAGED_KEY"
session_key="$REAPER_SESSION_KEY"
timeout_seconds="$REAPER_TIMEOUT"
pending_attempts="$REAPER_PENDING_ATTEMPTS"
run_with_timeout() {
  seconds="$1"
  shift
  "$@" & command_pid=$!
  (sleep "$seconds"; kill -9 "$command_pid" 2>/dev/null || true) >/dev/null 2>&1 & killer_pid=$!
  wait "$command_pid" 2>/dev/null
  rc=$?
  kill -9 "$killer_pid" 2>/dev/null || true
  wait "$killer_pid" 2>/dev/null || true
  return "$rc"
}
inspect_projection() {
  {
    if [ "$sub" = rm ]; then
      "$bin" inspect --type=container "$1" 2>/dev/null
    else
      "$bin" inspect "$1" 2>/dev/null
    fi
    rc=$?
    printf '\n__containergo_reaper_rc__%s\n' "$rc"
  } | sed -n \
    -e "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/generation=\1/p" \
    -e 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/id=\1/p' \
    -e "s/^[[:space:]]*\"$managed_key\"[[:space:]]*:[[:space:]]*\"true\".*/managed=true/p" \
    -e "s/^[[:space:]]*\"$session_key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/session=\1/p" \
    -e 's/^[[:space:]]*"State"[[:space:]]*:.*/state_object=yes/p' \
    -e 's/^[[:space:]]*"state"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/state_object=yes\nstate=\1/p' \
    -e 's/^[[:space:]]*"Status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/state=\1/p' \
    -e 's/^[[:space:]]*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/state=\1/p' \
    -e 's/^__containergo_reaper_rc__\([0-9][0-9]*\)$/rc=\1/p'
}
file_identity() {
  stat -c '%d:%i' "$1" 2>/dev/null || stat -f '%d:%i' "$1" 2>/dev/null
}
lock_file_secure() {
  lf_path="$1"
  [ -n "$lf_path" ] && [ -f "$lf_path" ] && [ ! -L "$lf_path" ] || return 1
  lf_mode=$(stat -c '%a' "$lf_path" 2>/dev/null || stat -f '%Lp' "$lf_path" 2>/dev/null) || return 1
  lf_owner=$(stat -c '%u' "$lf_path" 2>/dev/null || stat -f '%u' "$lf_path" 2>/dev/null) || return 1
  lf_uid=$(id -u 2>/dev/null) || return 1
  [ "$lf_mode" = 600 ] || return 1
  [ "$lf_owner" = "$lf_uid" ] || return 1
}
valid_lock_identity() {
  case "$1" in
    *:*) ;;
    *) return 1 ;;
  esac
  case "$1" in
    *:*:*|*[!0-9:]*|:*|*:) return 1 ;;
  esac
  return 0
}
identities_valid() {
  [ "$#" -eq 0 ] && return 0
  path="$1"
  expected="$2"
  shift 2
  valid_lock_identity "$expected" || return 1
  [ -f "$path" ] && [ ! -L "$path" ] || return 1
  lock_file_secure "$path" || return 1
  actual=$(file_identity "$path") || return 1
  [ "$actual" = "$expected" ] || return 1
  identities_valid "$@"
}
stage=$1
p1=$2
i1=$3
p2=$4
i2=$5
p3=$6
i3=$7
id=$8
creation=$9
sub=${10}
pending=${11}
require_owner=${12}
if [ "$stage" -lt 3 ] && [ "$stage" -ge 0 ]; then
  case "$stage" in
    0) exec "$0" 1 "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" "$id" "$creation" "$sub" "$pending" "$require_owner" "$REAPER_SESSION" ;;
    1) exec "$REAPER_LOCKF" -k -n -t 30 -w "$p2" "$0" 2 "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" "$id" "$creation" "$sub" "$pending" "$require_owner" "$REAPER_SESSION" ;;
    2) exec "$REAPER_LOCKF" -k -n -t 30 -w "$p3" "$0" 3 "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" "$id" "$creation" "$sub" "$pending" "$require_owner" "$REAPER_SESSION" ;;
  esac
  exit 0
fi
if [ "$stage" -ge 0 ]; then
  identities_valid "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" || exit 0
fi
fields=$(run_with_timeout 10 inspect_projection "$id") || fields=
got=
uid=
managed=
got_session=
state=
state_object=
inspect_rc=
for field in $fields; do
  case "$field" in
    generation=*) got=${field#generation=} ;;
    id=*) uid=${field#id=} ;;
    managed=*) managed=${field#managed=} ;;
    session=*) got_session=${field#session=} ;;
    state=*) state=${field#state=} ;;
    state_object=*) state_object=${field#state_object=} ;;
    rc=*) inspect_rc=${field#rc=} ;;
  esac
done
tries=0
while [ "$inspect_rc" != 0 ] || { [ -n "$creation" ] && [ -z "$got" ]; }; do
  [ "$pending" = 1 ] || exit 0
  tries=$((tries + 1))
  [ "$tries" -lt "$pending_attempts" ] || exit 0
  sleep 1
  fields=$(run_with_timeout 10 inspect_projection "$id") || fields=
  got=; uid=; managed=; got_session=; state=; state_object=; inspect_rc=
  for field in $fields; do
    case "$field" in
      generation=*) got=${field#generation=} ;;
      id=*) uid=${field#id=} ;;
      managed=*) managed=${field#managed=} ;;
      session=*) got_session=${field#session=} ;;
      state=*) state=${field#state=} ;;
    state_object=*) state_object=${field#state_object=} ;;
      rc=*) inspect_rc=${field#rc=} ;;
    esac
  done
done
# A stale or mismatched generation is skipped. Docker rm additionally
# requires a container-shaped State and an ID that matches the requested
# target before it can derive an immutable rm target.
if [ "$sub" = rm ]; then
  [ "$state_object" = yes ] || exit 0
  case "$state" in
    created|running|paused|restarting|removing|exited|dead) ;;
    *) exit 0 ;;
  esac
  if [ -z "$creation" ]; then
    [ -n "$uid" ] && [ "$uid" = "$id" ] || exit 0
  else
    [ "$got" = "$creation" ] || exit 0
    [ -n "$uid" ] || exit 0
  fi
  target="$uid"
else
  [ "$got" = "$creation" ] || exit 0
  if [ "$sub" = delete ]; then
    [ -z "$uid" ] || exit 0
    if [ "$require_owner" = 1 ]; then
      [ "$managed" = true ] || exit 0
      [ -n "$REAPER_SESSION" ] || exit 0
      [ "$got_session" = "$REAPER_SESSION" ] || exit 0
      case "$state" in running|stopped|created) ;; *) exit 0 ;; esac
    fi
    target="$id"
  else
    [ -n "$uid" ] || exit 0
    target="$uid"
  fi
fi
if [ "$stage" -ge 0 ]; then
  identities_valid "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" || exit 0
fi
run_with_timeout "$timeout_seconds" "$bin" "$sub" --force "$target" >/dev/null 2>&1 || true
exit 0
REAPER_LOCKED_HELPER
chmod 700 "$helper" 2>/dev/null || exit 0

valid_lock_path() {
  lock_file_secure "$1"
}
valid_docker_id() {
  docker_id=$1
  [ "${#docker_id}" -eq 64 ] || return 1
  case "$docker_id" in *[!0-9a-f]*) return 1 ;; esac
  return 0
}
valid_lock_identity() {
  case "$1" in
    *:*) ;;
    *) return 1 ;;
  esac
  case "$1" in
    *:*:*|*[!0-9:]*|:*|*:) return 1 ;;
  esac
  return 0
}
lock_file_secure() {
  lf_path="$1"
  [ -n "$lf_path" ] && [ -f "$lf_path" ] && [ ! -L "$lf_path" ] || return 1
  lf_mode=$(stat -c '%a' "$lf_path" 2>/dev/null || stat -f '%Lp' "$lf_path" 2>/dev/null) || return 1
  lf_owner=$(stat -c '%u' "$lf_path" 2>/dev/null || stat -f '%u' "$lf_path" 2>/dev/null) || return 1
  lf_uid=$(id -u 2>/dev/null) || return 1
  [ "$lf_mode" = 600 ] || return 1
  [ "$lf_owner" = "$lf_uid" ] || return 1
}
run_with_timeout() {
  seconds="$1"
  shift
  "$@" & command_pid=$!
  (sleep "$seconds"; kill -9 "$command_pid" 2>/dev/null || true) >/dev/null 2>&1 & killer_pid=$!
  wait "$command_pid" 2>/dev/null
  rc=$?
  kill -9 "$killer_pid" 2>/dev/null || true
  wait "$killer_pid" 2>/dev/null || true
  return "$rc"
}
run_locked() {
  p1=$1; i1=$2; p2=$3; i2=$4; p3=$5; i3=$6; id=$7; creation=$8; pending=$9; owner=${10}; session_expected=${11}
  valid_lock_identity "$i1" && valid_lock_identity "$i2" && valid_lock_identity "$i3" || return 0
  valid_lock_path "$p1" && valid_lock_path "$p2" && valid_lock_path "$p3" || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  attempt=0
  while [ "$attempt" -lt 300 ]; do
    REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_MANAGED_KEY="$managed_key" REAPER_SESSION_KEY="$session_key" REAPER_TIMEOUT="$timeout_seconds" REAPER_PENDING_ATTEMPTS="$pending_attempts" REAPER_LOCKF="$lockf_bin" REAPER_SESSION="$session_expected" \
      "$lockf_bin" -k -n -t 30 -w "$p1" "$helper" 0 "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" "$id" "$creation" "$sub" "$pending" "$owner" "$session_expected" >/dev/null 2>&1
    rc=$?
    [ "$rc" -eq 0 ] && return 0
    [ "$rc" -eq 75 ] || return 0
    attempt=$((attempt + 1))
    sleep 0.1
  done
  return 0
}
run_unlocked() {
  id=$1; creation=$2; pending=$3; owner=$4; session=$5
  REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_MANAGED_KEY="$managed_key" REAPER_SESSION_KEY="$session_key" REAPER_TIMEOUT="$timeout_seconds" REAPER_PENDING_ATTEMPTS="$pending_attempts" REAPER_SESSION="$session" \
    "$helper" -1 "" "" "" "" "" "" "$id" "$creation" "$sub" "$pending" "$owner" "$session" >/dev/null 2>&1 || true
}
tab=$(printf '\t')
records=$(awk -F '\t' '
function key(id, gen) { return id SUBSEP gen }
$1 == "P" { k = key($2, $3); state[k] = "P"; record[k] = $0; next }
$1 == "C" { k = key($2, $3); if (k in record) { state[k] = "A"; sub(/^C\t/, "A\t", record[k]) } next }
$1 == "+" { k = key($2, $3); state[k] = "A"; record[k] = $0; next }
$1 == "-" { k = key($2, $3); delete state[k]; delete record[k]; next }
END { for (k in record) print record[k] }
')
printf '%s\n' "$records" | while IFS="$tab" read -r op id creation p1 i1 p2 i2 p3 i3 owner session; do
  [ -n "$id" ] || continue
  pending=0
  [ "$op" = P ] && pending=1
  case "$sub" in
    delete)
      [ -n "$creation" ] || continue
      [ -n "$p1" ] && [ -n "$i1" ] && [ -n "$p2" ] && [ -n "$i2" ] && [ -n "$p3" ] && [ -n "$i3" ] || continue
      run_locked "$p1" "$i1" "$p2" "$i2" "$p3" "$i3" "$id" "$creation" "$pending" "$owner" "$session"
      ;;
    rm)
      if [ -n "$creation" ]; then
        run_unlocked "$id" "$creation" "$pending" "$owner" "$session"
      else
        valid_docker_id "$id" || continue
        # Even immutable IDs are inspected as containers before rm. A
        # network or volume with a colliding ID must never be removed.
        run_unlocked "$id" "" "$pending" "$owner" "$session"
      fi
      ;;
    *) continue ;;
  esac
done
`

const (
	maxReaperSpawnFailures    = 3
	initialReaperSpawnBackoff = time.Second
	maxReaperSpawnBackoff     = 30 * time.Second
	// A child that exits this quickly is treated as a failed spawn, not
	// as a healthy replacement. This prevents a broken reaper command
	// from turning recovery into a tight respawn loop.
	immediateReaperExitWindow = time.Second
	// A short observation interval closes the race between writing the
	// replay records and a command that exits immediately afterwards.
	reaperReplacementVerification = 50 * time.Millisecond
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

var nameLockIdentityRE = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

type reaperEntry struct {
	id              string
	creation        string
	pending         bool
	lockPaths       []string
	lockIdentities  []string
	requireOwner    bool
	expectedSession string
}

type reaperRegistration struct {
	reaper *reaper
	entry  reaperEntry
	// alternate is set while a Docker name/generation record is being
	// promoted. If promotion fails after the immutable record has been
	// staged, unregister must cancel both possible identities.
	alternate reaperEntry
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	opMu                  sync.Mutex
	mu                    sync.Mutex
	cmd                   *exec.Cmd
	stdin                 io.WriteCloser
	exited                chan struct{}
	closed                bool
	entries               []reaperEntry
	completed             []reaperEntry
	spawnFailures         int
	gaveUp                bool
	retryAt               time.Time
	retryLevel            int
	retryDelay            time.Duration
	retryTimer            *time.Timer
	retryEpoch            uint64
	processStarted        time.Time
	immediateExitFailures int
	now                   func() time.Time
	command               func() *exec.Cmd
	backoff               func(int) time.Duration
	timeoutSeconds        int
	pendingAttempts       int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:          binary,
		subcommand:      subcommand,
		now:             time.Now,
		timeoutSeconds:  30,
		pendingAttempts: 30,
	}
}

func (r *reaper) validateEntry(entry reaperEntry) error {
	switch r.subcommand {
	case "delete":
		if !nameRE.MatchString(entry.id) || !creationRE.MatchString(entry.creation) {
			return fmt.Errorf("reaper: invalid Apple name/generation entry")
		}
		if entry.requireOwner && !creationRE.MatchString(entry.expectedSession) {
			return fmt.Errorf("reaper: owned entry requires a valid ownership session")
		}
		if !entry.requireOwner && entry.expectedSession != "" && !creationRE.MatchString(entry.expectedSession) {
			return fmt.Errorf("reaper: invalid ownership session %q", entry.expectedSession)
		}
		if len(entry.lockPaths) != 3 || len(entry.lockIdentities) != 3 {
			return fmt.Errorf("reaper: incomplete Apple lock set for %q", entry.id)
		}
		for i, path := range entry.lockPaths {
			if !validNameLockProtocolPath(path) || !nameLockIdentityRE.MatchString(entry.lockIdentities[i]) {
				return fmt.Errorf("reaper: invalid Apple lock metadata for %q", entry.id)
			}
		}
	case "rm":
		if dockerIDRE.MatchString(entry.id) {
			return nil
		}
		if !entry.pending || !nameRE.MatchString(entry.id) || !creationRE.MatchString(entry.creation) {
			return fmt.Errorf("reaper: invalid Docker name/generation entry")
		}
	default:
		return fmt.Errorf("reaper: unsupported delete subcommand %q", r.subcommand)
	}
	return nil
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. creation is the generation
// ID from creationLabel; empty is reserved for immutable container IDs.
func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

func (r *reaper) registerPendingOwned(id, creation, session string) error {
	if !creationRE.MatchString(session) {
		return fmt.Errorf("reaper: invalid ownership session %q", session)
	}
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true, requireOwner: true, expectedSession: session})
}

func (r *reaper) registerEntry(entry reaperEntry) error {
	immutable := r.subcommand == "rm" && dockerIDRE.MatchString(entry.id)
	switch r.subcommand {
	case "delete":
		if !nameRE.MatchString(entry.id) {
			return fmt.Errorf("reaper: invalid container name %q", entry.id)
		}
		if !creationRE.MatchString(entry.creation) {
			return fmt.Errorf("reaper: name-addressed entry %q requires a valid creation generation", entry.id)
		}
	case "rm":
		if immutable {
			entry.creation = ""
		} else {
			// Docker operations must never fall back to a mutable name.
			// A name/generation is accepted only for the short interval
			// before run returns its immutable ID; the entry is promoted
			// before it is retained on a Container.
			if !entry.pending {
				return fmt.Errorf("reaper: invalid container id %q: Docker requires a full immutable ID", entry.id)
			}
			if !nameRE.MatchString(entry.id) || !creationRE.MatchString(entry.creation) {
				return fmt.Errorf("reaper: invalid pending Docker name/generation %q", entry.id)
			}
		}
	default:
		return fmt.Errorf("reaper: unsupported delete subcommand %q", r.subcommand)
	}
	if !immutable && entry.creation != "" && !creationRE.MatchString(entry.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", entry.creation)
	}
	if entry.requireOwner && !creationRE.MatchString(entry.expectedSession) {
		return fmt.Errorf("reaper: owned entry requires a valid ownership session")
	}
	if !entry.requireOwner && entry.expectedSession != "" && !creationRE.MatchString(entry.expectedSession) {
		return fmt.Errorf("reaper: invalid ownership session %q", entry.expectedSession)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	if r.subcommand == "delete" && !immutable {
		paths, err := reaperNameLockPaths(entry.id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks: %w", err)
		}
		identities := make([]string, len(paths))
		for i, path := range paths {
			if !validNameLockProtocolPath(path) {
				return fmt.Errorf("reaper: invalid name lock path %q", path)
			}
			identity, err := nameLockIdentity(path)
			if err != nil {
				return fmt.Errorf("reaper: identify name lock: %w", err)
			}
			if !nameLockIdentityRE.MatchString(identity) {
				return fmt.Errorf("reaper: invalid name lock identity %q", identity)
			}
			identities[i] = identity
		}
		entry.lockPaths, entry.lockIdentities = paths, identities
	}
	if err := r.validateEntry(entry); err != nil {
		return err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
	}
	entry = r.upsertEntryLocked(entry)
	live := r.processLiveLocked()
	stdin := r.stdin
	ready := r.retryReadyLocked()
	r.mu.Unlock()
	if !ready {
		r.mu.Lock()
		r.armRetryLocked()
		err := r.spawnCooldownErrorLocked()
		r.mu.Unlock()
		return err
	}
	if live {
		if err := writeReaperRecordTo(stdin, entryRecord(entry, "+")); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.recoverAndReplayLocked()
}

//nolint:unused // retained for package-local compatibility helpers.
func (r *reaper) containsLocked(entry reaperEntry) bool {
	for _, active := range r.entries {
		if active.id == entry.id && active.creation == entry.creation {
			return true
		}
	}
	return false
}

func (r *reaper) upsertEntryLocked(entry reaperEntry) reaperEntry {
	for i, active := range r.entries {
		if active.id != entry.id || active.creation != entry.creation {
			continue
		}
		// Completion is monotonic: a late pending record must not
		// reintroduce retry semantics after the active record exists.
		if active.pending && !entry.pending {
			active.pending = false
			r.entries[i] = active
		}
		return r.entries[i]
	}
	r.entries = append(r.entries, entry)
	return entry
}

func (r *reaper) writeLocked(entry reaperEntry) error {
	if err := r.validateEntry(entry); err != nil {
		return err
	}
	return writeReaperRecordTo(r.stdin, entryRecord(entry, "+"))
}

func entryRecord(entry reaperEntry, operation string) string {
	if operation == "+" && entry.pending {
		operation = "P"
	}
	line := operation + "\t" + entry.id + "\t" + entry.creation
	for i, path := range entry.lockPaths {
		if i < len(entry.lockIdentities) {
			line += "\t" + path + "\t" + entry.lockIdentities[i]
		}
	}
	if entry.requireOwner {
		line += "\t1"
		if entry.expectedSession != "" {
			line += "\t" + entry.expectedSession
		}
	}
	return line + "\n"
}

func writeReaperRecordTo(stdin io.Writer, line string) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	n, err := io.WriteString(stdin, line)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	return nil
}

func (r *reaper) completePending(id, creation string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	found := -1
	for i, entry := range r.entries {
		if entry.pending && entry.id == id && entry.creation == creation {
			found = i
			entry.pending = false
			r.entries[i] = entry
			break
		}
	}
	live := r.processLiveLocked()
	stdin := r.stdin
	r.mu.Unlock()
	if found < 0 || !live {
		if found >= 0 {
			return r.recoverAndReplayLocked()
		}
		return nil
	}
	if err := writeReaperRecordTo(stdin, "C\t"+id+"\t"+creation+"\n"); err != nil {
		return r.recoverAndReplayLocked()
	}
	r.clearSpawnFailure()
	return nil
}

// promotePendingToDockerID replaces a completed Docker name/generation
// entry with the immutable ID returned by run. The ID is sent before the
// name cancellation so a failed write cannot leave the reaper with no
// target. The old name entry remains safe if the cancellation write fails:
// the rm branch still refuses to delete without a full inspect ID.
func (r *reaper) promotePendingToDockerID(name, creation, uid string) error {
	if r.subcommand != "rm" {
		return fmt.Errorf("reaper: immutable ID promotion requires the rm subcommand")
	}
	if !dockerIDRE.MatchString(uid) {
		return fmt.Errorf("reaper: invalid immutable Docker ID %q", uid)
	}
	if !nameRE.MatchString(name) || !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid pending Docker name/generation")
	}

	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
	}
	pendingIndex := -1
	idIndex := -1
	for i, entry := range r.entries {
		if entry.pending && entry.id == name && entry.creation == creation {
			pendingIndex = i
		}
		if entry.id == uid {
			idIndex = i
		}
	}
	if pendingIndex < 0 {
		r.mu.Unlock()
		return nil
	}
	if idIndex < 0 {
		r.entries = append(r.entries, reaperEntry{id: uid})
	}
	r.entries = append(r.entries[:pendingIndex], r.entries[pendingIndex+1:]...)
	live := r.processLiveLocked()
	stdin := r.stdin
	r.mu.Unlock()
	if live {
		if err := writeReaperRecordTo(stdin, entryRecord(reaperEntry{id: uid}, "+")); err == nil {
			if err := writeReaperRecordTo(stdin, "-\t"+name+"\t"+creation+"\n"); err == nil {
				r.clearSpawnFailure()
				return nil
			}
		}
	}
	return r.recoverAndReplayLocked()
}

func (r *reaper) unregister(id, creation string) error {
	entry := reaperEntry{id: id, creation: creation}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	removed := r.removeActiveLocked(entry)
	closed := r.closed
	if removed {
		r.completed = append(r.completed, entry)
		if len(r.completed) > 1024 {
			r.completed = r.completed[len(r.completed)-1024:]
		}
	}
	live := r.processLiveLocked()
	stdin := r.stdin
	r.mu.Unlock()
	if !removed {
		return nil
	}
	if closed {
		// EOF has already been delivered. Do not kill the child while it
		// is processing the buffered pending record; a later unregister
		// must not turn a bounded create race into an unobserved kill.
		return nil
	}
	if live {
		if err := writeReaperRecordTo(stdin, "-\t"+id+"\t"+creation+"\n"); err == nil {
			return nil
		}
	}
	return r.recoverAndReplayLocked()
}

func (r *reaper) removeActiveLocked(entry reaperEntry) bool {
	for i, active := range r.entries {
		if active.id == entry.id && active.creation == entry.creation {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return true
		}
	}
	return false
}

func (r *reaper) recoverAndReplayLocked() error {
	if cmd := r.currentProcess(); cmd != nil {
		if err := r.stopProcess(cmd); err != nil {
			return err
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
	}
	if len(r.entries) == 0 {
		r.mu.Unlock()
		r.clearSpawnFailure()
		return nil
	}
	if !r.retryReadyLocked() {
		r.armRetryLocked()
		err := r.spawnCooldownErrorLocked()
		r.mu.Unlock()
		return err
	}
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < maxReaperSpawnFailures; attempt++ {
		if err := r.spawnLocked(); err != nil {
			lastErr = err
			r.recordSpawnFailure()
			continue
		}
		replayed := true
		for _, entry := range entries {
			if err := r.writeLocked(entry); err != nil {
				lastErr, replayed = err, false
				break
			}
		}
		if replayed {
			// A successful write is not proof that the replacement is
			// usable. Verify the child before clearing the failure budget;
			// an immediately exiting command must remain on the backoff path.
			if r.replacementAlive() {
				r.clearSpawnFailure()
				return nil
			}
			lastErr = errors.New("reaper: child exited before replay verification")
		}
		if cmd := r.currentProcess(); cmd != nil {
			_ = r.stopProcess(cmd)
		}
		r.recordSpawnFailure()
	}
	if lastErr == nil {
		lastErr = errors.New("reaper: spawn failed")
	}
	return errors.Join(errReaperSpawnFailed, lastErr)
}

func (r *reaper) currentProcess() *exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd
}

func (r *reaper) spawnLocked() error {
	var cmd *exec.Cmd
	if r.command != nil {
		cmd = r.command()
	} else {
		timeout := r.timeoutSeconds
		if timeout <= 0 {
			timeout = 30
		}
		pending := r.pendingAttempts
		if pending <= 0 {
			pending = 1
		}
		cmd = exec.Command(
			"/bin/sh", "-c", reaperScript, "containergo-reaper",
			r.binary, r.subcommand, breQuote(creationLabel),
			breQuote(managedLabel), breQuote(sessionLabel), strconv.Itoa(timeout), strconv.Itoa(pending),
		)
	}
	if cmd == nil {
		return errors.New("reaper: nil spawn command")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	exited := make(chan struct{})
	r.mu.Lock()
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
	r.processStarted = time.Now()
	r.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	go r.monitorExit(cmd, exited)
	return nil
}

func (r *reaper) monitorExit(cmd *exec.Cmd, exited <-chan struct{}) {
	<-exited
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	current := !r.closed && r.cmd == cmd
	immediate := current && !r.processStarted.IsZero() && time.Since(r.processStarted) < immediateReaperExitWindow
	if current {
		// The child is already reaped. Clear the current identity so a
		// replacement can be installed without trying to stop the old one.
		r.cmd, r.stdin, r.exited = nil, nil, nil
		r.processStarted = time.Time{}
	}
	r.mu.Unlock()
	if current {
		if immediate {
			// An immediate unexpected exit is a failed replacement, even
			// when the record write itself succeeded. Count it before
			// recovery so repeated exits reach the bounded cooldown instead
			// of spinning.
			r.recordImmediateExit()
		} else {
			r.mu.Lock()
			r.immediateExitFailures = 0
			r.mu.Unlock()
		}
	}
	if current {
		if err := r.recoverAndReplayLocked(); err != nil {
			log.Printf("container-go: reaper recovery failed: %v", err)
		}
	}
}

func (r *reaper) processLiveLocked() bool {
	return r.stdin != nil && !channelClosed(r.exited)
}

func (r *reaper) replacementAlive() bool {
	deadline := time.Now().Add(reaperReplacementVerification)
	for {
		r.mu.Lock()
		alive := r.processLiveLocked()
		r.mu.Unlock()
		if !alive || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *reaper) stopProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	r.mu.Lock()
	if r.cmd != cmd {
		r.mu.Unlock()
		return nil
	}
	stdin, exited := r.stdin, r.exited
	r.cmd, r.stdin, r.exited = nil, nil, nil
	r.processStarted = time.Time{}
	r.mu.Unlock()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			return errors.New("reaper: child shutdown timed out")
		}
	}
	return nil
}

func (r *reaper) clearSpawnFailure() {
	r.mu.Lock()
	r.spawnFailures = 0
	r.gaveUp = false
	r.retryAt = time.Time{}
	r.retryLevel = 0
	r.retryDelay = 0
	r.retryEpoch++
	if r.retryTimer != nil {
		r.retryTimer.Stop()
		r.retryTimer = nil
	}
	r.mu.Unlock()
}

func (r *reaper) recordSpawnFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spawnFailures++
	r.enterCooldownIfNeededLocked()
}

func (r *reaper) recordImmediateExit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.immediateExitFailures++
	r.spawnFailures++
	if r.immediateExitFailures >= maxReaperSpawnFailures {
		r.spawnFailures = maxReaperSpawnFailures
	}
	r.enterCooldownIfNeededLocked()
}

func (r *reaper) enterCooldownIfNeededLocked() {
	if r.spawnFailures < maxReaperSpawnFailures {
		return
	}
	r.gaveUp = true
	if r.retryLevel < 32 {
		r.retryLevel++
	}
	delay := r.backoffDurationLocked(r.retryLevel)
	r.retryAt = r.nowLocked().Add(delay)
	r.retryDelay = delay
	r.armRetryLocked()
	log.Printf("container-go: reaper spawn failures; retrying after %s", delay)
}

// armRetryLocked schedules an autonomous recovery attempt. The caller
// holds r.mu; the callback takes opMu before touching the child.
func (r *reaper) armRetryLocked() {
	if r.closed || !r.gaveUp || r.retryAt.IsZero() {
		return
	}
	delay := r.retryDelay
	if delay < 0 {
		delay = 0
	}
	r.retryEpoch++
	epoch := r.retryEpoch
	if r.retryTimer != nil {
		r.retryTimer.Stop()
	}
	r.retryTimer = time.AfterFunc(delay, func() { r.retryWake(epoch) })
}

func (r *reaper) retryWake(epoch uint64) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed || epoch != r.retryEpoch || !r.gaveUp {
		r.mu.Unlock()
		return
	}
	r.retryTimer = nil
	// The timer is the autonomous deadline. Do not make the callback wait
	// on r.now: tests and clock adjustments can move that logical clock
	// independently, and the wake itself is the liveness signal.
	r.spawnFailures = 0
	r.gaveUp = false
	r.retryAt = time.Time{}
	r.retryDelay = 0
	r.mu.Unlock()
	if err := r.recoverAndReplayLocked(); err != nil {
		log.Printf("container-go: reaper autonomous recovery failed: %v", err)
	}
}

func (r *reaper) backoffDurationLocked(level int) time.Duration {
	if level < 1 {
		level = 1
	}
	var delay time.Duration
	if r.backoff != nil {
		delay = r.backoff(level)
	} else {
		delay = initialReaperSpawnBackoff
		for i := 1; i < level; i++ {
			if delay >= maxReaperSpawnBackoff/2 {
				delay = maxReaperSpawnBackoff
				break
			}
			delay *= 2
		}
	}
	if delay < 0 {
		return 0
	}
	if delay > maxReaperSpawnBackoff {
		return maxReaperSpawnBackoff
	}
	return delay
}

func (r *reaper) nowLocked() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *reaper) retryReadyLocked() bool {
	if !r.gaveUp {
		return true
	}
	if r.nowLocked().Before(r.retryAt) {
		return false
	}
	r.spawnFailures = 0
	r.gaveUp = false
	r.retryAt = time.Time{}
	r.retryDelay = 0
	return true
}

func (r *reaper) spawnCooldownErrorLocked() error {
	if r.retryAt.IsZero() {
		return errReaperSpawnCooldown
	}
	return fmt.Errorf("%w until %s", errReaperSpawnCooldown, r.retryAt.Format(time.RFC3339Nano))
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

// closeStdin hands the reaper the same EOF it would see on parent death.
func (r *reaper) closeStdin() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if !r.closed {
		r.closed = true
	}
	r.retryEpoch++
	if r.retryTimer != nil {
		r.retryTimer.Stop()
		r.retryTimer = nil
	}
	r.retryDelay = 0
	stdin := r.stdin
	r.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

func (r *reaper) killForTest() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if exited != nil {
		<-exited
	}
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

func getGlobalReaper(binary, subcommand string) *reaper {
	globalReapersMu.Lock()
	defer globalReapersMu.Unlock()
	r := globalReapers[binary]
	if r == nil {
		r = newReaper(binary, subcommand)
		globalReapers[binary] = r
	}
	return r
}

func registerContainerWithGlobalReaper(binary, subcommand, id, creation, session string, pending, owned bool) (*reaperRegistration, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	immutable := subcommand == "rm" && dockerIDRE.MatchString(id)
	if immutable {
		creation = ""
	}
	entry := reaperEntry{id: id, creation: creation, pending: pending, requireOwner: owned, expectedSession: session}
	r := getGlobalReaper(binary, subcommand)
	if r.subcommand != subcommand {
		err := fmt.Errorf("reaper: binary %q is already registered for %q, not %q", binary, r.subcommand, subcommand)
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
		return nil, err
	}
	var err error
	if pending {
		if owned {
			err = r.registerPendingOwned(id, creation, session)
		} else {
			err = r.registerPending(id, creation)
		}
	} else {
		err = r.registerEntry(entry)
	}
	// Capture the normalized entry, including prepared lock identities and
	// the pending marker. The registration handle is later used to cancel
	// exactly the record that was sent to the child. If a recovery attempt
	// failed after staging the entry, return the handle too; the caller can
	// then transactionally unregister it when create is not attempted.
	r.mu.Lock()
	found := false
	for _, active := range r.entries {
		if active.id == id && active.creation == creation {
			entry = active
			found = true
			break
		}
	}
	r.mu.Unlock()
	if err != nil {
		label := "registration"
		if pending {
			label = "pre-registration"
		}
		log.Printf("container-go: reaper %s failed (binary=%q): %v", label, binary, err)
		if found {
			return &reaperRegistration{reaper: r, entry: entry}, err
		}
		return nil, err
	}
	return &reaperRegistration{reaper: r, entry: entry}, nil
}

func registerWithGlobalReaper(binary, subcommand, id, creation string) error {
	_, err := registerContainerWithGlobalReaper(binary, subcommand, id, creation, sessionID(), false, subcommand == "delete")
	return err
}

//nolint:unused // retained for package-local compatibility helpers.
func preRegisterWithGlobalReaper(binary, subcommand, id, creation string) error {
	return preRegisterWithGlobalReaperForSession(binary, subcommand, id, creation, sessionID())
}

//nolint:unused // retained for package-local compatibility helpers.
func preRegisterWithGlobalReaperForSession(binary, subcommand, id, creation, session string) error {
	_, err := preRegisterWithGlobalReaperRegistration(binary, subcommand, id, creation, session)
	return err
}

func preRegisterWithGlobalReaperRegistration(binary, subcommand, id, creation, session string) (*reaperRegistration, error) {
	return registerContainerWithGlobalReaper(binary, subcommand, id, creation, session, true, subcommand == "delete")
}

func promotePendingDockerIDWithGlobalReaper(binary, name, creation, uid string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return errors.New("reaper: no pending registration")
	}
	if err := r.promotePendingToDockerID(name, creation, uid); err != nil {
		log.Printf("container-go: reaper immutable-ID promotion failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

//nolint:unused // retained for package-local compatibility helpers.
func completePreRegistrationWithGlobalReaper(binary, id, creation string) error {
	return completePendingWithGlobalReaper(binary, id, creation)
}

//nolint:unused // retained for package-local compatibility helpers.
func completePendingWithGlobalReaper(binary, id, creation string) error {
	return completePendingWithGlobalReaperFor(binary, "", id, creation)
}

func completePendingWithGlobalReaperFor(binary, subcommand, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return nil
	}
	if subcommand != "" && r.subcommand != subcommand {
		return fmt.Errorf("reaper: binary %q is registered for %q, not %q", binary, r.subcommand, subcommand)
	}
	if err := r.completePending(id, creation); err != nil {
		log.Printf("container-go: reaper create-completion update failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func unregisterWithGlobalReaper(registration *reaperRegistration) {
	if registration == nil || registration.reaper == nil {
		return
	}
	entries := []reaperEntry{registration.entry}
	if registration.alternate.id != "" &&
		(registration.alternate.id != registration.entry.id || registration.alternate.creation != registration.entry.creation) {
		entries = append(entries, registration.alternate)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := entry.id + "\x00" + entry.creation
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := registration.reaper.unregister(entry.id, entry.creation); err != nil {
			log.Printf("container-go: reaper unregister %s: %v", entry.id, err)
		}
	}
}
