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
// Name-addressed entries carry library-prepared durable state and
// maintenance lock paths. The shell holds maintenance first and state
// second, matching lockName, across generation inspection and deletion.
// If lockf is unavailable, the entry is skipped rather than deleted
// without coordination. Docker's immutable-ID entries remain lock-free.
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
if [ "${REAPER_STATE_HELD:-}" != "1" ]; then
  REAPER_STATE_HELD=1 exec "$REAPER_LOCKF" -k -n -w -t 30 "$1" "$0" "$@"
fi
id=$2
creation=$3
bin=$REAPER_BIN
sub=$REAPER_SUB
key=$REAPER_KEY
tmp=$(mktemp 2>/dev/null) || exit 0
("$bin" inspect "$id" >"$tmp" 2>/dev/null & pid=$!; (sleep 10; kill -9 "$pid" 2>/dev/null) & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null; wait "$killer" 2>/dev/null; exit "$rc") || { rm -f "$tmp"; exit 0; }
got=$(sed -n "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/\1/p" "$tmp" 2>/dev/null | head -n 1)
rm -f "$tmp"
[ "$got" = "$creation" ] || exit 0
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
run_locked() {
  id="$1"
  creation="$2"
  statepath="$3"
  maintenancepath="$4"
  [ -n "$statepath" ] && [ -n "$maintenancepath" ] || return 0
  [ -L "$statepath" ] && return 0
  [ -L "$maintenancepath" ] && return 0
  [ -f "$statepath" ] && [ -f "$maintenancepath" ] || return 0
  lockf_bin=$(command -v lockf 2>/dev/null) || return 0
  [ -n "$lockf_bin" ] || return 0
  REAPER_STATE_HELD=0 REAPER_BIN="$bin" REAPER_SUB="$sub" REAPER_KEY="$key" REAPER_LOCKF="$lockf_bin" REAPER_HELPER="$helper" \
    "$lockf_bin" -k -n -w -t 30 "$maintenancepath" "$helper" "$statepath" "$id" "$creation" >/dev/null 2>&1 || true
}
printf '%s\n' "$ids" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$line" in
    *"$tab"*)
      id=${line%%"$tab"*}
      rest=${line#*"$tab"}
      creation=${rest%%"$tab"*}
      rest=${rest#*"$tab"}
      statepath=${rest%%"$tab"*}
      maintenancepath=${rest#*"$tab"}
      ;;
    *)
      id=$line
      creation=""
      statepath=""
      maintenancepath=""
      ;;
  esac
  [ -n "$id" ] || continue
  if [ "$sub" = delete ]; then
    [ -n "$creation" ] || continue
    [ -n "$statepath" ] && [ -n "$maintenancepath" ] || continue
    run_locked "$id" "$creation" "$statepath" "$maintenancepath"
  elif [ -n "$statepath" ]; then
    [ -n "$maintenancepath" ] || continue
    run_locked "$id" "$creation" "$statepath" "$maintenancepath"
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

func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}

type reaperEntry struct {
	id              string
	creation        string
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
// carry a valid generation and prepared stable lock paths.
func (r *reaper) register(id, creation string) error {
	if r.subcommand == "delete" {
		if !nameRE.MatchString(id) {
			return fmt.Errorf("reaper: invalid container id %q", id)
		}
	} else if !dockerIDRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid immutable container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	if runtime.GOOS == "windows" {
		return nil
	}

	statePath, maintenancePath := "", ""
	if r.subcommand == "delete" {
		if creation == "" {
			return fmt.Errorf("reaper: missing creation generation for name-addressed entry %q", id)
		}
		var err error
		statePath, maintenancePath, err = reaperNameLockPaths(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", id, err)
		}
		if !validNameLockProtocolPath(statePath) || !validNameLockProtocolPath(maintenancePath) {
			return fmt.Errorf("reaper: invalid name lock path for %q", id)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation, stateLockPath: statePath, maintenancePath: maintenancePath}
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
	if e.stateLockPath != "" && (!validNameLockProtocolPath(e.stateLockPath) || !validNameLockProtocolPath(e.maintenancePath)) {
		return fmt.Errorf("reaper: invalid stable name locks for %q", e.id)
	}
	line := e.id + "\t" + e.creation + "\t" + e.stateLockPath + "\t" + e.maintenancePath + "\n"
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
// container startup, but an Apple entry without valid stable locks is not
// registered and therefore cannot be deleted without coordination.
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
