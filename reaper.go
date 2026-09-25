package container

import (
	"context"
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
// The script is a fixed string; container IDs and library-generated lock
// paths enter it only as stdin data validated before registration, and
// the script itself disables globbing and quotes every expansion.
// Name-addressed entries require a creation generation and carry the
// unique legacy TMPDIR, transitional UserCacheDir, and durable
// account-state lock paths in that order. The reaper uses lockf(1) on
// macOS and flock(1) elsewhere to hold every barrier across inspect and
// delete, so old and new cooperating library revisions cannot replace a
// name in the middle of the operation. If that helper or any lock file
// is unavailable, the Apple entry is skipped rather than deleted without
// coordination. Each backend call runs with a per-entry timeout
// implemented with background jobs and kill (timeout(1) is not standard
// on macOS), so a hung daemon cannot wedge deletion of later entries.
// Failures stay silent (|| true) by design: the reaper is last-resort
// insurance.
//
// When a creation generation is known, the script inspects first and
// reads the creation label as a structural JSON field: the match is
// anchored at line start on the quoted key, so label values or other
// text containing the same characters cannot satisfy it. Apple Container
// has no immutable ID, so its guarded delete necessarily goes by name.
// Full Docker IDs never acquire Apple name locks; when a generation is
// supplied, the script verifies it and still deletes by the immutable ID.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
lock_helper="$4"
timeout="${5:-30}"
inspect_timeout="${6:-10}"
delete_timeout="${7:-30}"
case "$inspect_timeout" in
  ''|*[!0-9]*) inspect_timeout=10 ;;
esac
case "$delete_timeout" in
  ''|*[!0-9]*) delete_timeout=30 ;;
esac
ids=""
tab=$(printf '\t')
while IFS= read -r line; do
  ids="$ids
$line"
done
locked_command='
  inspect_timeout="${REAPER_INSPECT_TIMEOUT:-10}"
  delete_timeout="${REAPER_DELETE_TIMEOUT:-30}"
  kill_backend_tree() {
    kill_root=$1
    kill_depth=${2:-0}
    [ "$kill_depth" -lt 32 ] || return 0
    kill_children=$(pgrep -P "$kill_root" 2>/dev/null || true)
    for kill_child in $kill_children; do
      case "$kill_child" in
        ""|*[!0-9]*) continue ;;
      esac
      kill_backend_tree "$kill_child" "$((kill_depth + 1))"
    done
    kill -KILL "$kill_root" 2>/dev/null || true
  }
  lockpath=$1
  expected=$2
  shift 2
  if [ "$lockpath" != "-" ]; then
    [ -n "$lockpath" ] || exit 0
    [ -L "$lockpath" ] && exit 0
    [ -f "$lockpath" ] || exit 0
    lock_identity() {
      case "$REAPER_LOCK_HELPER" in
        lockf) stat -f "%d:%i" "$1" 2>/dev/null ;;
        flock) stat -c "%d:%i" "$1" 2>/dev/null ;;
        *) return 1 ;;
      esac
    }
    [ "$(lock_identity "$lockpath")" = "$expected" ] || exit 0
    while [ "$#" -gt 5 ] && { [ -z "$1" ] || [ "$1" = "-" ]; }; do
      shift 2
    done
    if [ "$#" -gt 5 ]; then
      next_lock=$1
      next_identity=$2
      shift 2
      REAPER_LOCK_HELPER="$REAPER_LOCK_HELPER" \
      REAPER_LOCK_BIN="$REAPER_LOCK_BIN" \
      REAPER_LOCK_FLAG1="$REAPER_LOCK_FLAG1" \
      REAPER_LOCK_FLAG2="$REAPER_LOCK_FLAG2" \
      REAPER_LOCK_TIMEOUT="$REAPER_LOCK_TIMEOUT" \
      REAPER_INSPECT_TIMEOUT="$REAPER_INSPECT_TIMEOUT" \
      REAPER_DELETE_TIMEOUT="$REAPER_DELETE_TIMEOUT" \
      REAPER_LOCKED_COMMAND="$REAPER_LOCKED_COMMAND" \
        "$REAPER_LOCK_BIN" "$REAPER_LOCK_FLAG1" "$REAPER_LOCK_FLAG2" "$REAPER_LOCK_TIMEOUT" \
        "$next_lock" sh -c "$REAPER_LOCKED_COMMAND" reaper-locked "$next_lock" "$next_identity" "$@"
      exit $?
    fi
  fi
  id=$1
  creation=$2
  bin=$3
  sub=$4
  key=$5
  target="$id"
  if [ -n "$creation" ]; then
    inspect_fields=$(
      {
        "$bin" inspect "$id" 2>/dev/null & inspect_pid=$!
        (sleep "$inspect_timeout"; kill_backend_tree "$inspect_pid") >/dev/null 2>&1 & killer=$!
        wait "$inspect_pid" 2>/dev/null
        inspect_rc=$?
        kill "$killer" 2>/dev/null || true
        printf "\n__containergo_inspect_rc__%s\n" "$inspect_rc"
      } | sed -n \
        -e "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/creation=\1/p" \
        -e "s/^[[:space:]]*\"Id\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/id=\1/p" \
        -e "s/^__containergo_inspect_rc__\([0-9][0-9]*\)$/inspect_rc=\1/p"
    )
    got=$(printf "%s\n" "$inspect_fields" | sed -n "s/^creation=//p" | head -n 1)
    uid=$(printf "%s\n" "$inspect_fields" | sed -n "s/^id=//p" | head -n 1)
    inspect_rc=$(printf "%s\n" "$inspect_fields" | sed -n "s/^inspect_rc=//p" | tail -n 1)
    unset inspect_fields
    [ "$inspect_rc" = 0 ] || exit 0
    [ "$got" = "$creation" ] || exit 0
    case "$sub" in
      delete) ;;
      rm)
        [ "$uid" = "$id" ] || exit 0
        target="$uid"
        ;;
      *) exit 0 ;;
    esac
  fi
  ("$bin" "$sub" --force "$target" >/dev/null 2>&1 & pid=$!; (sleep "$delete_timeout"; kill_backend_tree "$pid") >/dev/null 2>&1 & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null || true; exit "$rc") || true
