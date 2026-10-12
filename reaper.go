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
//
// A create is registered before the backend command starts. The pending
// record lets the child recheck a name while a create is still settling;
// the completion record keeps the same generation in the replay set. If
// the child exits while the parent is alive, a monitor starts a new child
// and replays every retained record.
//
// The script is fixed. IDs enter it only as stdin data validated as an
// Apple Container name or a full Docker ID. Inspect output is projected
// while it streams, so raw JSON (which may contain environment secrets)
// is never staged in a host file. Backend-specific delete flags are
// retained for the normal cleanup and reaper paths.
//
// The event stream is accumulated until stdin EOF before any delete
// runs, so registration while the parent lives cannot force-delete a
// live container. After EOF the reap is bounded overall and entries are
// processed a few at a time: the reaper is insurance and must finish
// even when the backend answers nothing, rather than paying the
// container count times the per-entry timeout.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
shift 3
timeout="$1"
pending_attempts="$2"
shift 2
case "$timeout" in ''|*[!0-9]*) timeout=30;; esac
case "$pending_attempts" in ''|*[!0-9]*) pending_attempts=30;; esac
[ "$timeout" -gt 0 ] 2>/dev/null || timeout=30
[ "$pending_attempts" -gt 0 ] 2>/dev/null || pending_attempts=1
tab=$(printf '\t')
# Overall reap budget and bounded concurrency. Both apply only after
# EOF (the awk END below), so a long-lived parent does not spend them.
budget="${REAPER_BUDGET_SECONDS:-120}"
parallel="${REAPER_PARALLEL:-4}"
case "$budget" in ''|*[!0-9]*) budget=120;; esac
case "$parallel" in ''|*[!0-9]*) parallel=4;; esac
[ "$budget" -gt 0 ] 2>/dev/null || budget=120
[ "$parallel" -gt 0 ] 2>/dev/null || parallel=4
started=0
remaining() {
  if [ "$started" -eq 0 ]; then
    printf '%s' "$budget"
    return 0
  fi
  now=$(date +%s 2>/dev/null || echo 0)
  left=$((started + budget - now))
  [ "$left" -gt 0 ] || left=0
  printf '%s' "$left"
}

# Keep the backend operation in a separate shell process. The same
# operation is run either directly for an immutable ID or under flock
# (with lockf only as a fallback) for a name-addressed generation. The
# fixed helper contains no inspect output and is passed only validated
# IDs and options.
entry_script=
while IFS= read -r entry_line
do
  entry_script="$entry_script$entry_line
"
done <<'REAPER_ENTRY'
set -f
set -m 2>/dev/null || true
bin="$REAPER_BIN"
sub="$REAPER_SUB"
key="$REAPER_KEY"
timeout="$REAPER_TIMEOUT"
pending_attempts="$REAPER_PENDING_ATTEMPTS"
entry_id="$1"
entry_creation="$2"
entry_pending="$3"
shift 3

# Keep a backend call bounded without leaving the sleep process behind.
# Job control gives the command its own process group when supported.
run_with_timeout() {
  seconds="$1"
  shift
  "$@" & command_pid=$!
  (
    timer_sleeper=
    timer_cleanup() {
      if [ -n "$timer_sleeper" ]; then
        kill -9 "$timer_sleeper" 2>/dev/null || true
        wait "$timer_sleeper" 2>/dev/null || true
      fi
      timer_sleeper=
    }
    trap 'timer_cleanup; exit 0' HUP INT TERM
    sleep "$seconds" &
    timer_sleeper=$!
    wait "$timer_sleeper"
    timer_rc=$?
    if [ "$timer_rc" -eq 0 ]; then
      kill -9 -"$command_pid" 2>/dev/null || kill -9 "$command_pid" 2>/dev/null || true
    fi
    exit 0
  ) & timer_pid=$!
  wait "$command_pid" 2>/dev/null
  command_rc=$?
  kill "$timer_pid" 2>/dev/null || true
  wait "$timer_pid" 2>/dev/null || true
  return "$command_rc"
}

