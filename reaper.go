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
// The script is a fixed string; container IDs and library-generated lock
// paths enter it only as stdin data validated before registration, and
// the script itself disables globbing and quotes every expansion.
// Name-addressed entries require a creation generation and carry the
// legacy TMPDIR, transitional UserCacheDir, and durable account-state
// lock paths in that fixed order. The reaper uses the standard lockf(1)
// default-wait behavior to hold all three across inspect and delete, so
// old and new cooperating library revisions cannot replace a name in the
// middle of the operation. If lockf or any lock file is unavailable, the
// entry is skipped rather than deleted without coordination. Each backend
// call runs with a per-entry timeout implemented with background jobs and
// kill (timeout(1)
// is not standard on macOS), so a hung daemon cannot wedge deletion of
// later entries. Failures stay silent (|| true) by design: the reaper is
// last-resort insurance.
//
// When a creation generation is known, the script inspects first and
// reads the creation label as a structural JSON field: the match is
// anchored at line start on the quoted key, so label values or other
// text containing the same characters cannot satisfy it. When inspect
// also reports an immutable "Id" (Docker), the delete targets that ID
// instead of the name, so a same-name replacement created after the
// check is simply not found. Apple Container has no such ID; there the
// delete necessarily goes by name.
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
locked_command='
  lockpath=$1
  id=$2
  creation=$3
  [ -n "$lockpath" ] || exit 0
  [ -L "$lockpath" ] && exit 0
  [ -f "$lockpath" ] || exit 0
  bin=$REAPER_BIN
  sub=$REAPER_SUB
  key=$REAPER_KEY
  target="$id"
  if [ -n "$creation" ]; then
    tmp=$(mktemp 2>/dev/null) || exit 0
    ("$bin" inspect "$id" >"$tmp" 2>/dev/null & pid=$!; (sleep 10; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || { rm -f "$tmp"; exit 0; }
    got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
    uid=$(sed -n "s/^[[:space:]]*\"Id\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
    rm -f "$tmp"
    [ "$got" = "$creation" ] || exit 0
    [ -n "$uid" ] && target="$uid"
  fi
  ("$bin" "$sub" --force "$target" >/dev/null 2>&1 & pid=$!; (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || true
'
lock_three_command='
  lockf_bin=${1}
  lock3=${2}
  shift 2
  [ -n "$lock3" ] || exit 0
  [ -L "$lock3" ] && exit 0
  [ -f "$lock3" ] || exit 0
  "$lockf_bin" -k -t 30 "$lock3" sh -c "$REAPER_LOCKED_COMMAND" \
    reaper-locked "$lock3" "$@"
'
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
  lock2="$4"
  lock3="$5"
  valid_lock_path "$lock1" || return 0
  valid_lock_path "$lock2" || return 0
  valid_lock_path "$lock3" || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  [ -n "$lockf_bin" ] || return 0
  REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
    REAPER_LOCKED_COMMAND="$locked_command" REAPER_LOCK_THREE_COMMAND="$lock_three_command" \
    "$lockf_bin" -k -t 30 "$lock1" sh -c '
      lockf_bin=${1}
      lock2=${2}
      lock3=${3}
      shift 3
      [ -n "$lock2" ] || exit 0
      [ -L "$lock2" ] && exit 0
      [ -f "$lock2" ] || exit 0
      [ -n "$lock3" ] || exit 0
      [ -L "$lock3" ] && exit 0
      [ -f "$lock3" ] || exit 0
      "$lockf_bin" -k -t 30 "$lock2" sh -c "$REAPER_LOCK_THREE_COMMAND" \
        reaper-lock-two "$lockf_bin" "$lock3" "$@"
    ' reaper-lock "$lockf_bin" "$lock2" "$lock3" "$id" "$creation" >/dev/null 2>&1 || true
}
printf '%s\n' "$ids" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$line" in
    *"$tab"*)
      id=${line%%"$tab"*}
      rest=${line#*"$tab"}
      creation=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lock1=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      lock2=${rest%%"$tab"*}
      lock3=${rest#*"$tab"}
      case "$lock3" in *"$tab"*) continue ;; esac
      ;;
    *)
      id=$line
      creation=""
      lock1=""
      lock2=""
      lock3=""
      ;;
  esac
  [ -n "$id" ] || continue
  case "$sub" in
    delete)
      [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] || continue
      run_locked "$id" "$creation" "$lock1" "$lock2" "$lock3"
      ;;
    rm)
      if [ -n "$creation" ]; then
        [ -n "$lock1" ] && [ -n "$lock2" ] && [ -n "$lock3" ] || continue
        run_locked "$id" "$creation" "$lock1" "$lock2" "$lock3"
      else
        [ -n "$lock1" ] && continue
        run_with_timeout "$bin" "$sub" --force "$id" || true
      fi
      ;;
    *)
      continue
      ;;
  esac
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

type reaperEntry struct {
	id       string
	creation string
	// lockPaths contains the legacy, transitional, and durable barriers
	// in acquisition order for name-addressed entries. Immutable-ID
	// entries leave it empty and do not need a name lock.
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
// respawning the reaper process as needed. Name-addressed entries require
// a valid creation generation and carry all compatibility lock barriers.
// A generation-less entry is accepted only for a full immutable Docker ID.
func (r *reaper) register(id, creation string) error {
	immutableID := r.subcommand == "rm" && dockerIDRE.MatchString(id)
	if !nameRE.MatchString(id) && !immutableID {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	if immutableID {
		creation = ""
	} else if creation == "" {
		return fmt.Errorf("reaper: name-addressed entry %q requires a creation generation", id)
	}

	var lockPaths []string
	if r.subcommand == "delete" || creation != "" {
		var err error
		lockPaths, err = reaperNameLockPaths(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", id, err)
		}
		if len(lockPaths) != 3 {
			return fmt.Errorf("reaper: got %d name lock barriers for %q, want 3", len(lockPaths), id)
		}
		for _, path := range lockPaths {
			if !validNameLockProtocolPath(path) {
				return fmt.Errorf("reaper: invalid name lock path %q", path)
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation, lockPaths: lockPaths}
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
	if len(e.lockPaths) > 0 {
		if len(e.lockPaths) != 3 {
			return fmt.Errorf("reaper: got %d name lock barriers for %q, want 3", len(e.lockPaths), e.id)
		}
		line += "\t" + e.creation
		for _, path := range e.lockPaths {
			if !validNameLockProtocolPath(path) {
				return fmt.Errorf("reaper: invalid stable name lock for %q", e.id)
			}
			line += "\t" + path
		}
	} else if e.creation != "" {
		return fmt.Errorf("reaper: missing stable name locks for %q", e.id)
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
// process-wide reaper for its backend binary. Reaper trouble never
// fails container startup. The reaper needs /bin/sh, so on Windows
// this is a no-op and cleanup relies on the normal paths.
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
		// A name-addressed entry is not safe without its stable lock
		// file, so registration failure is deliberately fail-closed. The
		// best-effort reaper contract still leaves normal Run/Cleanup
		// paths available to the caller.
		log.Printf("container-go: reaper registration %s: %v", id, err)
	}
}