'
lock_bin=""
lock_flag1=""
lock_flag2=""
case "$lock_helper" in
  lockf)
    lock_bin=$(command -v lockf 2>/dev/null || true)
    lock_flag1=-k
    lock_flag2=-t
    ;;
  flock)
    lock_bin=$(command -v flock 2>/dev/null || true)
    lock_flag1=-x
    lock_flag2=-w
    ;;
  *)
    exit 0
    ;;
esac
run_locked() {
  [ -n "$lock_bin" ] || return 0
  id=$1
  creation=$2
  shift 2
  set -- "$@" "$id" "$creation" "$bin" "$sub" "$key"
  while [ "$#" -gt 5 ] && { [ -z "$1" ] || [ "$1" = "-" ]; }; do
    shift 2
  done
  [ "$#" -ge 7 ] || return 0
  lock=$1
  identity=$2
  shift 2
  REAPER_LOCK_HELPER="$lock_helper" \
  REAPER_LOCK_BIN="$lock_bin" \
  REAPER_LOCK_FLAG1="$lock_flag1" \
  REAPER_LOCK_FLAG2="$lock_flag2" \
  REAPER_LOCK_TIMEOUT="$timeout" \
  REAPER_INSPECT_TIMEOUT="$inspect_timeout" \
  REAPER_DELETE_TIMEOUT="$delete_timeout" \
  REAPER_LOCKED_COMMAND="$locked_command" \
    "$lock_bin" "$lock_flag1" "$lock_flag2" "$timeout" "$lock" \
    sh -c "$locked_command" reaper-locked "$lock" "$identity" "$@" >/dev/null 2>&1 || true
}
run_guarded() {
  id=$1
  creation=$2
  REAPER_INSPECT_TIMEOUT="$inspect_timeout" \
  REAPER_DELETE_TIMEOUT="$delete_timeout" \
    sh -c "$locked_command" reaper-locked - - "$id" "$creation" "$bin" "$sub" "$key" >/dev/null 2>&1 || true
}
run_unlocked() {
  ("$@" >/dev/null 2>&1 & pid=$!; (sleep "$delete_timeout"; kill_backend_tree "$pid") >/dev/null 2>&1 & killer=$!; wait "$pid" 2>/dev/null; rc=$?; kill "$killer" 2>/dev/null || true; exit "$rc") || true
}
printf "%s\n" "$ids" | while IFS= read -r line; do
  [ -z "$line" ] && continue
  old_ifs=$IFS
  IFS=$tab
  set -f
  set -- $line
  IFS=$old_ifs
  [ "$#" -eq 8 ] || continue
  id=$1
  creation=$2
  [ "$creation" = "-" ] && creation=""
  shift 2
  if [ -n "$1" ] && [ "$1" != "-" ]; then
    run_locked "$id" "$creation" "$@"
    continue
  fi
  if [ -n "$creation" ]; then
    [ "$sub" = rm ] && run_guarded "$id" "$creation"
    continue
  fi
  case "$sub" in
    rm) run_unlocked "$bin" "$sub" --force "$id" ;;
    *) ;;
  esac
