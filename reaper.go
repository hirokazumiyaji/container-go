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
// data validated as an Apple Container name or a full Docker ID, and
// the script itself disables globbing and quotes every expansion the
// IDs reach.
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
// targets that validated ID instead of the name; a Docker entry with a
// generation but no valid ID is skipped.
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
descendant_timeout=1
max_descendant_pids=256
max_descendant_depth=32
max_descendant_passes=4
max_descendant_lookups=8
# Resolve pgrep once to an absolute executable; never invoke a shell
# function, alias, or bare PATH lookup from the cleanup recursion.
pgrep_bin=$(command -v pgrep 2>/dev/null) || pgrep_bin=
case "$pgrep_bin" in
  /*) [ -x "$pgrep_bin" ] || pgrep_bin= ;;
  *) pgrep_bin= ;;
esac
mktemp_bin=$(command -v mktemp 2>/dev/null) || mktemp_bin=
case "$mktemp_bin" in
  /*) [ -x "$mktemp_bin" ] || mktemp_bin= ;;
  *) mktemp_bin= ;;
esac
reaper_warn() {
  printf 'containergo: %s\n' "$*" >&2
}
bounded_pgrep() {
  parent="$1"
  [ -n "$pgrep_bin" ] && [ -n "$mktemp_bin" ] || return 127
  lookup_file=$("$mktemp_bin" "${TMPDIR:-/tmp}/containergo-pgrep.XXXXXX") || return 127
  lookup_error_file=$("$mktemp_bin" "${TMPDIR:-/tmp}/containergo-pgrep-error.XXXXXX") || {
    rm -f "$lookup_file"
    return 127
  }
  trap 'rm -f "$lookup_file" "$lookup_error_file" 2>/dev/null || true' 0
  trap 'rm -f "$lookup_file" "$lookup_error_file" 2>/dev/null || true; exit 1' HUP INT TERM
  "$pgrep_bin" -P "$parent" >"$lookup_file" 2>"$lookup_error_file" &
  lookup="$!"
  (
    sleeper=
    trap 'if [ -n "$sleeper" ]; then kill -KILL "$sleeper" 2>/dev/null || true; wait "$sleeper" 2>/dev/null || true; fi; exit 0' HUP INT TERM
    sleep "$descendant_timeout" &
    sleeper="$!"
    wait "$sleeper"
    if [ "$process_groups" = 1 ]; then
      kill -KILL -"$lookup" 2>/dev/null || true
    fi
    kill -KILL "$lookup" 2>/dev/null || true
    wait "$lookup" 2>/dev/null || true
  ) &
  lookup_timer="$!"
  wait "$lookup"
  status="$?"
  kill -TERM "$lookup_timer" 2>/dev/null || true
  wait "$lookup_timer" 2>/dev/null || true
  if [ "$status" -eq 1 ] && [ -s "$lookup_error_file" ]; then
    status=125
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    printf '%s\n' "$line"
  done < "$lookup_file"
  rm -f "$lookup_file" "$lookup_error_file"
  if [ "$status" -ge 128 ] 2>/dev/null; then
    return 124
  fi
  return "$status"
}
list_children() {
  parent="$1"
  descendant_lookups=$((descendant_lookups + 1))
  if [ "$descendant_lookups" -gt "$max_descendant_lookups" ]; then
    reaper_warn "descendant lookup limit reached"
    return 1
  fi
  children=$(bounded_pgrep "$parent")
  status="$?"
  case "$status" in
    0) ;;
    1) children=; return 0 ;;
    *) reaper_warn "pgrep lookup failed for pid $parent (status $status)"; return 1 ;;
  esac
  validated=
  for child in $children; do
    case "$child" in
      ""|*[!0-9]*) reaper_warn "pgrep returned an invalid child pid for $parent"; return 1 ;;
    esac
    [ "$child" -gt 0 ] 2>/dev/null || {
      reaper_warn "pgrep returned an invalid child pid for $parent"
      return 1
    }
    validated="$validated $child"
  done
  children="${validated# }"
  return 0
}
clear_failed_pid() {
  target="$1"
  remaining_failed=
  for failed in $failed_pids; do
    [ "$failed" = "$target" ] || remaining_failed="$remaining_failed $failed"
  done
  failed_pids="$remaining_failed"
}
kill_descendants() {
  pid="$1"
  depth="$2"
  if [ "$depth" -gt "$max_descendant_depth" ]; then
    reaper_warn "descendant depth limit reached at pid $pid"
    kill -9 "$pid" 2>/dev/null || true
    return 1
  fi
  # Stop a newly discovered process group before asking pgrep about its
  # children. A previously successful PID is only re-expanded, never
  # signaled again, so a recycled numeric PID cannot be killed.
  already=0
  case " $seen_pids " in
    *" $pid "*) already=1 ;;
    *) seen_pids="$seen_pids $pid" ;;
  esac
  resignal=0
  case " $failed_pids " in
    *" $pid "*) resignal=1 ;;
  esac
  if { [ "$already" -eq 0 ] || [ "$resignal" -eq 1 ]; } && [ "$process_groups" = 1 ] && kill -0 -"$pid" 2>/dev/null; then
    kill -9 -"$pid" 2>/dev/null || reaper_warn "failed to signal process group $pid"
  fi
  if ! list_children "$pid"; then
    failed_pids="$failed_pids $pid"
    if [ "$already" -eq 0 ] || [ "$resignal" -eq 1 ]; then
      kill -9 "$pid" 2>/dev/null || true
    fi
    return 1
  fi
  clear_failed_pid "$pid"
  if [ "$already" -eq 0 ] || [ "$resignal" -eq 1 ]; then
    kill -9 "$pid" 2>/dev/null || true
  fi
  branch="$children"
  for child in $branch; do
    case " $seen_pids " in
      *" $child "*) ;;
      *) descendant_count=$((descendant_count + 1)) ;;
    esac
    if [ "$descendant_count" -gt "$max_descendant_pids" ]; then
      reaper_warn "descendant pid limit reached"
      descendant_error=1
      break
    fi
    kill_descendants "$child" "$((depth + 1))" || descendant_error=1
  done
  return 0
}
kill_pipeline() {
  pid="$1"
  descendant_error=0
  descendant_count=0
  descendant_lookups=0
  seen_pids=
  failed_pids=
  pass=1
  while [ "$pass" -le "$max_descendant_passes" ]; do
    before="$descendant_count"
    kill_descendants "$pid" 0 || descendant_error=1
    if [ "$descendant_error" -eq 0 ] && [ "$descendant_count" -eq "$before" ]; then
      break
    fi
    pass=$((pass + 1))
  done
  [ "$descendant_error" -eq 0 ]
}
run_with_timeout() {
  "$@" & pid=$!
  (
    sleeper=
    trap 'if [ -n "$sleeper" ]; then kill -KILL "$sleeper" 2>/dev/null || true; wait "$sleeper" 2>/dev/null || true; fi; exit 0' HUP INT TERM
    sleep "$timeout" &
    sleeper="$!"
    wait "$sleeper"
    kill_pipeline "$pid"
  ) & killer=$!
  wait "$pid" 2>/dev/null
  rc="$?"
  kill -TERM "$killer" 2>/dev/null || true
  wait "$killer" 2>/dev/null || true
  return "$rc"
}
valid_docker_id() {
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
  [ "${#1}" -eq 64 ] 2>/dev/null
}
process_entry() {
  bin="$1"
  sub="$2"
  id="$3"
  creation="$4"
  key="$5"
  target="$id"
  case "$sub" in
    delete|rm) ;;
    *) return 0 ;;
  esac
  if [ "$sub" = "rm" ] && [ -z "$creation" ]; then
    valid_docker_id "$id" || return 0
  fi
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
    if [ "$sub" = "rm" ]; then
      valid_docker_id "$uid" || return 0
      target="$uid"
    fi
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

func validReaperID(subcommand, id string) bool {
	return nameRE.MatchString(id) || (subcommand == "rm" && dockerIDRE.MatchString(id))
}

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
// ID from creationLabel; a full immutable Docker ID is normalized to an
// ungenerated entry.
func (r *reaper) register(id, creation string) error {
	immutableID := r.subcommand == "rm" && dockerIDRE.MatchString(id)
	if !validReaperID(r.subcommand, id) {
		return fmt.Errorf("reaper: invalid container id %q", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	if immutableID {
		// A full Docker ID is already immutable; it must not be sent
		// through the name/generation fallback path.
		creation = ""
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
// process-wide reaper for its backend binary. Reaper trouble is logged
// but never fails container startup. The reaper needs /bin/sh, so on
// Windows this is a no-op and cleanup relies on the normal paths.
func registerWithGlobalReaper(binary, subcommand, id, creation string) error {
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
	if err := r.register(id, creation); err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}
