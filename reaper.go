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
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated as an Apple Container name or a full Docker container
// ID, and the script itself disables globbing and quotes every expansion
// the IDs reach.
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
// delete necessarily goes by name. Name-addressed entries carry the
// stable per-name lock path used by the library; the child acquires that
// lock before inspect and delete, so a cooperating create/prune/cleanup
// cannot replace the name in the middle of the guarded operation.
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
run_locked() {
  id="$1"
  creation="$2"
  lockpath="$3"
  [ -n "$lockpath" ] || return 0
  [ -L "$lockpath" ] && return 0
  [ -f "$lockpath" ] || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  [ -n "$lockf_bin" ] || return 0
  REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" \
    "$lockf_bin" -k -w -t 30 "$lockpath" sh -c '
      id=$1
      creation=$2
      bin=$REAPER_BIN
      sub=$REAPER_SUB
      key=$REAPER_KEY
      for once in 1; do
        target="$id"
        if [ -n "$creation" ]; then
          tmp=$(mktemp 2>/dev/null) || exit 0
          ("$bin" inspect "$id" >"$tmp" 2>/dev/null & pid=$!; (sleep 10; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || { rm -f "$tmp"; exit 0; }
          got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
          uid=$(sed -n "s/^[[:space:]]*\"Id\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
          rm -f "$tmp"
          [ "$got" = "$creation" ] || continue
          [ -n "$uid" ] && target="$uid"
        fi
        ("$bin" "$sub" --force "$target" >/dev/null 2>&1 & pid=$!; (sleep 30; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || true
      done
    ' sh "$id" "$creation" >/dev/null 2>&1 || true
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
    *)
      id=$line
      creation=""
      lockpath=""
      ;;
  esac
  [ -n "$id" ] || continue
  case "$sub" in
    delete)
      [ -n "$lockpath" ] || continue
      run_locked "$id" "$creation" "$lockpath"
      ;;
    rm)
      if [ -n "$creation" ]; then
        [ -n "$lockpath" ] || continue
        run_locked "$id" "$creation" "$lockpath"
      else
        [ -n "$lockpath" ] && continue
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

// validReaperID keeps the backend's address space explicit. Docker's
// immutable IDs are 64 hex characters, while Apple Container addresses
// containers by its shorter name rule.
func validReaperID(subcommand, id string) bool {
	if nameRE.MatchString(id) {
		return true
	}
	return subcommand == "rm" && dockerIDRE.MatchString(id)
}

func validReaperLockPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}

type reaperEntry struct {
	id       string
	creation string
	// lockPath is present for name-addressed entries. It is the same
	// persistent path used by lockName; immutable-ID entries leave it
	// empty and do not need a name lock.
	lockPath string
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
// respawning the reaper process as needed. Apple targets are names;
// Docker targets may be the full 64-hex ID printed by docker run. creation
// is the generation ID from creationLabel; empty skips the generation
// check for backward compatibility.
func (r *reaper) register(id, creation string) error {
	if !validReaperID(r.subcommand, id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}

	lockPath := ""
	// Apple deletes by name even when an old caller omitted the
	// generation. Always prepare a lock for that subcommand so the shell
	// protocol can never take an unlocked name-delete path.
	if r.subcommand == "delete" || creation != "" {
		var err error
		lockPath, err = reaperNameLockPath(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name lock for %q: %w", id, err)
		}
		if lockPath != "" && !validReaperLockPath(lockPath) {
			return fmt.Errorf("reaper: invalid name lock path %q", lockPath)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation, lockPath: lockPath}
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
	if e.lockPath != "" {
		if !validReaperLockPath(e.lockPath) {
			return fmt.Errorf("reaper: invalid name lock for %q", e.id)
		}
		line += "\t" + e.creation + "\t" + e.lockPath
	} else if e.creation != "" {
		return fmt.Errorf("reaper: missing name lock for %q", e.id)
	}
	n, err := io.WriteString(r.stdin, line+"\n")
	if err == nil && n != len(line)+1 {
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
// process-wide reaper for its backend binary. Reaper trouble is logged
// but never fails container startup. The reaper needs /bin/sh, so on
// Windows this is a no-op and cleanup relies on the normal paths.
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
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
	}
}