done
`

const (
	maxReaperSpawnFailures             = 3
	reaperRegistrationTimeout          = 500 * time.Millisecond
	defaultReaperTimeoutSeconds        = 30
	defaultReaperInspectTimeoutSeconds = 10
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

type nameLockTarget struct {
	path     string
	identity string
}

type reaperEntry struct {
	id       string
	creation string
	// lockPaths contains the legacy, transitional, and durable barriers
	// in acquisition order for name-addressed entries. Immutable-ID
	// entries leave it empty and do not need a name lock.
	lockPaths []nameLockTarget
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu                    sync.Mutex
	cmd                   *exec.Cmd
	stdin                 io.WriteCloser
	exited                chan struct{}
	entries               []reaperEntry
	spawnFailures         int
	gaveUp                bool
	timeoutSeconds        int
	inspectTimeoutSeconds int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:                binary,
		subcommand:            subcommand,
		timeoutSeconds:        defaultReaperTimeoutSeconds,
		inspectTimeoutSeconds: defaultReaperInspectTimeoutSeconds,
	}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. Docker entries are always full
// immutable IDs, with or without a generation. Apple name-addressed
// entries require a generation and carry all unique compatibility locks.
func (r *reaper) register(id, creation string) error {
	return r.registerContext(context.Background(), id, creation)
}

func (r *reaper) registerContext(ctx context.Context, id, creation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}

	var lockPaths []nameLockTarget
	switch r.subcommand {
	case "rm":
		if !dockerIDRE.MatchString(id) {
			return fmt.Errorf("reaper: Docker entry %q requires a full immutable ID", id)
		}
	case "delete":
		if !nameRE.MatchString(id) {
			return fmt.Errorf("reaper: invalid container name %q", id)
		}
		if creation == "" {
			return fmt.Errorf("reaper: name-addressed entry %q requires a creation generation", id)
		}
		var err error
		lockPaths, err = reaperNameLockPaths(id)
		if err != nil {
			return fmt.Errorf("reaper: prepare name locks for %q: %w", id, err)
		}
		if len(lockPaths) == 0 || len(lockPaths) > 3 {
			return fmt.Errorf("reaper: got %d name lock barriers for %q", len(lockPaths), id)
		}
		seen := make(map[string]struct{}, len(lockPaths))
		for _, target := range lockPaths {
			if !validNameLockProtocolPath(target.path) {
				return fmt.Errorf("reaper: invalid name lock path %q", target.path)
			}
			if !nameLockIdentityRE.MatchString(target.identity) {
				return fmt.Errorf("reaper: invalid name lock identity %q", target.identity)
			}
			if _, ok := seen[target.path]; ok {
				return fmt.Errorf("reaper: duplicate name lock path %q", target.path)
			}
			seen[target.path] = struct{}{}
		}
	default:
		return fmt.Errorf("reaper: unsupported delete subcommand %q", r.subcommand)
	}

	if err := lockMutex(ctx, &r.mu); err != nil {
		return fmt.Errorf("reaper: register %s: %w", id, err)
	}
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation, lockPaths: lockPaths}
	r.entries = append(r.entries, entry)
	if r.stdin != nil && r.writeLockedContext(ctx, entry) == nil {
		return nil
	}
	return r.respawnAndReplayLocked(ctx)
}

func lockMutex(ctx context.Context, mu *sync.Mutex) error {
	for {
		if mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

var nameLockIdentityRE = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

func (r *reaper) writeLockedContext(ctx context.Context, e reaperEntry) error {
	if deadline, ok := ctx.Deadline(); ok {
		if writer, ok := r.stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
			if err := writer.SetWriteDeadline(deadline); err != nil {
				return err
			}
			defer func() { _ = writer.SetWriteDeadline(time.Time{}) }()
			if err := ctx.Err(); err != nil {
				return err
			}
			return r.writeLocked(e)
		}
	}

	// Test and alternate runners may provide a writer without pipe
	// deadlines. Do not let such a writer hold the registration mutex past
	// the caller's bounded context. Production StdinPipe is an *os.File and
	// takes the deadline path above; this fallback is intentionally isolated
	// to non-file writers.
	if _, ok := r.stdin.(interface{ SetWriteDeadline(time.Time) error }); !ok {
		result := make(chan error, 1)
		go func() { result <- r.writeLocked(e) }()
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.writeLocked(e)
}

func (r *reaper) writeLocked(e reaperEntry) error {
	if r.stdin == nil {
		return io.ErrClosedPipe
	}
	creation := e.creation
	if creation == "" {
		creation = "-"
	}
	fields := []string{e.id, creation}
	if len(e.lockPaths) > 3 {
		return fmt.Errorf("reaper: too many name lock barriers for %q", e.id)
	}
	seen := make(map[string]struct{}, len(e.lockPaths))
	for i := 0; i < 3; i++ {
		if i >= len(e.lockPaths) {
			fields = append(fields, "-", "-")
			continue
		}
		target := e.lockPaths[i]
		if !validNameLockProtocolPath(target.path) || !nameLockIdentityRE.MatchString(target.identity) {
			return fmt.Errorf("reaper: invalid stable name lock for %q", e.id)
		}
		if _, ok := seen[target.path]; ok {
			return fmt.Errorf("reaper: duplicate stable name lock for %q", e.id)
		}
		seen[target.path] = struct{}{}
		fields = append(fields, target.path, target.identity)
	}
	line := strings.Join(fields, "\t") + "\n"
	n, err := io.WriteString(r.stdin, line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

// respawnAndReplayLocked starts a fresh reaper process and re-registers
// every known ID with it. Success resets the consecutive-failure count;
// giving up logs once so a permanently broken reaper is visible.
func (r *reaper) respawnAndReplayLocked(ctx context.Context) error {
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.spawnLocked(); err != nil {
			r.spawnFailures++
			continue
		}
		replayed := true
		for _, e := range r.entries {
			if r.writeLockedContext(ctx, e) != nil {
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

func reaperLockHelperForOS(goos string) string {
	if goos == "darwin" {
		return "lockf"
	}
	return "flock"
}

func reaperLockHelper() string {
	return reaperLockHelperForOS(runtime.GOOS)
}

func (r *reaper) spawnLocked() error {
	timeout := r.timeoutSeconds
	if timeout <= 0 {
		timeout = defaultReaperTimeoutSeconds
	}
	inspectTimeout := r.inspectTimeoutSeconds
	if inspectTimeout <= 0 {
		inspectTimeout = defaultReaperInspectTimeoutSeconds
	}
	cmd := exec.Command(
		"/bin/sh", "-c", reaperScript, "containergo-reaper",
		r.binary, r.subcommand, breQuote(creationLabel), reaperLockHelper(), strconv.Itoa(timeout), strconv.Itoa(inspectTimeout), strconv.Itoa(timeout),
	)
	prepareReaperCommand(cmd)
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

// killForTest kills the isolated reaper process group and waits until it
// is reaped, so the next write deterministically fails without leaving
// helper descendants behind.
func (r *reaper) killForTest() error {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	if cmd == nil {
		return nil
	}
	if err := killReaperProcess(cmd); err != nil {
		return err
	}
	if exited == nil {
		return nil
	}
	select {
	case <-exited:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("reaper: timed out waiting for killed child")
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
func registerWithGlobalReaper(ctx context.Context, binary, subcommand, id, creation string) {
	if runtime.GOOS == "windows" {
		return
	}
	registrationCtx, cancel := context.WithTimeout(ctx, reaperRegistrationTimeout)
	defer cancel()
	if err := lockMutex(registrationCtx, &globalReapersMu); err != nil {
		log.Printf("container-go: reaper registration %s: %v", id, err)
		return
	}
	r, ok := globalReapers[binary]
	if !ok {
		r = newReaper(binary, subcommand)
		globalReapers[binary] = r
	}
	globalReapersMu.Unlock()
	if err := r.registerContext(registrationCtx, id, creation); err != nil {
		// A name-addressed entry is not safe without its stable lock
		// file, so registration failure is deliberately fail-closed. The
		// best-effort reaper contract still leaves normal Run/Cleanup
		// paths available to the caller.
		log.Printf("container-go: reaper registration %s: %v", id, err)
	}
}
