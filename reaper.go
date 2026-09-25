package container

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
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
// entry is skipped rather than deleted without coordination. A process-level
// active marker is flock-held by the parent and inherited by the child, so
// lease GC cannot reclaim an aged entry while either side is alive. This is
// a same-revision protocol: it does not coordinate with an older reaper
// that does not take these barriers. Mixed-version reaper safety is not
// claimed; drain old reapers before upgrading. Docker's immutable-ID
// entries remain lock-free.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
ids=""
tab=$(printf '\t')
lease_suffix=".reaper-lease"
file_identity() {
  stat -c '%d:%i' "$1" 2>/dev/null || stat -f '%d:%i' "$1" 2>/dev/null
}
while IFS= read -r line; do
  ids="$ids
$line"
done
helper=$(mktemp 2>/dev/null) || exit 0
hold_records=$(mktemp 2>/dev/null) || exit 0
fixed_records=$(mktemp 2>/dev/null) || { rm -f "$helper" "$hold_records"; exit 0; }
maintenance_records=$(mktemp 2>/dev/null) || { rm -f "$helper" "$hold_records" "$fixed_records"; exit 0; }
cleanup_helper=$(mktemp 2>/dev/null) || { rm -f "$helper" "$hold_records" "$fixed_records" "$maintenance_records"; exit 0; }
trap 'rm -f "$helper" "$hold_records" "$fixed_records" "$maintenance_records" "$cleanup_helper"' EXIT
cat >"$cleanup_helper" <<'REAPER_CLEANUP'
#!/bin/sh
set -f
tab=$(printf '\t')
file_identity() {
  stat -c '%d:%i' "$1" 2>/dev/null || stat -f '%d:%i' "$1" 2>/dev/null
}
fixed_records=$1
lease_suffix=$2
while IFS="$tab" read -r fixed expected; do
  [ -n "$fixed" ] || continue
  [ -n "$expected" ] || continue
  actual=$(file_identity "$fixed") || continue
  [ "$actual" = "$expected" ] || continue
  raw=${fixed%"$lease_suffix"}
  has_hold=0
  set +f
  for candidate in "$raw$lease_suffix".*; do
    if [ -e "$candidate" ]; then
      has_hold=1
      break
    fi
  done
  set -f
  [ "$has_hold" -eq 0 ] || continue
  rm -f "$fixed" 2>/dev/null || true