valid_docker_id() {
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
  [ "${#1}" -eq 64 ] 2>/dev/null
}

inspect_projection() {
  inspect_id="$1"
  {
    run_with_timeout 10 "$bin" inspect "$inspect_id" 2>/dev/null
    inspect_rc=$?
    printf '\n__containergo_inspect_rc__%s\n' "$inspect_rc"
  } | sed -n \
    -e "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/creation=\1/p" \
    -e 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/id=\1/p' \
    -e 's/^__containergo_inspect_rc__\([0-9][0-9]*\)$/rc=\1/p'
}

process_entry() {
  target="$entry_id"
  if [ -n "$entry_creation" ]; then
    tries=0
    while :; do
      fields=$(inspect_projection "$entry_id") || fields=
      got=
      uid=
      inspect_rc=
      for field in $fields; do
        case "$field" in
          creation=*) got=${field#creation=} ;;
          id=*) uid=${field#id=} ;;
          rc=*) inspect_rc=${field#rc=} ;;
        esac
      done
      if [ "$inspect_rc" = 0 ]; then
        if [ "$got" != "$entry_creation" ]; then
          # A completed generation must never be deleted after it has
          # been replaced. A pending create may still be settling.
          [ "$entry_pending" = 1 ] && [ -z "$got" ] || return 0
        else
          # Exact generation match is the delete gate.
          [ "$got" = "$entry_creation" ] || return 0
          if [ "$sub" = rm ]; then
            valid_docker_id "$uid" || return 0
            target="$uid"
          fi
          break
        fi
      fi
      [ "$entry_pending" = 1 ] || return 0
      tries=$((tries + 1))
      [ "$tries" -lt "$pending_attempts" ] || return 0
      sleep 1
    done
  elif [ "$sub" = rm ]; then
    valid_docker_id "$entry_id" || return 0
  fi
  run_with_timeout "$timeout" "$bin" "$sub" --force "$@" "$target" >/dev/null 2>&1 || true
}

process_entry "$@"
REAPER_ENTRY

