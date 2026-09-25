package container

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While the process lives the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// Name-addressed entries carry all four library lock barriers in the
// order used by lockName: legacy TMPDIR, transitional cache,
// maintenance, and durable state. Each path is a durable hard-link lease
// validated at registration. The shell uses lockf(1) to hold every path,
// checks the registered device/inode identity, and only then inspects and
// deletes. If any lock, lease, identity, or helper is unavailable, the
// entry is skipped rather than deleted without coordination. Docker's
// immutable-ID entries remain lock-free.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
ids=""
tab=$(printf '\t')
while IFS= read -r line; do
  ids="$ids
$line"
done
helper=$(mktemp 2>/dev/null) || exit 0
trap 'rm -f "$helper"' EXIT
cat >"$helper" <<'REAPER_LOCKED_HELPER'
#!/bin/sh
set -f
file_identity() {
  stat -c '%d:%i' "$1" 2>/dev/null || stat -f '%d:%i' "$1" 2>/dev/null
}
identities_valid() {
  [ "$#" -eq 0 ] && return 0
  path=$1
  expected=$2
  shift 2
  actual=$(file_identity "$path") || return 1
  [ "$actual" = "$expected" ] || return 1
  identities_valid "$@"
}
stage=$1
shift
if [ "$stage" -lt 3 ]; then
  case "$stage" in
    0) next=$3; next_identity=$4 ;;
    1) next=$5; next_identity=$6 ;;
    2) next=$7; next_identity=$8 ;;
    *) exit 0 ;;
  esac
  next_stage=$((stage + 1))
  REAPER_LOCK_STAGE=$next_stage exec "$REAPER_LOCKF" -k -n -t 30 "$next" "$0" "$next_stage" "$@"