done <"$fixed_records"
REAPER_CLEANUP
chmod 700 "$cleanup_helper" 2>/dev/null || exit 0
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
  # BSD lockf opens read-only by default; exclusive lockf(3) requires a
  # writable descriptor. -w is mandatory on macOS and safe on the other
  # supported POSIX hosts. Never fall back to an unlocked delete.
  REAPER_LOCK_STAGE=$next_stage exec "$REAPER_LOCKF" -k -n -t 30 -w "$next" "$0" "$next_stage" "$@"
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
  hold1="${11}"
  hold2="${12}"
  hold3="${13}"
  hold4="${14}"
  valid_lock_path "$lock1" || return 0
  valid_lock_path "$lock2" || return 0
  valid_lock_path "$lock3" || return 0
  valid_lock_path "$lock4" || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  [ -n "$lockf_bin" ] || return 0
  REAPER_STATE_HELD=0 REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_LOCKF="$lockf_bin" \
    "$lockf_bin" -k -n -t 30 -w "$lock1" "$helper" 0 "$lock1" "$identity1" "$lock2" "$identity2" "$lock3" "$identity3" "$lock4" "$identity4" "$id" "$creation" "$hold1" "$hold2" "$hold3" "$hold4" >/dev/null 2>&1 || true
}
cleanup_leases() {
  while IFS="$tab" read -r hold expected; do
    [ -n "$hold" ] || continue
    [ -n "$expected" ] || continue
    actual=$(file_identity "$hold") || continue
    [ "$actual" = "$expected" ] || continue
    rm -f "$hold" 2>/dev/null || true
  done <"$hold_records"
  maintenance=
  IFS= read -r maintenance <"$maintenance_records" || true
  [ -n "$maintenance" ] || return 0
  valid_lock_path "$maintenance" || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  "$lockf_bin" -k -n -t 30 -w "$maintenance" "$cleanup_helper" "$fixed_records" "$lease_suffix" >/dev/null 2>&1 || true
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
      lock4=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      identity4=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      hold1=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      hold2=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      hold3=${rest%%"$tab"*}; rest=${rest#*"$tab"}
      hold4=${rest#*"$tab"}
      case "$hold4" in *"$tab"*) continue ;; esac
      printf '%s\t%s\n' "$hold1" "$identity1" >>"$hold_records"
      printf '%s\t%s\n' "$hold2" "$identity2" >>"$hold_records"
      printf '%s\t%s\n' "$hold3" "$identity3" >>"$hold_records"
      printf '%s\t%s\n' "$hold4" "$identity4" >>"$hold_records"
      printf '%s\t%s\n' "$lock1" "$identity1" >>"$fixed_records"
      printf '%s\t%s\n' "$lock2" "$identity2" >>"$fixed_records"
      printf '%s\t%s\n' "$lock3" "$identity3" >>"$fixed_records"
      printf '%s\t%s\n' "$lock4" "$identity4" >>"$fixed_records"
      printf '%s\n' "$lock3" >>"$maintenance_records"
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
      hold1=""
      hold2=""
      hold3=""
      hold4=""
      ;;
  esac
  [ -n "$id" ] || continue
  if [ "$sub" = delete ]; then
    [ -n "$creation" ] || continue
    [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] && [ -n "$lock4" ] || continue
    run_locked "$id" "$creation" "$lock1" "$identity1" "$lock2" "$identity2" "$lock3" "$identity3" "$lock4" "$identity4" "$hold1" "$hold2" "$hold3" "$hold4"
  else
    run_with_timeout "$bin" "$sub" --force "$id" || true
  fi
done
cleanup_leases
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
	// leaseHolds are per-entry hard-link references. lockPaths remains the
	// stable fixed lease path sent to the shell; the holds let independent
	// reapers share an inode without one process unlinking another one's
	// lease during cleanup.
	leaseHolds []string
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

	mu         sync.Mutex
	registerMu sync.Mutex
	activeMu   sync.Mutex
	// activeHold is inherited by every child spawned for this reaper.
	activeHold    *reaperActiveHold
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	exited        chan struct{}
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
	closed        bool
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{binary: binary, subcommand: subcommand}
}

// ensureActiveHold creates one durable marker for this reaper process. The
// marker records the state-lock names covered by its registrations and is
// independent of the per-entry hard links, so a long-lived parent cannot
// lose a live registration to mtime-based lease GC.
func (r *reaper) ensureActiveHold() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeHold != nil && r.activeHold.file != nil {
		return nil
	}
	dir, err := nameLockDir()
	if err != nil {
		return err
	}
	hold, err := newReaperActiveHold(dir)
	if err != nil {
		return err
	}
	r.activeHold = hold
	return nil
}

func (r *reaper) addActiveRaw(raw string) error {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeHold == nil {
		return errors.New("reaper: missing process active lease")
	}
	return r.activeHold.addRaw(raw)
}

func (r *reaper) removeActiveRaw(raw string, barriersHeld bool) error {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeHold == nil {
		return nil
	}
	return r.activeHold.removeRaw(raw, barriersHeld)
}

func (r *reaper) releaseActiveIfUnused(barriersHeld bool) error {
	r.registerMu.Lock()
	defer r.registerMu.Unlock()
	return r.releaseActiveIfUnusedLocked(barriersHeld)
}

