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
	"time"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated as an Apple Container name or a full Docker ID, and the script
// itself disables globbing and quotes every expansion the IDs reach.
// Every backend entry runs behind a bounded timeout. The timeout covers
// the complete inspect, status-marker, filter, and delete pipeline; it
// uses a bounded child-process cleanup path without signaling a
// possibly recycled process-group ID. Inspect
// output is streamed through a field filter instead of being staged on
// disk. Failures stay silent (|| true) by design: the reaper is
// last-resort insurance.
// When a creation generation is known, the script inspects first and
// reads the creation label as a structural JSON field: the match is
// anchored at line start on the quoted key, so label values or other
// text containing the same characters cannot satisfy it. Inspect output
// is filtered before command substitution, so only the generation, ID,
// and status are held in memory; it is never staged in a host file, so
// a killed reaper cannot leave an environment-bearing inspect dump behind.
// When inspect also reports an immutable "Id" (Docker), the delete
// targets that ID instead of the name, so a same-name replacement
// created after the check is simply not found.
// Apple Container has no such ID; there the delete necessarily goes by
// name.
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
pgrep_bin=$(command -v pgrep 2>/dev/null) || pgrep_bin=
case "$pgrep_bin" in
  /*) [ -x "$pgrep_bin" ] || pgrep_bin= ;;
  *) pgrep_bin= ;;
esac
ps_bin=$(command -v ps 2>/dev/null) || ps_bin=
case "$ps_bin" in
  /*) [ -x "$ps_bin" ] || ps_bin= ;;
  *) ps_bin= ;;
esac
stat_bin=$(command -v stat 2>/dev/null) || stat_bin=
case "$stat_bin" in
  /*) [ -x "$stat_bin" ] || stat_bin= ;;
  *) stat_bin= ;;
esac
ids=""
while IFS= read -r line; do
  case "$line" in
    R#containergo-retire:*)
      # A retirement barrier acknowledges that all preceding D records are
      # in the child's private snapshot. It does not execute that snapshot;
      # EOF remains the only trigger for backend deletion.
      printf 'containergo-reaper-ack:%s\n' "${line#R#containergo-retire:}"
      continue
      ;;
    Q#containergo-quiesce:*)
      # Stop before EOF so an unrelated respawn cannot execute the still-live
      # entries that the replacement child will replay.
      printf 'containergo-reaper-ack:%s\n' "${line#Q#containergo-quiesce:}"
      exit 0
      ;;
  esac
  ids="$ids
$line"
done
kill_owned_pid() {
  pid="$1"
  owner="$2"
  signal="$3"
  if [ -n "$owner" ]; then
    current=$("$ps_bin" -o lstart= -p "$pid" 2>/dev/null) || return 0
    [ "$current" = "$owner" ] || return 0
  else
    kill -0 "$pid" 2>/dev/null || return 0
  fi
  kill "-$signal" "$pid" 2>/dev/null || true
}
bounded_pgrep() {
  parent="$1"
  "$pgrep_bin" -P "$parent" 2>/dev/null & lookup="$!"
  lookup_owner=$("$ps_bin" -o lstart= -p "$lookup" 2>/dev/null) || lookup_owner=""
  (sleep 1; kill_owned_pid "$lookup" "$lookup_owner" 9) & lookup_timer="$!"
  wait "$lookup" 2>/dev/null
  status=$?
  timer_owner=$("$ps_bin" -o lstart= -p "$lookup_timer" 2>/dev/null) || timer_owner=""
  kill_owned_pid "$lookup_timer" "$timer_owner" TERM
  wait "$lookup_timer" 2>/dev/null || true
  return "$status"
}
kill_owned_descendants() {
  parent="$1"
  depth="$2"
  [ "$depth" -lt 8 ] 2>/dev/null || return 0
  children=$(bounded_pgrep "$parent") || return 0
  for child in $children; do
    case "$child" in
      ''|*[!0-9]*) continue ;;
    esac
    descendant_count=$((descendant_count + 1))
    [ "$descendant_count" -le 256 ] 2>/dev/null || return 0
    owner=$("$ps_bin" -o lstart= -p "$child" 2>/dev/null) || continue
    [ -n "$owner" ] || continue
    current=$("$ps_bin" -o lstart= -p "$child" 2>/dev/null) || continue
    [ "$current" = "$owner" ] || continue
    kill_owned_descendants "$child" "$((depth + 1))"
    current=$("$ps_bin" -o lstart= -p "$child" 2>/dev/null) || continue
    [ "$current" = "$owner" ] || continue
    kill -9 "$child" 2>/dev/null || true
  done
}
kill_pipeline() {
  pid="$1"
  owner="$2"
  descendant_count=0
  # A numeric PID can be reused after the command exits. Match the
  # process start identity captured at launch before signaling the
  # positive PID; never signal a process-group ID.
  if [ -n "$owner" ]; then
    current=$("$ps_bin" -o lstart= -p "$pid" 2>/dev/null) || return 0
    [ "$current" = "$owner" ] || return 0
    kill_owned_descendants "$pid" 0
    current=$("$ps_bin" -o lstart= -p "$pid" 2>/dev/null) || return 0
    [ "$current" = "$owner" ] || return 0
  fi
  kill_owned_pid "$pid" "$owner" 9
}
run_with_timeout() {
  "$@" & pid=$!
  owner=$("$ps_bin" -o lstart= -p "$pid" 2>/dev/null) || owner=""
  (sleep "$timeout"; kill_pipeline "$pid" "$owner") & killer=$!
  killer_owner=$("$ps_bin" -o lstart= -p "$killer" 2>/dev/null) || killer_owner=""
  wait "$pid" 2>/dev/null
  rc=$?
  kill_owned_pid "$killer" "$killer_owner" TERM
  wait "$killer" 2>/dev/null || true
  return "$rc"
}
locked_command='
  stat_bin="$REAPER_STAT_BIN"
  stat_identity() {
    "$stat_bin" -c '\''%d:%i:%u'\'' "$1" 2>/dev/null || "$stat_bin" -f '\''%d:%i:%u'\'' "$1" 2>/dev/null
  }
  verify_lock_identity() {
    path=$1
    expected=$2
    [ -n "$path" ] && [ -n "$expected" ] || return 1
    [ -L "$path" ] && return 1
    [ -f "$path" ] || return 1
    actual=$(stat_identity "$path") || return 1
    [ "$actual" = "$expected" ] || return 1
    return 0
  }
  target=$1
  id=$2
  creation=$3
  pending=$4
  lock1=$5
  lockid1=$6
  lock2=$7
  lockid2=$8
  lock3=$9
  lockid3=${10}
  bin=$REAPER_BIN
  sub=$REAPER_SUB
  key=$REAPER_KEY
  pending_attempts=${REAPER_PENDING_ATTEMPTS:-30}
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  if [ -n "$creation" ]; then
    tries=0
    while :; do
      inspect_fields=$(
        {
          "$bin" inspect "$id" 2>/dev/null
          printf "\n__containergo_inspect_rc__%s\n" "$?"
        } | sed -n \
          -e "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/creation=\1/p" \
          -e "s/^[[:space:]]*\"Id\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/id=\1/p" \
          -e "s/^__containergo_inspect_rc__\([0-9][0-9]*\)$/inspect_rc=\1/p"
      ) || inspect_fields=""
      got=$(printf "%s\n" "$inspect_fields" | sed -n "s/^creation=//p" | head -n 1)
      uid=$(printf "%s\n" "$inspect_fields" | sed -n "s/^id=//p" | head -n 1)
      inspect_rc=$(printf "%s\n" "$inspect_fields" | sed -n "s/^inspect_rc=//p" | tail -n 1)
      unset inspect_fields
      if [ "$inspect_rc" = 0 ] && [ "$got" = "$creation" ]; then
        if [ "$sub" = "rm" ]; then
          case "$uid" in
            *[!0-9a-f]*) exit 0 ;;
          esac
          [ "${#uid}" -eq 64 ] 2>/dev/null || exit 0
        fi
        [ -n "$uid" ] && target="$uid"
        break
      fi
      [ "$pending" = 1 ] || exit 0
      tries=$((tries + 1))
      [ "$tries" -lt "$pending_attempts" ] || exit 0
      sleep 1
    done
  fi
  verify_lock_identity "$lock1" "$lockid1" || exit 0
  verify_lock_identity "$lock2" "$lockid2" || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  "$bin" "$sub" --force "$target" >/dev/null 2>&1 || true
'
lock_three_command='
  stat_bin="$REAPER_STAT_BIN"
  stat_identity() {
    "$stat_bin" -c '\''%d:%i:%u'\'' "$1" 2>/dev/null || "$stat_bin" -f '\''%d:%i:%u'\'' "$1" 2>/dev/null
  }
  verify_lock_identity() {
    path=$1
    expected=$2
    [ -n "$path" ] && [ -n "$expected" ] || return 1
    [ -L "$path" ] && return 1
    [ -f "$path" ] || return 1
    actual=$(stat_identity "$path") || return 1
    [ "$actual" = "$expected" ] || return 1
    return 0
  }
  lockf_bin=${1}
  lock3=${2}
  lockid3=${3}
  shift 3
  [ -n "$lock3" ] || exit 0
  [ -L "$lock3" ] && exit 0
  [ -f "$lock3" ] || exit 0
  verify_lock_identity "$lock3" "$lockid3" || exit 0
  id=$1
  creation=$2
  pending=$3
  lock1=$4
  lockid1=$5
  lock2=$6
  lockid2=$7
  lock3=$8
  lockid3=$9
  "$lockf_bin" -k -t 30 "$lock3" sh -c "$REAPER_LOCKED_COMMAND" \
    reaper-locked "$id" "$id" "$creation" "$pending" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3"
'
valid_lock_path() {
  [ -n "$1" ] || return 1
  [ -L "$1" ] && return 1
  [ -f "$1" ] || return 1
  return 0
}
stat_identity() {
  "$stat_bin" -c '%d:%i:%u' "$1" 2>/dev/null || "$stat_bin" -f '%d:%i:%u' "$1" 2>/dev/null
}
verify_lock_identity() {
  path=$1
  expected=$2
  valid_lock_path "$path" || return 1
  [ -n "$expected" ] || return 1
  actual=$(stat_identity "$path") || return 1
  [ "$actual" = "$expected" ] || return 1
  return 0
}
run_locked() {
  id="$1"
  creation="$2"
  lock1="$3"
  lockid1="$4"
  lock2="$5"
  lockid2="$6"
  lock3="$7"
  lockid3="$8"
  pending="$9"
  verify_lock_identity "$lock1" "$lockid1" || return 0
  verify_lock_identity "$lock2" "$lockid2" || return 0
  verify_lock_identity "$lock3" "$lockid3" || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  case "$lockf_bin" in
    /*) [ -x "$lockf_bin" ] || return 0 ;;
    *) return 0 ;;
  esac
  REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
    REAPER_PENDING_ATTEMPTS="$pending_attempts" REAPER_STAT_BIN="$stat_bin" \
    REAPER_LOCKED_COMMAND="$locked_command" REAPER_LOCK_THREE_COMMAND="$lock_three_command" \
    "$lockf_bin" -k -t 30 "$lock1" sh -c '
      stat_bin="$REAPER_STAT_BIN"
      stat_identity() {
        "$stat_bin" -c '\''%d:%i:%u'\'' "$1" 2>/dev/null || "$stat_bin" -f '\''%d:%i:%u'\'' "$1" 2>/dev/null
      }
      verify_lock_identity() {
        path=$1
        expected=$2
        [ -n "$path" ] && [ -n "$expected" ] || return 1
        [ -L "$path" ] && return 1
        [ -f "$path" ] || return 1
        actual=$(stat_identity "$path") || return 1
        [ "$actual" = "$expected" ] || return 1
        return 0
      }
      lockf_bin=${1}
      lock2=${2}
      lockid2=${3}
      lock3=${4}
      lockid3=${5}
      shift 5
      verify_lock_identity "$lock2" "$lockid2" || exit 0
      verify_lock_identity "$lock3" "$lockid3" || exit 0
      "$lockf_bin" -k -t 30 "$lock2" sh -c "$REAPER_LOCK_THREE_COMMAND" \
        reaper-lock-two "$lockf_bin" "$lock3" "$lockid3" "$@"
    ' reaper-lock "$lockf_bin" "$lock2" "$lockid2" "$lock3" "$lockid3" "$id" "$creation" "$pending" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" >/dev/null 2>&1 || true
  verify_lock_identity "$lock1" "$lockid1" || return 0
  verify_lock_identity "$lock2" "$lockid2" || return 0
  verify_lock_identity "$lock3" "$lockid3" || return 0
}
records=$(printf "%s\n" "$ids" | awk -F '\t' '
  $1 == "P" { key=$2 SUBSEP $3; line[key]=$0; next }
  $1 == "A" { key=$2 SUBSEP $3; line[key]=$0; next }
  $1 == "C" { key=$2 SUBSEP $3; if (key in line) sub(/^P\t/, "A\t", line[key]); next }
  $1 == "D" { key=$2 SUBSEP $3; delete line[key]; next }
  NF >= 1 { key=$1 SUBSEP $2; line[key]=$0 }
  END { for (key in line) print line[key] }
')
printf "%s\n" "$records" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  tab=$(printf '\t')
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
    *"$tab"*) creation=${rest%%"$tab"*}; rest=${rest#*"$tab"}; lock1=${rest%%"$tab"*}; rest=${rest#*"$tab"}; lockid1=${rest%%"$tab"*}; rest=${rest#*"$tab"}; lock2=${rest%%"$tab"*}; rest=${rest#*"$tab"}; lockid2=${rest%%"$tab"*}; rest=${rest#*"$tab"}; lock3=${rest%%"$tab"*}; lockid3=${rest#*"$tab"}; case "$lockid3" in *"$tab"*) continue ;; esac ;;
    *) creation="" ;;
  esac
  pending=0
  [ "$state" = P ] && pending=1
  case "$sub" in
    delete)
      [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] || continue
      run_with_timeout run_locked "$id" "$creation" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" "$pending" || true
      ;;
    rm)
      if [ -n "$creation" ]; then
        [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] || continue
        run_with_timeout run_locked "$id" "$creation" "$lock1" "$lockid1" "$lock2" "$lockid2" "$lock3" "$lockid3" "$pending" || true
      else
        case "$id" in *[!0-9a-f]*) continue ;; esac
        [ "${#id}" -eq 64 ] || continue
        run_with_timeout "$bin" "$sub" --force "$id" || true
      fi
      ;;
    *) continue ;;
  esac
done
`

const (
	maxReaperSpawnFailures       = 3
	maxReaperEntries             = 256
	reaperAckMarker              = "containergo-reaper-ack"
	defaultReaperTimeoutSeconds  = 30
	defaultReaperPendingAttempts = 30
	defaultReaperWriteTimeout    = 250 * time.Millisecond
	reaperCleanupTimeout         = 2 * time.Second
)

var reaperBackoff = 100 * time.Millisecond

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
var reaperLockIdentityRE = regexp.MustCompile(`^[0-9]+:[0-9]+:[0-9]+$`)

func validReaperLockIdentity(identity string) bool {
	return reaperLockIdentityRE.MatchString(identity)
}

type reaperEntry struct {
	id        string
	creation  string
	pending   bool
	lockPaths []string
	lockIDs   []string
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	opMu            sync.Mutex
	mu              sync.Mutex
	lifecycleMu     sync.Mutex
	reaping         bool
	stopping        bool
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	exited          chan struct{}
	ack             io.ReadCloser
	entries         []reaperEntry
	retiredPending  bool
	retirementAcked bool
	spawnFailures   int
	gaveUp          bool
	closed          bool
	// timeoutSeconds and pendingAttempts are internal test seams; production
	// reapers use bounded values for a create that may still be settling.
	timeoutSeconds  int
	pendingAttempts int
	writeTimeout    time.Duration
	command         func() *exec.Cmd
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:          binary,
		subcommand:      subcommand,
		timeoutSeconds:  defaultReaperTimeoutSeconds,
		pendingAttempts: defaultReaperPendingAttempts,
		writeTimeout:    defaultReaperWriteTimeout,
	}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. Name-addressed entries carry
// a valid generation and all three stable lock barriers; only a full
// Docker ID may omit the generation.
func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

// registerPending records a name/generation before the backend create
// starts. If the parent dies while the create is still settling, EOF
// gives the reaper a bounded window to observe the eventual generation.
func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

func (r *reaper) makeRoomLocked(needed int) error {
	excess := len(r.entries) + needed - maxReaperEntries
	if excess <= 0 {
		return nil
	}
	if excess > len(r.entries) {
		return errors.New("reaper: entry capacity is exhausted")
	}
	if r.stdin != nil && !channelClosed(r.exited) {
		for i := 0; i < excess; i++ {
			if err := writeReaperCancellation(r.stdin, r.entries[i]); err != nil {
				return err
			}
		}
		if err := r.acknowledgeRetirementLocked(); err != nil {
			return err
		}
		r.retiredPending = true
		r.retirementAcked = true
	}
	r.entries = r.entries[excess:]
	return nil
}

func (r *reaper) registerEntry(entry reaperEntry) error {
	immutableID := r.subcommand == "rm" && dockerIDRE.MatchString(entry.id)
	if !nameRE.MatchString(entry.id) && !immutableID {
		return fmt.Errorf("reaper: invalid container id %q", entry.id)
	}
	if entry.creation != "" && !creationRE.MatchString(entry.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", entry.creation)
	}
	if entry.pending && entry.creation == "" {
		return errors.New("reaper: pending entry requires a creation id")
	}
	if immutableID {
		entry.creation = ""
		entry.pending = false
	} else if entry.creation == "" {
		return fmt.Errorf("reaper: name-addressed entry %q requires a creation generation", entry.id)
	}
	if !immutableID {
		paths, identities, err := reaperNameLockMetadata(entry.id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", entry.id, err)
		}
		if len(paths) != 3 || len(identities) != len(paths) {
			return fmt.Errorf("reaper: got %d name lock barriers for %q, want 3", len(paths), entry.id)
		}
		for i, path := range paths {
			if !validNameLockProtocolPath(path) || !validReaperLockIdentity(identities[i]) {
				return fmt.Errorf("reaper: invalid stable name lock metadata %q", path)
			}
		}
		entry.lockPaths = paths
		entry.lockIDs = identities
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("reaper: closed")
	}
	duplicate := false
	for i, existing := range r.entries {
		if existing.id == entry.id && existing.creation == entry.creation {
			if existing.pending && !entry.pending {
				entry.lockPaths = existing.lockPaths
				entry.lockIDs = existing.lockIDs
				r.entries[i] = entry
			} else if !existing.pending && entry.pending {
				entry = existing
				duplicate = true
			} else {
				duplicate = true
			}
			break
		}
	}
	if !duplicate {
		if err := r.makeRoomLocked(1); err != nil {
			return err
		}
		r.entries = append(r.entries, entry)
	}
	if r.stdin != nil {
		if r.writeLocked(entry) == nil {
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func writeReaperLine(w io.Writer, line string, timeout time.Duration) error {
	if w == nil {
		return io.ErrClosedPipe
	}
	if timeout <= 0 {
		timeout = defaultReaperWriteTimeout
	}
	type writeDeadliner interface {
		SetWriteDeadline(time.Time) error
	}
	f, ok := w.(writeDeadliner)
	if !ok {
		return errors.New("reaper: registration pipe has no write deadline")
	}
	if err := f.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("reaper: set registration pipe deadline: %w", err)
	}
	defer func() { _ = f.SetWriteDeadline(time.Time{}) }()
	n, err := io.WriteString(w, line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) writeLocked(e reaperEntry) error {
	if r.stdin == nil {
		return io.ErrClosedPipe
	}
	state := "A"
	if e.pending {
		state = "P"
	}
	line := state + "\t" + e.id
	if e.creation != "" {
		line += "\t" + e.creation
	}
	if len(e.lockPaths) != 0 && len(e.lockIDs) != len(e.lockPaths) {
		return errors.New("reaper: lock path metadata is incomplete")
	}
	for i, path := range e.lockPaths {
		if !validNameLockProtocolPath(path) || !validReaperLockIdentity(e.lockIDs[i]) {
			return fmt.Errorf("reaper: invalid stable name lock metadata %q", path)
		}
		line += "\t" + path + "\t" + e.lockIDs[i]
	}
	line += "\n"
	return writeReaperLine(r.stdin, line, r.writeTimeout)
}

func acknowledgeReaperCommand(_ context.Context, stdin io.Writer, ack io.Reader, prefix string, writeTimeout time.Duration) error {
	if stdin == nil || ack == nil {
		return errors.New("reaper: acknowledgement channel is unavailable")
	}
	token := newCreationID()
	command := prefix + token
	if err := writeReaperLine(stdin, command+"\n", writeTimeout); err != nil {
		return fmt.Errorf("reaper: write acknowledgement barrier: %w", err)
	}
	if deadline, ok := ack.(interface{ SetReadDeadline(time.Time) error }); ok {
		if err := deadline.SetReadDeadline(time.Now().Add(reaperCleanupTimeout)); err != nil {
			return fmt.Errorf("reaper: set acknowledgement deadline: %w", err)
		}
		defer func() { _ = deadline.SetReadDeadline(time.Time{}) }()
	}
	expected := reaperAckMarker + ":" + token + "\n"
	buf := make([]byte, len(expected))
	if _, err := io.ReadFull(ack, buf); err != nil {
		return fmt.Errorf("reaper: command was not acknowledged: %w", err)
	}
	if string(buf) != expected {
		return fmt.Errorf("reaper: invalid acknowledgement %q", buf)
	}
	return nil
}

func (r *reaper) acknowledgeCommandLocked(prefix string) error {
	if channelClosed(r.exited) {
		return errors.New("reaper: acknowledgement channel is unavailable")
	}
	return acknowledgeReaperCommand(context.Background(), r.stdin, r.ack, prefix, r.writeTimeout)
}

func (r *reaper) acknowledgeRetirementLocked() error {
	return r.acknowledgeCommandLocked("R#containergo-retire:")
}

func (r *reaper) unregisterHandoff(name, uid string) error {
	validName := nameRE.MatchString(name)
	validUID := r.subcommand == "rm" && dockerIDRE.MatchString(uid)
	if !validName && !validUID {
		return fmt.Errorf("reaper: invalid handoff identity %q/%q", name, uid)
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.entries = nil
		r.retiredPending = false
		r.retirementAcked = false
		return nil
	}
	kept := make([]reaperEntry, 0, len(r.entries))
	removed := make([]reaperEntry, 0)
	for _, entry := range r.entries {
		if entry.id == name || (uid != "" && entry.id == uid) {
			removed = append(removed, entry)
		} else {
			kept = append(kept, entry)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	// Write cancellation records and wait for the child to acknowledge that
	// they are in its private snapshot. EOF remains the only delete trigger;
	// keeping the pipe open (or waiting for an acknowledged empty child exit)
	// prevents a stale pre-cancellation snapshot from being replayed.
	if r.stdin != nil && !channelClosed(r.exited) {
		for _, entry := range removed {
			if err := writeReaperCancellation(r.stdin, entry); err != nil {
				return err
			}
		}
		if err := r.acknowledgeRetirementLocked(); err != nil {
			return err
		}
		r.retiredPending = true
		r.retirementAcked = true
	} else {
		r.retiredPending = false
		r.retirementAcked = false
	}
	r.entries = kept
	if len(kept) == 0 {
		return r.retireEmptyChildLocked()
	}
	return nil
}

// retireEmptyChildLocked closes the pipe only after all cancellation records
// for the last entries have been written. The child reads the complete
// stream before applying it, so EOF cannot replay a handed-off generation.
// The caller holds opMu and r.mu.
func (r *reaper) retireEmptyChildLocked() error {
	stdin, cmd, exited, ack := r.stdin, r.cmd, r.exited, r.ack
	if stdin == nil || cmd == nil {
		r.retiredPending = false
		r.retirementAcked = false
		return nil
	}
	if exited == nil {
		return errors.New("reaper: retired child has no exit channel")
	}
	r.stdin = nil
	r.ack = nil
	_ = stdin.Close()
	r.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cancel()
	var waitErr error
	select {
	case <-exited:
	case <-waitCtx.Done():
		waitErr = fmt.Errorf("reaper: retired child did not exit: %w", waitCtx.Err())
	}
	if ack != nil {
		_ = ack.Close()
	}
	r.mu.Lock()
	if waitErr == nil || r.cmd != cmd {
		if r.cmd == cmd {
			r.cmd = nil
			r.exited = nil
		}
		r.retiredPending = false
		r.retirementAcked = false
		return waitErr
	}
	// Keep the exited child discoverable for a later bounded retry. Its
	// stdin is already closed, so a caller cannot append new entries to the
	// stale stream.
	r.cmd = cmd
	r.stdin = stdin
	r.exited = exited
	r.ack = ack
	return waitErr
}

func writeReaperCompletion(stdin io.Writer, e reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "C\t" + e.id
	if e.creation != "" {
		line += "\t" + e.creation
	}
	line += "\n"
	return writeReaperLine(stdin, line, defaultReaperWriteTimeout)
}

// completePending marks a pre-registered generation as completed. The
// active entry remains registered so EOF still deletes the exact
// generation if the parent later dies.
func (r *reaper) completePending(id, creation string) error {
	entry := reaperEntry{id: id, creation: creation, pending: true}
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
	found := -1
	for i, active := range r.entries {
		if active.pending && active.id == id && active.creation == creation {
			found = i
			break
		}
	}
	if found < 0 {
		r.mu.Unlock()
		return nil
	}
	entry.lockPaths = r.entries[found].lockPaths
	entry.lockIDs = r.entries[found].lockIDs
	entry.pending = false
	r.entries[found] = entry
	if r.stdin != nil {
		if err := writeReaperCompletion(r.stdin, entry); err == nil {
			r.mu.Unlock()
			return nil
		}
	}
	r.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.respawnAndReplayLocked()
}

func writeReaperCancellation(stdin io.Writer, e reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "D\t" + e.id
	if e.creation != "" {
		line += "\t" + e.creation
	}
	line += "\n"
	return writeReaperLine(stdin, line, defaultReaperWriteTimeout)
}

// promotePendingToDockerID replaces a completed Docker create's
// name/generation insurance with its immutable ID. The ID record is
// written first; cancellation is sent only after that succeeds so a
// parent death cannot lose the only remaining target.
func (r *reaper) promotePendingToDockerID(name, creation, uid string) error {
	if !dockerIDRE.MatchString(uid) {
		return fmt.Errorf("reaper: invalid immutable Docker ID %q", uid)
	}
	if err := r.validateEntry(reaperEntry{id: name, creation: creation, pending: true}); err != nil {
		return err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
	}

	idEntry := reaperEntry{id: uid}
	idIndex, pendingIndex := -1, -1
	for i, entry := range r.entries {
		if entry.id == idEntry.id {
			idIndex = i
		}
		if entry.pending && entry.id == name && entry.creation == creation {
			pendingIndex = i
		}
	}
	if idIndex >= 0 && pendingIndex < 0 {
		r.mu.Unlock()
		return nil
	}
	writeID := idIndex < 0
	if pendingIndex >= 0 {
		if idIndex >= 0 {
			r.entries = append(r.entries[:pendingIndex], r.entries[pendingIndex+1:]...)
		} else {
			r.entries[pendingIndex] = idEntry
		}
	} else {
		if err := r.makeRoomLocked(1); err != nil {
			r.mu.Unlock()
			return err
		}
		r.entries = append(r.entries, idEntry)
	}
	if r.stdin != nil {
		wrote := !writeID
		if writeID {
			wrote = r.writeLocked(idEntry) == nil
		}
		if wrote && writeReaperCancellation(r.stdin, reaperEntry{id: name, creation: creation}) == nil {
			if err := r.acknowledgeRetirementLocked(); err != nil {
				r.mu.Unlock()
				return err
			}
			r.retiredPending = true
			r.retirementAcked = true
			r.mu.Unlock()
			return nil
		}
	}
	defer r.mu.Unlock()
	return r.respawnAndReplayLocked()
}

func (r *reaper) validateEntry(e reaperEntry) error {
	immutableID := r.subcommand == "rm" && dockerIDRE.MatchString(e.id)
	if !nameRE.MatchString(e.id) && !immutableID {
		return fmt.Errorf("reaper: invalid container id %q", e.id)
	}
	if e.creation != "" && !creationRE.MatchString(e.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", e.creation)
	}
	if e.pending && e.creation == "" {
		return errors.New("reaper: pending entry requires a creation id")
	}
	return nil
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

// stopChildLocked retires the child that currently owns the reaper pipe
// before a replacement can be replayed. It never signals a process-group
// ID: after cmd.Wait reports exit, that numeric group may already belong
// to an unrelated process. The caller holds opMu and r.mu on entry; the
// short wait is performed with r.mu released so registrations are not
// held behind a stuck child indefinitely.
func (r *reaper) stopChildLocked() error {
	stdin, cmd, exited, ack := r.stdin, r.cmd, r.exited, r.ack
	r.stdin, r.cmd, r.exited, r.ack = nil, nil, nil, nil
	if cmd == nil || channelClosed(exited) {
		if stdin != nil {
			_ = stdin.Close()
		}
		if ack != nil {
			_ = ack.Close()
		}
		return nil
	}

	r.mu.Unlock()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cleanupCancel()
	var cleanupErr error
	quiesceErr := acknowledgeReaperCommand(cleanupCtx, stdin, ack, "Q#containergo-quiesce:", r.writeTimeout)
	quiesced := quiesceErr == nil
	canKill := false
	if !quiesced {
		// Wait and kill are serialized by the lifecycle state. Once Wait
		// has begun, the PID is never signaled; a stopper that wins the
		// state transition signals the still-owned process and lets Wait
		// reap it.
		r.lifecycleMu.Lock()
		canKill = !r.reaping
		if canKill {
			r.stopping = true
		}
		r.lifecycleMu.Unlock()
		if canKill && !channelClosed(exited) && cmd.Process != nil {
			// killReaperCommand uses the owned root process (and only
			// verified positive descendant PIDs); it never signals a group ID.
			cleanupErr = killReaperCommand(cleanupCtx, cmd)
			if errors.Is(cleanupErr, os.ErrProcessDone) {
				cleanupErr = nil
			}
		}
	}
	if (quiesced || canKill) && stdin != nil {
		_ = stdin.Close()
	}
	if exited != nil {
		select {
		case <-exited:
		case <-cleanupCtx.Done():
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("reaper: old child did not exit: %w", cleanupCtx.Err()))
		}
	}
	if cleanupErr != nil && exited != nil && channelClosed(exited) {
		// The owned child exited on its own; an ownership error reported
		// during the final signal attempt is no longer actionable.
		cleanupErr = nil
	}
	if ack != nil {
		_ = ack.Close()
	}
	r.mu.Lock()
	if cleanupErr != nil {
		// Keep the old child discoverable so a later retry cannot replay
		// entries while the previous process is still alive.
		r.cmd = cmd
		r.stdin = stdin
		r.exited = exited
		r.ack = ack
	}
	return cleanupErr
}

// respawnAndReplayLocked starts a fresh reaper process and re-registers
// every known ID with it. A retirement must have received the child's
// acknowledgement before an old child can be stopped; otherwise the
// existing pipe stays open and this operation fails closed.
func (r *reaper) respawnAndReplayLocked() error {
	if r.retiredPending && !r.retirementAcked {
		return errors.New("reaper: retirement is awaiting the existing child's EOF barrier")
	}
	if len(r.entries) == 0 {
		r.spawnFailures = 0
		r.gaveUp = false
		r.retiredPending = false
		r.retirementAcked = false
		return nil
	}
	if err := r.stopChildLocked(); err != nil {
		r.recordFailureLocked(err)
		return err
	}
	if r.spawnFailures >= maxReaperSpawnFailures {
		r.spawnFailures = 0
		r.gaveUp = false
	}
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			r.spawnFailures++
			r.recordFailureLocked(err)
			continue
		}
		replayed := true
		for _, e := range r.entries {
			if err := r.writeLocked(e); err != nil {
				replayed = false
				r.recordFailureLocked(err)
				break
			}
		}
		if replayed {
			r.spawnFailures = 0
			r.gaveUp = false
			return nil
		}
		if err := r.stopChildLocked(); err != nil {
			r.recordFailureLocked(err)
			return err
		}
		r.spawnFailures++
	}
	if !r.gaveUp {
		r.gaveUp = true
		r.recordFailureLocked(errors.New("reaper: giving up after repeated spawn failures"))
	}
	return errors.New("reaper: giving up after repeated spawn failures")
}

func (r *reaper) recordFailureLocked(err error) {
	if err == nil {
		return
	}
	log.Printf("container-go: reaper %s/%s: %v", r.binary, r.subcommand, err)
}

func (r *reaper) spawnLocked() error {
	timeout := r.timeoutSeconds
	if timeout <= 0 {
		timeout = defaultReaperTimeoutSeconds
	}
	pendingAttempts := r.pendingAttempts
	if pendingAttempts <= 0 {
		pendingAttempts = 1
	}
	var cmd *exec.Cmd
	if r.command != nil {
		cmd = r.command()
	} else {
		cmd = exec.Command(
			"/bin/sh", "-c", reaperScript, "containergo-reaper",
			r.binary, r.subcommand, breQuote(creationLabel), strconv.Itoa(timeout), strconv.Itoa(pendingAttempts),
		)
	}
	if cmd == nil {
		return errors.New("reaper: nil spawn command")
	}
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	ack, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = ack.Close()
		return err
	}
	r.retiredPending = false
	r.retirementAcked = false
	exited := make(chan struct{})
	r.lifecycleMu.Lock()
	r.reaping = false
	r.stopping = false
	r.lifecycleMu.Unlock()
	go func() {
		// Mark the hand-off to Wait before blocking on it. Retirement
		// checks the same state, so it never signals a PID after Wait has
		// begun reaping it; a concurrent stopper marks stopping first and
		// the Wait below reaps the owned process after that signal.
		r.lifecycleMu.Lock()
		if !r.stopping {
			r.reaping = true
		}
		r.lifecycleMu.Unlock()
		_ = cmd.Wait()
		r.lifecycleMu.Lock()
		// Publish the terminal channel before clearing the ownership state.
		// A stopper that observes the cleared state can then also observe
		// the closed channel and will never signal the reaped PID.
		close(exited)
		r.reaping = false
		r.stopping = false
		r.lifecycleMu.Unlock()
	}()
	r.cmd, r.stdin, r.exited, r.ack = cmd, stdin, exited, ack
	go r.respawnAfterUnexpectedExit(cmd, exited)
	return nil
}

func (r *reaper) respawnAfterUnexpectedExit(cmd *exec.Cmd, exited <-chan struct{}) {
	<-exited
	r.mu.Lock()
	if r.closed || r.cmd != cmd {
		r.mu.Unlock()
		return
	}
	ack := r.ack
	r.cmd = nil
	r.stdin = nil
	r.exited = nil
	r.ack = nil
	// EOF means the child consumed the retirement records before it
	// exited; a future replay cannot resurrect a handed-off entry.
	r.retiredPending = false
	r.retirementAcked = false
	if ack != nil {
		_ = ack.Close()
	}
	backoff := reaperBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	r.mu.Unlock()

	timer := time.NewTimer(backoff)
	defer timer.Stop()
	<-timer.C
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.cmd != nil {
		return
	}
	_ = r.respawnAndReplayLocked()
}

// closeStdin hands the reaper the same EOF it would see on parent
// death. Test hook and best-effort shutdown.
func (r *reaper) closeStdin() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.stdin != nil {
		_ = r.stdin.Close()
		r.stdin = nil
	}
}

// killForTest kills the owned reaper child and waits until it is reaped,
// so the next write deterministically fails. The cleanup is bounded and
// never signals a numeric process-group ID.
func (r *reaper) killForTest() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	stdin := r.stdin
	r.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cancel()
	var cleanupErrors []error
	r.lifecycleMu.Lock()
	canKill := cmd != nil && !r.reaping
	if canKill {
		r.stopping = true
	}
	r.lifecycleMu.Unlock()
	if canKill && !channelClosed(exited) {
		if err := killReaperCommand(ctx, cmd); err != nil {
			log.Printf("container-go: reaper cleanup: %v", err)
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if exited != nil {
		select {
		case <-exited:
		case <-ctx.Done():
			cleanupErrors = append(cleanupErrors, fmt.Errorf("reaper: cleanup timed out: %w", ctx.Err()))
		}
	}
	return errors.Join(cleanupErrors...)
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

// registerWithGlobalReaper best-effort registers a container with the
// process-wide reaper for its backend binary. Reaper trouble is logged
// but never fails container startup. The reaper needs /bin/sh, so on
// Windows this is a no-op and cleanup relies on normal paths.
//
//nolint:unused // retained for package-level compatibility and tests
func registerWithGlobalReaper(binary, subcommand, id, creation string) error {
	return registerWithGlobalReaperPending(binary, subcommand, id, creation, false)
}

func preRegisterWithGlobalReaper(binary, subcommand, id, creation string) error {
	return registerWithGlobalReaperPending(binary, subcommand, id, creation, true)
}

func registerWithGlobalReaperPending(binary, subcommand, id, creation string, pending bool) error {
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
	var err error
	if pending {
		err = r.registerPending(id, creation)
	} else {
		err = r.register(id, creation)
	}
	if err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
	}
	return err
}

func promotePendingDockerIDWithGlobalReaper(binary, subcommand, name, creation, uid string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return registerWithGlobalReaper(binary, subcommand, uid, "")
	}
	if err := r.promotePendingToDockerID(name, creation, uid); err != nil {
		log.Printf("container-go: reaper immutable-ID promotion failed: %v", err)
		return err
	}
	return nil
}

func retireReaperEntry(binary, subcommand, name, uid string) {
	if err := unregisterHandoffWithGlobalReaper(binary, subcommand, name, uid); err != nil {
		log.Printf("container-go: reaper retirement failed (binary=%q): %v", binary, err)
	}
}

func unregisterHandoffWithGlobalReaper(binary, subcommand, name, uid string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return nil
	}
	if r.subcommand != subcommand {
		return fmt.Errorf("reaper: subcommand mismatch for %q: have %q, want %q", binary, r.subcommand, subcommand)
	}
	if err := r.unregisterHandoff(name, uid); err != nil {
		log.Printf("container-go: reaper handoff cleanup failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func completePendingWithGlobalReaper(binary, subcommand, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil {
		return nil
	}
	if err := r.completePending(id, creation); err != nil {
		log.Printf("container-go: reaper completion failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}