run_entry() {
  entry_state="$1"
  entry_id="$2"
  entry_creation="$3"
  entry_lock="$4"
  shift 4
  [ -n "$entry_id" ] || return 0
  [ "$entry_creation" = "-" ] && entry_creation=
  [ "$entry_state" = S ] && return 0
  pending=0
  [ "$entry_state" = P ] && pending=1

  # Every name-addressed operation must share the Go name lock. An
  # immutable Docker ID is the only operation that may run unlocked.
  case "$entry_id" in
    *[!A-Za-z0-9_.-]*) [ -n "$entry_lock" ] || return 0 ;;
  esac
  case "$sub" in
    delete)
      if [ -z "$entry_lock" ]; then
        entry_lock="${TMPDIR:-/tmp}/containergo-$entry_id.lock"
      fi
      ;;
    rm)
      case "$entry_id" in
        *[!0-9a-f]*)
          if [ -z "$entry_lock" ]; then entry_lock="${TMPDIR:-/tmp}/containergo-$entry_id.lock"; fi
          ;;
        *)
          if [ "${#entry_id}" -ne 64 ] && [ -z "$entry_lock" ]; then entry_lock="${TMPDIR:-/tmp}/containergo-$entry_id.lock"; fi
          ;;
      esac
      ;;
  esac
  if [ -n "$entry_lock" ]; then
    [ -L "$entry_lock" ] && return 0
    flock_bin=$(command -v flock 2>/dev/null) || flock_bin=
    case "$flock_bin" in
      /*) [ -x "$flock_bin" ] || flock_bin= ;;
      *) flock_bin= ;;
    esac
    if [ -n "$flock_bin" ]; then
      REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
      REAPER_TIMEOUT="$timeout" REAPER_PENDING_ATTEMPTS="$pending_attempts" \
        "$flock_bin" -x "$entry_lock" sh -c "$entry_script" reaper-entry \
        "$entry_id" "$entry_creation" "$pending" "$@"
      return $?
    fi
    lockf_bin=$(command -v lockf 2>/dev/null) || lockf_bin=
    case "$lockf_bin" in
      /*) [ -x "$lockf_bin" ] || lockf_bin= ;;
      *) lockf_bin= ;;
    esac
    if [ -n "$lockf_bin" ]; then
      REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
      REAPER_TIMEOUT="$timeout" REAPER_PENDING_ATTEMPTS="$pending_attempts" \
        "$lockf_bin" -k -t 30 "$entry_lock" sh -c "$entry_script" reaper-entry \
        "$entry_id" "$entry_creation" "$pending" "$@"
      return $?
    else
      return 0
    fi
  fi
  REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
  REAPER_TIMEOUT="$timeout" REAPER_PENDING_ATTEMPTS="$pending_attempts" \
    sh -c "$entry_script" reaper-entry "$entry_id" "$entry_creation" "$pending" "$@"
}

# The input is a small event stream. P marks a create that is still
# settling, C changes that exact key to active, S makes it non-destructive,
# and + is an already complete entry. The fourth field is the shared
# name-lock path. Plain id lines remain accepted for compatibility.
awk '
{
  if (index($0, "\t") != 0) { print; next }
  n=split($0, legacy, /[[:space:]]+/)
  if (n == 1) { print legacy[1]; next }
  if (legacy[1] == "+" || legacy[1] == "P" || legacy[1] == "C" || legacy[1] == "S" || legacy[1] == "-") {
    gen=(n >= 3 && legacy[3] != "" ? legacy[3] : "-")
    line=legacy[1] "\t" legacy[2] "\t" gen
    if (n >= 4) line=line "\t" legacy[4]
    print line
  } else {
    print legacy[1] "\t" legacy[2] "\t" legacy[3]
  }
}
' | awk -F '\t' '
function key(id, gen) { return id SUBSEP gen }
$1 == "P" && NF >= 3 { k=key($2, $3); active[k]="P"; locks[k]=$4; next }
$1 == "C" && NF >= 3 { k=key($2, $3); if (k in active) active[k]="A"; if (NF >= 4) locks[k]=$4; next }
$1 == "S" && NF >= 3 { k=key($2, $3); if (k in active) active[k]="S"; if (NF >= 4) locks[k]=$4; next }
$1 == "+" && NF >= 2 { gen=$3; if (gen == "-") gen=""; k=key($2, gen); active[k]="A"; locks[k]=$4; next }
$1 == "-" && NF >= 2 { gen=$3; if (gen == "-") gen=""; k=key($2, gen); delete active[k]; delete locks[k]; next }
NF == 1 { active[key($1, "")]="A"; next }
NF >= 2 && $1 !~ /^[+PCS-]$/ { k=key($1, $2); active[k]="A"; locks[k]=$3; next }
END {
  for (k in active) {
    split(k, fields, SUBSEP)
    gen=(fields[2] == "" ? "-" : fields[2])
    line=active[k] "\t" fields[1] "\t" gen
    if (locks[k] != "") line=line "\t" locks[k]
    print line
  }
}
' | {
  # The consumer starts with the pipeline, but awk's END only prints
  # after EOF. Arm the budget on the first entry so a long-lived parent
  # does not spend it before cleanup begins.
  batch=0
  while IFS="$tab" read -r state entry_id entry_creation entry_lock; do
    [ -n "$entry_id" ] || continue
    if [ "$started" -eq 0 ]; then
      started=$(( $(date +%s 2>/dev/null || echo 1) ))
      [ "$started" -gt 0 ] 2>/dev/null || started=1
    fi
    [ "$(remaining)" -gt 0 ] || break
    run_entry "$state" "$entry_id" "$entry_creation" "$entry_lock" "$@" &
    batch=$((batch + 1))
    if [ "$batch" -ge "$parallel" ]; then
      wait
      batch=0
    fi
  done
  wait
}
`

const (
	maxReaperSpawnFailures       = 3
	defaultReaperTimeoutSeconds  = 30
	defaultReaperPendingAttempts = 30

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
	lockPath string
	pending  bool
	shared   bool
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force. deleteFlags carries backend-specific
	// options between --force and the target.
	subcommand  string
	deleteFlags []string

	// opMu serializes registration, completion, close, and recovery.
	// mu protects fields; waiting for a child is never done while mu is
	// held.
	opMu          sync.Mutex
	mu            sync.Mutex
	cmd           *exec.Cmd
	pgid          int
	stdin         io.WriteCloser
	exited        chan struct{}
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
	gaveUpLogged  bool
	closed        bool

	// A failed recovery burst is retried only after a bounded cooldown.
	// retryPending allows one immediate recovery attempt before the
	// cooldown, which lets a transient launch failure recover without
	// turning every subsequent registration into a hot retry loop.
	retryPending bool
	recovering   bool
	retryAt      time.Time
	retryLevel   int

	// These are test seams. Production values are bounded defaults.
	timeoutSeconds  int
	pendingAttempts int
	spawnCommand    func() (*exec.Cmd, error)
	now             func() time.Time
	retryBackoff    func(int) time.Duration
}

func newReaper(binary, subcommand string, deleteFlags ...string) *reaper {
	return &reaper{
		binary:          binary,
		subcommand:      subcommand,
		deleteFlags:     append([]string(nil), deleteFlags...),
		timeoutSeconds:  defaultReaperTimeoutSeconds,
		pendingAttempts: defaultReaperPendingAttempts,
	}
}

func validateReaperEntry(entry reaperEntry) error {
	if !nameRE.MatchString(entry.id) && !dockerIDRE.MatchString(entry.id) {
		return fmt.Errorf("reaper: invalid container id %q", entry.id)
	}
	if entry.creation != "" && !creationRE.MatchString(entry.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", entry.creation)
	}
	if entry.pending && entry.creation == "" {
		return errors.New("reaper: pending entry requires a creation id")
	}
	if strings.ContainsAny(entry.lockPath, "\t\r\n") {
		return errors.New("reaper: lock path contains a control character")
	}
	return nil
}

func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

func (r *reaper) prepareEntry(entry reaperEntry) (reaperEntry, error) {
	if err := r.validateEntry(entry); err != nil {
		return reaperEntry{}, err
	}
	// A full Docker ID is already immutable; a supplied generation is
	// redundant and must not turn the entry into a name lookup.
	if r.subcommand == "rm" && dockerIDRE.MatchString(entry.id) {
		entry.creation = ""
		entry.pending = false
	}
	// Apple is name-addressed, and Docker may temporarily address a
	// generation by name before run returns its immutable ID. Both use
	// the same lock file as Go's generation-checked name operations.
	if r.subcommand == "delete" || !dockerIDRE.MatchString(entry.id) {
		lockPath, err := reaperNameLockPath(entry.id)
		if err != nil {
			return reaperEntry{}, fmt.Errorf("reaper: prepare name lock: %w", err)
		}
		entry.lockPath = lockPath
	}
	if err := validateReaperEntry(entry); err != nil {
		return reaperEntry{}, err
	}
	return entry, nil
}

func (r *reaper) registerEntry(entry reaperEntry) error {
	entry, err := r.prepareEntry(entry)
	if err != nil {
		return err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
	}
	index := -1
	changed := false
	for i, existing := range r.entries {
		if existing.id == entry.id && existing.creation == entry.creation {
			index = i
			if existing.shared {
				r.mu.Unlock()
				return nil
			}
			if entry.pending {
				// Never downgrade an already complete generation to
				// pending; the existing event already protects it.
				r.mu.Unlock()
				return nil
			}
			if existing.pending {
				r.entries[i] = entry
				changed = true
			} else if existing.lockPath == "" && entry.lockPath != "" {
				existing.lockPath = entry.lockPath
				r.entries[i] = existing
				changed = true
			}
			break
		}
	}
	if index < 0 {
		r.entries = append(r.entries, entry)
	} else if !changed {
		// Duplicate completed registration is already represented in
		// the event stream; replaying it is unnecessary.
		r.mu.Unlock()
		return nil
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

func (r *reaper) validateEntry(entry reaperEntry) error {
	if err := validateReaperEntry(entry); err != nil {
		return err
	}
	if r.subcommand != "delete" && r.subcommand != "rm" {
		return fmt.Errorf("reaper: invalid delete subcommand %q", r.subcommand)
	}
	if r.subcommand == "delete" && !nameRE.MatchString(entry.id) {
		return fmt.Errorf("reaper: invalid Apple container name %q", entry.id)
	}
	return nil
}

func writeReaperEntry(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	prefix := "+"
	if entry.shared {
		prefix = "S"
	} else if entry.pending {
		prefix = "P"
	}
	line := prefix + "\t" + entry.id
	if entry.creation != "" {
		line += "\t" + entry.creation
	} else {
		line += "\t-"
	}
	if entry.lockPath != "" {
		line += "\t" + entry.lockPath
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) writeCurrentEntry(entry reaperEntry) error {
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	return writeReaperEntry(stdin, entry)
}

func writeReaperCompletion(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "C\t" + entry.id
	if entry.creation != "" {
		line += "\t" + entry.creation
	} else {
		line += "\t-"
	}
	if entry.lockPath != "" {
		line += "\t" + entry.lockPath
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) completePending(id, creation string) error {
	entry, err := r.prepareEntry(reaperEntry{id: id, creation: creation})
	if err != nil {
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
	wasPending := false
	for i, existing := range r.entries {
		if existing.id == id && existing.creation == creation {
			index = i
			if existing.shared || !existing.pending {
				r.mu.Unlock()
				return nil
			}
			wasPending = true
			break
		}
	}
	if index < 0 {
		r.entries = append(r.entries, entry)
	} else {
		r.entries[index] = entry
	}
	stdin, exited, failures := r.stdin, r.exited, r.spawnFailures
	r.mu.Unlock()

	if failures < maxReaperSpawnFailures && stdin != nil && !channelClosed(exited) {
		var err error
		if wasPending {
			err = writeReaperCompletion(stdin, entry)
		} else {
			err = writeReaperEntry(stdin, entry)
		}
		if err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) markShared(id, creation string) error {
	entry := reaperEntry{id: id, creation: creation}
	if err := r.validateEntry(entry); err != nil {
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
	for i, existing := range r.entries {
		if existing.id == id && existing.creation == creation {
			index = i
			break
		}
	}
	if index < 0 {
		r.mu.Unlock()
		return nil
	}
	entry = r.entries[index]
	entry.pending = false
	entry.shared = true
	r.entries[index] = entry
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

func (r *reaper) unregister(id, creation string) error {
	entry := reaperEntry{id: id, creation: creation}
	if err := r.validateEntry(entry); err != nil {
		return err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.Lock()
	index := -1
	for i, existing := range r.entries {
		if existing.id == id && existing.creation == creation {
			index = i
			entry = existing
			break
		}
	}
	if index < 0 {
		r.mu.Unlock()
		return nil
	}
	closed := r.closed
	stdin, exited, failures := r.stdin, r.exited, r.spawnFailures
	r.mu.Unlock()

	if closed {
		r.removeEntryLocked(entry)
		return nil
	}
	// If the child is already gone, the confirmed delete cannot be
	// undone by a later replay; remove the durable intent immediately.
	// A live child, however, keeps the record until it accepts the
	// removal event so an uncertain pipe failure cannot lose cleanup.
	if stdin == nil || channelClosed(exited) {
		r.removeEntryLocked(entry)
		if r.entriesEmpty() {
			// With no retained targets there is no fail-closed state to
			// preserve; a later registration may start a fresh bounded
			// recovery window immediately.
			r.clearSpawnFailure()
			r.stopCurrentProcess()
		}
		return nil
	}
	// Keep the entry in memory until the removal event is accepted by
	// the child. If the pipe write fails, replay the retained entry
	// instead of silently losing a pending cleanup guarantee.
	if failures < maxReaperSpawnFailures {
		if err := writeReaperRemoval(stdin, entry); err == nil {
			r.removeEntryLocked(entry)
			r.clearSpawnFailure()
			if r.entriesEmpty() {
				r.stopCurrentProcess()
			}
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) removeEntryLocked(entry reaperEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.entries {
		if existing.id == entry.id && existing.creation == entry.creation {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return
		}
	}
}

func (r *reaper) entriesEmpty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries) == 0
}

func writeReaperRemoval(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "-\t" + entry.id
	if entry.creation != "" {
		line += "\t" + entry.creation
	} else {
		line += "\t-"
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) clearSpawnFailure() {
	r.mu.Lock()
	r.clearSpawnFailureLocked()
	r.mu.Unlock()
}

func (r *reaper) clearSpawnFailureLocked() {
	r.spawnFailures = 0
	r.gaveUp = false
	r.gaveUpLogged = false
	r.retryPending = false
	r.recovering = false
	r.retryAt = time.Time{}
	r.retryLevel = 0
}

func (r *reaper) nowLocked() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *reaper) retryBackoffLocked(level int) time.Duration {
	if level < 1 {
		level = 1
	}
	if r.retryBackoff != nil {
		delay := r.retryBackoff(level)
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

// retryReadyLocked permits one immediate recovery burst after the first
// failed burst, then applies exponential backoff if that recovery also
// fails. A cooldown only suppresses a best-effort reaper attempt; entries
// remain in memory and no destructive operation can run without a child.
func (r *reaper) retryReadyLocked() bool {
	if !r.gaveUp {
		// Keep recovery possible for a state assembled by an older
		// caller that reached the limit without setting gaveUp.
		if r.spawnFailures >= maxReaperSpawnFailures {
			r.spawnFailures = 0
		}
		return true
	}
	if r.retryPending {
		r.retryPending = false
		r.recovering = true
		r.spawnFailures = 0
		r.gaveUp = false
		r.retryAt = time.Time{}
		return true
	}
	if !r.retryAt.IsZero() && r.nowLocked().Before(r.retryAt) {
		return false
	}
	r.recovering = true
	r.spawnFailures = 0
	r.gaveUp = false
	r.retryAt = time.Time{}
	return true
}

// respawnAndReplayLocked starts a replacement and replays all retained
// entries. The caller holds opMu.
func (r *reaper) respawnAndReplayLocked() error {
	r.stopCurrentProcess()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("reaper: closed")
	}
	if len(r.entries) == 0 {
		r.clearSpawnFailureLocked()
		r.mu.Unlock()
		return nil
	}
	if !r.retryReadyLocked() {
		r.mu.Unlock()
		return errReaperSpawnCooldown
	}
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()

	var lastErr error
	for r.spawnFailuresValue() < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			lastErr = err
			r.recordSpawnFailure()
			continue
		}
		replayed := true
		for _, entry := range entries {
			if err := r.writeCurrentEntry(entry); err != nil {
				lastErr = err
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
	r.mu.Lock()
	logGiveUp := !r.gaveUpLogged
	r.gaveUpLogged = true
	r.mu.Unlock()
	if logGiveUp {
		log.Printf("container-go: reaper giving up temporarily after %d consecutive spawn failures (binary=%q); a later registration will retry", maxReaperSpawnFailures, r.binary)
	}
	if lastErr == nil {
		lastErr = errReaperSpawnFailed
	}
	return fmt.Errorf("%w: %v", errReaperSpawnFailed, lastErr)
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
	if r.spawnFailures < maxReaperSpawnFailures {
		r.mu.Unlock()
		return
	}
	r.gaveUp = true
	if r.recovering {
		r.recovering = false
		r.retryPending = false
		if r.retryLevel < 32 {
			r.retryLevel++
		}
		r.retryAt = r.nowLocked().Add(r.retryBackoffLocked(r.retryLevel))
	} else {
		// Leave one immediate recovery burst available. If that burst
		// fails, recordSpawnFailure takes the cooldown path above.
		r.retryPending = true
		r.retryAt = time.Time{}
	}
	r.mu.Unlock()
}

func (r *reaper) spawnLocked() error {
	var cmd *exec.Cmd
	if r.spawnCommand != nil {
		var err error
		cmd, err = r.spawnCommand()
		if err != nil {
			return err
		}
	} else {
		timeout := r.timeoutSeconds
		if timeout <= 0 {
			timeout = defaultReaperTimeoutSeconds
		}
		attempts := r.pendingAttempts
		if attempts <= 0 {
			attempts = 1
		}
		args := []string{"-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel), strconv.Itoa(timeout), strconv.Itoa(attempts)}
		args = append(args, r.deleteFlags...)
		cmd = exec.Command("/bin/sh", args...)
	}
	if cmd == nil {
		return errors.New("reaper: nil spawn command")
	}
	configureReaperProcess(cmd)
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
	r.mu.Lock()
	r.cmd, r.pgid, r.stdin, r.exited = cmd, reaperProcessGroupID(cmd), stdin, exited
	r.mu.Unlock()
	go r.monitorChild(cmd, exited)
	return nil
}

func (r *reaper) monitorChild(cmd *exec.Cmd, exited <-chan struct{}) {
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

func (r *reaper) stopCurrentProcess() {
	r.mu.Lock()
	cmd, pgid, stdin, exited := r.cmd, r.pgid, r.stdin, r.exited
	r.cmd, r.pgid, r.stdin, r.exited = nil, 0, nil, nil
	r.mu.Unlock()
	if cmd == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if !channelClosed(exited) {
		killReaperProcess(cmd, pgid)
	}
	if exited != nil {
		<-exited
	}
}

// closeStdin hands the reaper the intentional EOF used on parent death
// and in tests. It prevents the monitor from respawning that child.
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

// killForTest kills the current child and waits for it. The monitor sees
// that it is no longer the current child, so the next registration can
// explicitly exercise the respawn path.
func (r *reaper) killForTest() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.stopCurrentProcess()
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

// registerWithGlobalReaper best-effort registers a completed container
// with the process-wide reaper. Reaper trouble never fails startup.
func registerWithGlobalReaper(binary, subcommand string, deleteFlags []string, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	r := getGlobalReaper(binary, subcommand, deleteFlags)
	if err := r.register(id, creation); err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func preRegisterWithGlobalReaper(binary, subcommand string, deleteFlags []string, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	r := getGlobalReaper(binary, subcommand, deleteFlags)
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

func markSharedWithGlobalReaper(binary, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	globalReapersMu.Unlock()
	if !ok {
		return nil
	}
	if err := r.markShared(id, creation); err != nil {
		log.Printf("container-go: reaper shared-state update failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func unregisterWithGlobalReaper(binary, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	globalReapersMu.Unlock()
	if !ok {
		return nil
	}
	if err := r.unregister(id, creation); err != nil {
		log.Printf("container-go: reaper unregister failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}

func getGlobalReaper(binary, subcommand string, deleteFlags []string) *reaper {
	globalReapersMu.Lock()
	defer globalReapersMu.Unlock()
	r, ok := globalReapers[binary]
	if !ok {
		r = newReaper(binary, subcommand, deleteFlags...)
		globalReapers[binary] = r
	}
	return r
}