func (r *reaper) releaseActiveIfUnusedLocked(barriersHeld bool) error {
	r.mu.Lock()
	unused := len(r.entries) == 0 && r.cmd == nil
	r.mu.Unlock()
	if !unused {
		return nil
	}
	r.activeMu.Lock()
	hold := r.activeHold
	r.activeHold = nil
	r.activeMu.Unlock()
	return hold.release(barriersHeld)
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
	r.registerMu.Lock()
	defer r.registerMu.Unlock()
	keepActive := false
	activeRaw := ""
	activeRawAdded := false
	defer func() {
		if activeRawAdded {
			if err := r.removeActiveRaw(activeRaw, false); err != nil {
				log.Printf("container-go: reaper active registration cleanup %s: %v", activeRaw, err)
			}
		}
		if keepActive {
			return
		}
		if err := r.releaseActiveIfUnusedLocked(false); err != nil {
			log.Printf("container-go: reaper active lease cleanup after registration failure: %v", err)
		}
	}()

	var lockPaths, lockIdentities, leaseHolds []string
	if r.subcommand == "delete" {
		if err := r.ensureActiveHold(); err != nil {
			return fmt.Errorf("reaper: prepare active lease: %w", err)
		}
		rawState, err := rawNameLockPath(id)
		if err != nil {
			return fmt.Errorf("reaper: resolve active lease for %q: %w", id, err)
		}
		activeRaw = filepath.Base(rawState)
		if err := r.addActiveRaw(activeRaw); err != nil {
			return fmt.Errorf("reaper: register active lease for %q: %w", id, err)
		}
		activeRawAdded = true
		lockPaths, lockIdentities, leaseHolds, err = reaperNameLockSetForReaper(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", id, err)
		}
		if len(lockPaths) != 4 || len(lockIdentities) != 4 || len(leaseHolds) != 4 {
			_ = releaseReaperLeaseFiles(lockPaths, lockIdentities, leaseHolds, false)
			return fmt.Errorf("reaper: got %d/%d/%d lock barriers for %q, want 4/4/4", len(lockPaths), len(lockIdentities), len(leaseHolds), id)
		}
		for i, path := range lockPaths {
			if !validNameLockProtocolPath(path) || !validNameLockProtocolIdentity(lockIdentities[i]) {
				_ = releaseReaperLeaseFiles(lockPaths, lockIdentities, leaseHolds, false)
				return fmt.Errorf("reaper: invalid name lock lease for %q", id)
			}
		}
	}

	stateLockPath, maintenancePath := "", ""
	if len(lockPaths) == 4 {
		stateLockPath, maintenancePath = lockPaths[3], lockPaths[2]
	}
	entry := reaperEntry{
		id: id, creation: creation, lockPaths: lockPaths, lockIdentities: lockIdentities,
		leaseHolds: leaseHolds, stateLockPath: stateLockPath, maintenancePath: maintenancePath,
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		leaseErr := releaseReaperLeaseFiles(lockPaths, lockIdentities, leaseHolds, false)
		return errors.Join(errors.New("reaper: closed"), leaseErr)
	}
	r.entries = append(r.entries, entry)
	if r.stdin != nil {
		if r.writeLocked(entry) == nil {
			keepActive = true
			activeRawAdded = false
			r.mu.Unlock()
			return nil
		}
	}
	err := r.respawnAndReplayLocked()
	if err != nil {
		for i := len(r.entries) - 1; i >= 0; i-- {
			if sameReaperEntry(r.entries[i], entry) {
				r.entries = append(r.entries[:i], r.entries[i+1:]...)
				break
			}
		}
	} else {
		keepActive = true
		activeRawAdded = false
	}
	r.mu.Unlock()
	if err != nil {
		if releaseErr := releaseReaperLeaseFiles(lockPaths, lockIdentities, leaseHolds, false); releaseErr != nil {
			log.Printf("container-go: reaper lease cleanup %s: %v", id, releaseErr)
		}
	}
	return err
}

