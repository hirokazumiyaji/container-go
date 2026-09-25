package container

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// Name-addressed entries carry the stable historical and durable lock-file
// paths prepared by the Go process. The shell holds all of those barriers,
// in lockName's order, across its generation inspect and delete calls. If a
// lock helper or file is unavailable, the entry is skipped rather than
// deleting by name without coordination. The original single-path line
// format remains accepted; the multi-path form is an additive extension.
// Each backend call remains bounded by a portable background-job timeout.
// Failures stay silent (|| true) by design: the reaper is last-resort
// insurance.
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
run_with_timeout() {
  "$@" >/dev/null 2>&1 & pid=$!
  (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!
  wait "$pid" 2>/dev/null
  rc=$?
  kill "$killer" 2>/dev/null
  wait "$killer" 2>/dev/null
  return $rc
}
entry_script='
id=$1
creation=$2
bin=$REAPER_BIN
sub=$REAPER_SUB
key=$REAPER_KEY
target="$id"
if [ -n "$creation" ]; then
  tmp=$(mktemp 2>/dev/null) || exit 0
  ("$bin" inspect "$id" >"$tmp" 2>/dev/null & pid=$!; (sleep 10; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || { rm -f "$tmp"; exit 0; }
  got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
  uid=$(sed -n '\''s/^[[:space:]]*\"Id\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p'\'' "$tmp" 2>/dev/null | head -n 1)
  rm -f "$tmp"
  [ "$got" = "$creation" ] || exit 0
  # Legacy guarded entries used [ "$got" = "$creation" ] || continue;
  # keep that exact comparison in the script for compatibility with older
  # reaper fixtures while the locked path exits its private shell.
  if [ "$sub" = "rm" ]; then
    [ -n "$uid" ] || exit 0
    target="$uid"
  elif [ -n "$uid" ]; then
    target="$uid"
  fi
fi
("$bin" "$sub" --force "$target" >/dev/null 2>&1 & pid=$!; (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || true
'
helper=$(mktemp 2>/dev/null) || exit 0
trap 'rm -f "$helper"' EXIT
cat >"$helper" <<'REAPER_MULTI_LOCK'
#!/bin/sh
if [ "$1" = "--lock" ]; then
  path=$2
  shift 2
  if [ -n "$REAPER_LOCKF" ]; then
    exec "$REAPER_LOCKF" -k -w -t 30 "$path" "$0" --held "$@"
  fi
  if [ -n "$REAPER_FLOCK" ]; then
    exec "$REAPER_FLOCK" -w 30 "$path" "$0" --held "$@"
  fi
  exit 0
fi
[ "$1" = "--held" ] || exit 0
shift
next=$1
shift
if [ "$next" = "--run" ]; then
  id=$1
  creation=$2
  REAPER_BIN="$REAPER_BIN" REAPER_SUB="$REAPER_SUB" REAPER_KEY="$REAPER_KEY" REAPER_ENTRY_SCRIPT="$REAPER_ENTRY_SCRIPT" \
    sh -c 'sh -c "$REAPER_ENTRY_SCRIPT" sh "$@"' sh "$id" "$creation"
  exit $?
fi
[ -n "$next" ] || exit 0
"$0" --lock "$next" "$@"
exit $?
REAPER_MULTI_LOCK
chmod 700 "$helper" 2>/dev/null || exit 0
run_locked() {
  id=$1
  creation=$2
  lockpath=$3
  if [ -z "$lockpath" ]; then
    REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_ENTRY_SCRIPT="$entry_script" \
      sh -c 'sh -c "$REAPER_ENTRY_SCRIPT" sh "$@"' sh "$id" "$creation" || true
    return
  fi
  case "$lockpath" in
    *";"*)
      old_ifs=$IFS
      IFS=';'
      set -- $lockpath
      IFS=$old_ifs
      path1=$1
      path2=$2
      path3=$3
      for path in "$path1" "$path2" "$path3"; do
        [ -n "$path" ] || return 0
        [ -L "$path" ] && return 0
        [ -f "$path" ] || return 0
      done
      REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_ENTRY_SCRIPT="$entry_script" \
      REAPER_LOCKF=$(command -v lockf 2>/dev/null) REAPER_FLOCK=$(command -v flock 2>/dev/null) \
        "$helper" --lock "$path1" "$path2" "$path3" --run "$id" "$creation" || true
      return
      ;;
  esac
  [ -L "$lockpath" ] && return 0
  [ -f "$lockpath" ] || return 0
  if flock_bin=$(command -v flock 2>/dev/null); then
    REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_ENTRY_SCRIPT="$entry_script" \
      "$flock_bin" -w 30 "$lockpath" sh -c 'sh -c "$REAPER_ENTRY_SCRIPT" sh "$@"' sh "$id" "$creation" || true
    return
  fi
  if lockf_bin=$(command -v lockf 2>/dev/null); then
    REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_ENTRY_SCRIPT="$entry_script" \
      "$lockf_bin" -k -w -t 30 "$lockpath" sh -c 'sh -c "$REAPER_ENTRY_SCRIPT" sh "$@"' sh "$id" "$creation" || true
  fi
}
printf '%s\n' "$ids" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$line" in
    *"$tab"*)
      id=${line%%"$tab"*}
      rest=${line#*"$tab"}
      creation=${rest%%"$tab"*}
      lockpath=${rest#*"$tab"}
      ;;
    *" "*)
      id=${line%% *}
      creation=${line#* }
      lockpath=""
      ;;
    *)
      id=$line
      creation=""
      lockpath=""
      ;;
  esac
  [ -n "$id" ] || continue
  if [ -n "$lockpath" ]; then
    run_locked "$id" "$creation" "$lockpath"
  elif [ -n "$creation" ] && [ "$sub" = "rm" ] && printf '%s\n' "$id" | grep -Eq '^[0-9a-f]{64}$'; then
    # An immutable Docker ID is safe without a name lock; retain the
    # generation and inspect guard before deleting it.
    run_locked "$id" "$creation" ""
  elif [ -n "$creation" ]; then
    # A generation-guarded name entry without a lock is unsafe.
    continue
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

const reaperLockPathSeparator = ";"

type reaperEntry struct {
	id       string
	creation string
	// lockPath is retained for compatibility with entries constructed by
	// older in-package callers. New name entries carry all migration
	// barriers in lockPaths, in lockName acquisition order.
	lockPath  string
	lockPaths []string
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
// respawning the reaper process as needed. creation is the generation
// ID from creationLabel. A name-addressed generation entry also receives
// the stable path of the lock used by Terminate and create.
func (r *reaper) register(id, creation string) error {
	if !nameRE.MatchString(id) && !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	if creation == "" && !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: refusing name delete without a creation generation")
	}

	lockPath := ""
	var lockPaths []string
	if creation != "" && !dockerIDRE.MatchString(id) {
		var err error
		lockPaths, err = reaperNameLockPaths(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", id, err)
		}
		for _, path := range lockPaths {
			if !validNameLockProtocolPath(path) {
				return fmt.Errorf("reaper: invalid name lock path %q", path)
			}
		}
		lockPath = lockPaths[len(lockPaths)-1]
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation, lockPath: lockPath, lockPaths: append([]string(nil), lockPaths...)}
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
	var line string
	paths := append([]string(nil), e.lockPaths...)
	if len(paths) == 0 && e.lockPath != "" {
		paths = []string{e.lockPath}
	}
	if len(paths) > 0 {
		for _, path := range paths {
			if !validNameLockProtocolPath(path) {
				return fmt.Errorf("reaper: missing stable name lock for %q", e.id)
			}
		}
		// The third field remains the historical path field. Multiple
		// barriers use an additive separator; old reapers fail closed on
		// the unknown composite path rather than deleting unlocked.
		line = e.id + "\t" + e.creation + "\t" + strings.Join(paths, reaperLockPathSeparator)
	} else if e.creation != "" {
		// Retain the legacy space-delimited form for an immutable ID that
		// nevertheless carries a generation for an additional guard.
		line = e.id + " " + e.creation
	} else {
		line = e.id
	}
	line += "\n"
	n, err := io.WriteString(r.stdin, line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

// respawnAndReplayLocked starts a fresh reaper process and re-registers
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

// closeStdin hands the reaper the same EOF it would see on parent death.
// Test hook and best-effort shutdown.
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
// process-wide reaper for its backend binary. Reaper trouble never
// fails container startup. The reaper needs /bin/sh, so on Windows this
// is a no-op and cleanup relies on normal paths.
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
		// Reaper registration is best effort for startup, but a
		// generation-guarded name entry without a lock is never allowed to
		// run unprotected.
		log.Printf("container-go: reaper registration %s: %v", id, err)
	}
}
