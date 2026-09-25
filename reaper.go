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
// The script is a fixed string; container IDs enter it only as stdin
// data validated against Apple Container's name rule, and the script
// itself disables globbing and quotes every expansion the IDs reach.
// Every backend entry runs behind a bounded timeout. The timeout covers
// the complete inspect, status-marker, filter, and delete pipeline; it
// kills the whole local process group when the shell supports one and
// falls back to killing descendants individually otherwise. Inspect
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
bin="$1"
sub="$2"
key="$3"
timeout="${4:-30}"
ids=""
while IFS= read -r line; do
  ids="$ids
$line"
done
kill_descendants() {
  children=$(pgrep -P "$1" 2>/dev/null)
  for child in $children; do
    kill_descendants "$child"
  done
  kill -9 "$1" 2>/dev/null || true
}
kill_pipeline() {
  pid="$1"
  group=0
  if [ "$process_groups" = 1 ] && kill -0 -"$pid" 2>/dev/null; then
    group=1
  fi
  # Kill descendants first so a nested monitor-mode group cannot escape;
  # the group signal below also catches descendants not seen by pgrep.
  kill_descendants "$pid"
  if [ "$group" = 1 ]; then
    kill -9 -"$pid" 2>/dev/null || true
  fi
}
run_with_timeout() {
  "$@" & pid=$!
  (sleep "$timeout"; kill_pipeline "$pid") & killer=$!
  wait "$pid" 2>/dev/null
  rc=$?
  kill "$killer" 2>/dev/null
  wait "$killer" 2>/dev/null
  return "$rc"
}
process_entry() {
  bin="$1"
  sub="$2"
  id="$3"
  creation="$4"
  key="$5"
  target="$id"
  if [ -n "$creation" ]; then
    # Apple has no inspect format; filter on the pipe before command
    # substitution can materialize output. This whole pipeline runs in
    # the process group protected by run_with_timeout.
    inspect_fields=$(
      {
        "$bin" inspect "$id" 2>/dev/null
        printf '\n__containergo_inspect_rc__%s\n' "$?"
      } | sed -n \
        -e "s/^[[:space:]]*\"$key\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{16\}\)\".*/creation=\1/p" \
        -e 's/^[[:space:]]*"Id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*/id=\1/p' \
        -e 's/^__containergo_inspect_rc__\([0-9][0-9]*\)$/inspect_rc=\1/p'
    ) || return 0
    got=$(printf '%s\n' "$inspect_fields" | sed -n 's/^creation=//p' | head -n 1)
    uid=$(printf '%s\n' "$inspect_fields" | sed -n 's/^id=//p' | head -n 1)
    inspect_rc=$(printf '%s\n' "$inspect_fields" | sed -n 's/^inspect_rc=//p' | tail -n 1)
    unset inspect_fields
    [ "$inspect_rc" = 0 ] || return 0
    [ "$got" = "$creation" ] || return 0
    [ -n "$uid" ] && target="$uid"
  fi
  run_with_timeout "$bin" "$sub" --force "$target" >/dev/null 2>&1 || true
}
monitor_enabled() {
  set -o 2>/dev/null | grep -q '^monitor[[:space:]]*on'
}
process_groups=0
if monitor_enabled; then
  process_groups=1
fi
echo "$ids" | while IFS= read -r line; do
  # The input loop runs in a pipeline subshell. Re-enable monitor mode
  # there so each timed entry gets its own process group where supported.
  set -m 2>/dev/null
  if monitor_enabled; then
    process_groups=1
  else
    process_groups=0
  fi
  [ -z "$line" ] && continue
  id=${line%% *}
  creation=${line#* }
  [ "$id" = "$line" ] && creation=""
  run_with_timeout process_entry "$bin" "$sub" "$id" "$creation" "$key" || true
done
`

const (
	maxReaperSpawnFailures      = 3
	defaultReaperTimeoutSeconds = 30
	reaperCleanupTimeout        = 2 * time.Second
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
	// timeoutSeconds is an internal test seam; production reapers use
	// defaultReaperTimeoutSeconds.
	timeoutSeconds int
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:         binary,
		subcommand:     subcommand,
		timeoutSeconds: defaultReaperTimeoutSeconds,
	}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. creation is the generation
// ID from creationLabel; empty skips the generation check for
// backward compatibility.
func (r *reaper) register(id, creation string) error {
	if !nameRE.MatchString(id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation}
	r.entries = append(r.entries, entry)
	if r.stdin != nil {
		if r.writeLocked(entry) == nil {
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) writeLocked(e reaperEntry) error {
	if e.creation == "" {
		_, err := io.WriteString(r.stdin, e.id+"\n")
		return err
	}
	_, err := io.WriteString(r.stdin, e.id+" "+e.creation+"\n")
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
	timeout := r.timeoutSeconds
	if timeout <= 0 {
		timeout = defaultReaperTimeoutSeconds
	}
	cmd := exec.Command(
		"/bin/sh", "-c", reaperScript, "containergo-reaper",
		r.binary, r.subcommand, breQuote(creationLabel), strconv.Itoa(timeout),
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

// killForTest kills the reaper process group and waits until the child is
// reaped, so fake descendants cannot survive the test and the next write
// deterministically fails. The cleanup is bounded because a test must not
// be able to wedge the whole suite on a stalled pgrep or an expanding tree.
func (r *reaper) killForTest() error {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cancel()
	var cleanupErrors []error
	if cmd != nil {
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
	_ = r.register(id, creation)
}