func sameReaperEntry(a, b reaperEntry) bool {
	return a.id == b.id && a.creation == b.creation
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
		if len(e.leaseHolds) != 4 {
			return fmt.Errorf("reaper: incomplete reaper lease holds for %q", e.id)
		}
		for _, hold := range e.leaseHolds {
			if !validNameLockProtocolPath(hold) {
				return fmt.Errorf("reaper: invalid reaper lease hold for %q", e.id)
			}
			line += "\t" + hold
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
	if err := r.prepareEntriesLocked(); err != nil {
		return err
	}
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

// prepareEntriesLocked refreshes leases after an unexpected child exit.
// An exited reaper cannot be using its old inode, so retaining the stale
// path would only allow cleanup to split the coordination namespace.
func (r *reaper) prepareEntriesLocked() error {
	for i := range r.entries {
		if r.subcommand != "delete" || len(r.entries[i].leaseHolds) == 4 {
			continue
		}
		paths, identities, holds, err := reaperNameLockSetForReaper(r.entries[i].id)
		if err != nil {
			return fmt.Errorf("reaper: refresh name leases for %q: %w", r.entries[i].id, err)
		}
		if len(paths) != 4 || len(identities) != 4 || len(holds) != 4 {
			_ = releaseReaperLeaseFiles(paths, identities, holds, false)
			return fmt.Errorf("reaper: refreshed %d/%d/%d name leases for %q, want 4/4/4", len(paths), len(identities), len(holds), r.entries[i].id)
		}
		r.entries[i].lockPaths = paths
		r.entries[i].lockIdentities = identities
		r.entries[i].leaseHolds = holds
		r.entries[i].stateLockPath = paths[3]
		r.entries[i].maintenancePath = paths[2]
	}
	return nil
}

func (r *reaper) spawnLocked() error {
	cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", r.binary, r.subcommand, breQuote(creationLabel))
	if r.subcommand == "delete" {
		r.activeMu.Lock()
		active := r.activeHold
		r.activeMu.Unlock()
		if active == nil || active.file == nil {
			return errors.New("reaper: missing process active lease")
		}
		cmd.ExtraFiles = []*os.File{active.file}
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
	go func() {
		_ = cmd.Wait()
		r.handleProcessExit(cmd)
		close(exited)
	}()
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
	return nil
}

func (r *reaper) handleProcessExit(cmd *exec.Cmd) {
	r.mu.Lock()
	if r.cmd != cmd {
		r.mu.Unlock()
		return
	}
	closed := r.closed
	entries := append([]reaperEntry(nil), r.entries...)
	r.cmd, r.stdin = nil, nil
	var active *reaperActiveHold
	r.activeMu.Lock()
	if closed {
		active = r.activeHold
		r.activeHold = nil
	}
	r.activeMu.Unlock()
	if closed {
		r.entries = nil
	} else {
		for i := range r.entries {
			r.entries[i].lockPaths = nil
			r.entries[i].lockIdentities = nil
			r.entries[i].leaseHolds = nil
			r.entries[i].stateLockPath = ""
			r.entries[i].maintenancePath = ""
		}
	}
	r.mu.Unlock()
	for _, entry := range entries {
		if err := releaseReaperLeaseFiles(entry.lockPaths, entry.lockIdentities, entry.leaseHolds, false); err != nil {
			log.Printf("container-go: reaper lease cleanup on exit %s: %v", entry.id, err)
		}
	}
	if active != nil {
		if err := active.release(false); err != nil {
			log.Printf("container-go: reaper active lease cleanup on exit: %v", err)
		}
	}
}

// closeStdin hands the reaper the same EOF it would see on parent
// death. Test hook and best-effort shutdown. Lease reclamation is done by
// the exit watcher after the child has stopped using the paths.
func (r *reaper) closeStdin() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	stdin := r.stdin
	entries := append([]reaperEntry(nil), r.entries...)
	noChild := r.cmd == nil
	var active *reaperActiveHold
	if noChild {
		r.activeMu.Lock()
		active = r.activeHold
		r.activeHold = nil
		r.activeMu.Unlock()
		r.entries = nil
	}
	r.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if noChild {
		for _, entry := range entries {
			if err := releaseReaperLeaseFiles(entry.lockPaths, entry.lockIdentities, entry.leaseHolds, false); err != nil {
				log.Printf("container-go: reaper lease cleanup on close %s: %v", entry.id, err)
			}
		}
		if active != nil {
			if err := active.release(false); err != nil {
				log.Printf("container-go: reaper active lease cleanup on close: %v", err)
			}
		}
	}
}

// killForTest kills the reaper child and waits until it is reaped, so
// the next write deterministically fails. The exit watcher releases the
// old leases; a later registration refreshes them before replay.
func (r *reaper) killForTest() {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	if cmd != nil {
		_ = cmd.Process.Kill()
	}
	if exited != nil {
		<-exited
	}
}

// unregister removes a completed entry. When the caller still holds the
// four name barriers (the normal Terminate path), lease removal is done
// under that barrier and cannot race a reaper's critical section.
func (r *reaper) unregister(id, creation string, barriersHeld bool) error {
	if r.subcommand == "delete" {
		if !nameRE.MatchString(id) || !creationRE.MatchString(creation) {
			return fmt.Errorf("reaper: invalid name-addressed unregister %q", id)
		}
	} else if !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid immutable unregister %q", id)
	}
	r.mu.Lock()
	index := -1
	for i, entry := range r.entries {
		if entry.id == id && entry.creation == creation {
			index = i
			break
		}
	}
	if index < 0 {
		r.mu.Unlock()
		return nil
	}
	entry := r.entries[index]
	r.entries = append(r.entries[:index], r.entries[index+1:]...)
	r.mu.Unlock()
	leaseErr := releaseReaperLeaseFiles(entry.lockPaths, entry.lockIdentities, entry.leaseHolds, barriersHeld)
	var activeRaw string
	if base := filepath.Base(entry.stateLockPath); base != "." && strings.HasSuffix(base, nameLockLeaseSuffix) {
		activeRaw = strings.TrimSuffix(base, nameLockLeaseSuffix)
	}
	if activeRaw == "" && r.subcommand == "delete" {
		if rawState, rawErr := rawNameLockPath(entry.id); rawErr == nil {
			activeRaw = filepath.Base(rawState)
		}
	}
	var activeStateErr error
	if activeRaw != "" {
		activeStateErr = r.removeActiveRaw(activeRaw, barriersHeld)
	}
	activeErr := r.releaseActiveIfUnused(barriersHeld)
	return errors.Join(leaseErr, activeStateErr, activeErr)
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

func unregisterFromGlobalReaper(binary, subcommand, id, creation string, barriersHeld bool) {
	if runtime.GOOS == "windows" {
		return
	}
	globalReapersMu.Lock()
	r := globalReapers[binary]
	globalReapersMu.Unlock()
	if r == nil || r.subcommand != subcommand {
		return
	}
	if err := r.unregister(id, creation, barriersHeld); err != nil {
		log.Printf("container-go: reaper unregister %s: %v", id, err)
	}
}