fi
# All four locks are held. Recheck every registered inode before the
# inspect and again after parsing it, so a replaced lease fails closed.
identities_valid "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" || exit 0
id=$9
creation=${10}
bin=$REAPER_BIN
sub=$REAPER_SUB
key=$REAPER_KEY
tmp=$(mktemp 2>/dev/null) || exit 0
("$bin" inspect "$id" >"$tmp" 2>/dev/null & pid=$!; (sleep 10; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || { rm -f "$tmp"; exit 0; }
got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\\([0-9a-f]\\{16\\}\\)\".*/\\1/p" "$tmp" 2>/dev/null | head -n 1)
rm -f "$tmp"
[ "$got" = "$creation" ] || exit 0
identities_valid "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" || exit 0
("$bin" "$sub" --force "$id" >/dev/null 2>&1 & pid=$!; (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || true
REAPER_LOCKED_HELPER
chmod 700 "$helper" 2>/dev/null || exit 0
run_with_timeout() {
  "$@" >/dev/null 2>&1 & pid=$!
  (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!
  wait "$pid" 2>/dev/null
  rc=$?
  kill "$killer" 2>/dev/null
  wait "$killer" 2>/dev/null
  return $rc
}
valid_lock_path() {
  [ -n "$1" ] || return 1
  [ -L "$1" ] && return 1
  [ -f "$1" ] || return 1
  return 0
}
run_locked() {
  id="$1"
  creation="$2"
  lock1="$3"
  identity1="$4"
  lock2="$5"
  identity2="$6"
  lock3="$7"
  identity3="$8"
  lock4="$9"
  identity4="${10}"
  valid_lock_path "$lock1" || return 0
  valid_lock_path "$lock2" || return 0
  valid_lock_path "$lock3" || return 0
  valid_lock_path "$lock4" || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  [ -n "$lockf_bin" ] || return 0
  REAPER_STATE_HELD=0 REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_LOCKF="$lockf_bin" \
    "$lockf_bin" -k -n -t 30 "$lock1" "$helper" 0 "$lock1" "$identity1" "$lock2" "$identity2" "$lock3" "$identity3" "$lock4" "$identity4" "$id" "$creation" >/dev/null 2>&1 || true
}
printf '%s\n' "$ids" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$line" in
    *"$tab"*)
      id=${line%%"$tab"*}
      rest=${line#*"$tab"}
      creation=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lock1=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      identity1=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      lock2=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      identity2=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      lock3=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      identity3=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      lock4=${rest%%"$tab"*}
      identity4=${rest#*"$tab"}
      case "$identity4" in *"$tab"*) continue ;; esac
      ;;
    *)
      id=$line
      creation=""
      lock1=""
      identity1=""
      lock2=""
      identity2=""
      lock3=""
      identity3=""
      lock4=""
      identity4=""
      ;;
  esac
  [ -n "$id" ] || continue
  if [ "$sub" = delete ]; then
    [ -n "$creation" ] || continue
    [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] && [ -n "$lock4" ] || continue
    run_locked "$id" "$creation" "$lock1" "$identity1" "$lock2" "$identity2" "$lock3" "$identity3" "$lock4" "$identity4"
  else
    run_with_timeout "$bin" "$sub" --force "$id" || true
  fi
done
`

const maxReaperSpawnFailures = 3

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

func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}

func validNameLockProtocolIdentity(identity string) bool {
	return nameLockIdentityRE.MatchString(identity)
}

type reaperEntry struct {
	id             string
	creation       string
	lockPaths      []string
	lockIdentities []string
	// stateLockPath and maintenancePath retain the pre-lease field names
	// for package-local compatibility; new protocol entries also carry
	// the historical barriers in lockPaths.
	stateLockPath   string
	maintenancePath string
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	exited        chan struct{}
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{binary: binary, subcommand: subcommand}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. Name-addressed entries must
// carry a valid generation and all four prepared stable lock barriers.
func (r *reaper) register(id, creation string) error {
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	if r.subcommand == "delete" {
		if !nameRE.MatchString(id) {
			return fmt.Errorf("reaper: invalid container name %q", id)
		}
		if !creationRE.MatchString(creation) {
			return fmt.Errorf("reaper: name-addressed entry %q requires a valid creation generation", id)
		}
	} else {
		if !dockerIDRE.MatchString(id) {
			return fmt.Errorf("reaper: invalid immutable container id %q", id)
		}
		// Docker IDs are already immutable; do not add a lock or inspect
		// dependency to their watchdog path.
		creation = ""
	}
	if runtime.GOOS == "windows" {
		return nil
	}

	var lockPaths, lockIdentities []string
	if r.subcommand == "delete" {
		var err error
		lockPaths, lockIdentities, err = reaperNameLockSet(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", id, err)
		}
		if len(lockPaths) != 4 || len(lockIdentities) != 4 {
			return fmt.Errorf("reaper: got %d/%d lock barriers for %q, want 4/4", len(lockPaths), len(lockIdentities), id)
		}
		for i, path := range lockPaths {
			if !validNameLockProtocolPath(path) || !validNameLockProtocolIdentity(lockIdentities[i]) {
				return fmt.Errorf("reaper: invalid name lock lease for %q", id)
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	stateLockPath, maintenancePath := "", ""
	if len(lockPaths) == 4 {
		stateLockPath, maintenancePath = lockPaths[3], lockPaths[2]
	}
	entry := reaperEntry{
		id: id, creation: creation, lockPaths: lockPaths, lockIdentities: lockIdentities,
		stateLockPath: stateLockPath, maintenancePath: maintenancePath,
	}
	r.entries = append(r.entries, entry)
	if r.stdin != nil {
		if r.writeLocked(entry) == nil {
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) writeLocked(e reaperEntry) error {
	if r.stdin == nil {
		return io.ErrClosedPipe
	}
	line := e.id
	if len(e.lockPaths) > 0 || e.creation != "" {
		if len(e.lockPaths) != 4 || len(e.lockIdentities) != 4 {
			return fmt.Errorf("reaper: incomplete stable name locks for %q", e.id)
		}
		line += "\t" + e.creation
		for i, path := range e.lockPaths {
			if !validNameLockProtocolPath(path) || !validNameLockProtocolIdentity(e.lockIdentities[i]) {
				return fmt.Errorf("reaper: invalid stable name lock for %q", e.id)
			}
			line += "\t" + path + "\t" + e.lockIdentities[i]
		}
	}
	line += "\n"
	n, err := io.WriteString(r.stdin, line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

// respawnAndReplayLocked starts a fresh reaper child and re-registers
// every known ID with it. Success resets the consecutive-failure count;
// giving up logs once so a permanently broken reaper is visible.
func (r *reaper) respawnAndReplayLocked() error {
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			r.spawnFailures++
			continue
		}
		replayed := true
		for _, e := range r.entries {
			if r.writeLocked(e) != nil {
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
	cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
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

// closeStdin hands the reaper the same EOF it would see on parent
// death. Test hook and best-effort shutdown.
func (r *reaper) closeStdin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
}

// killForTest kills the reaper child and waits until it is reaped, so
// the next write deterministically fails.
func (r *reaper) killForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		_ = r.cmd.Process.Kill()
		<-r.exited
	}
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

// registerWithGlobalReaper best-effort registers a container with the
// process-wide reaper for its backend binary. Reaper trouble never fails
// container startup, but an Apple entry without all validated leases is
// not registered and therefore cannot be deleted without coordination.
func registerWithGlobalReaper(binary, subcommand, id, creation string) {
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
	if err := r.register(id, creation); err != nil {
		log.Printf("container-go: reaper registration %s: %v", id, err)
	}
}
