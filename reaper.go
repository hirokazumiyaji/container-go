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
const reaperScript = `set -f
set -m 2>/dev/null || true
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

# Keep a backend call bounded without leaving the sleep process behind.
# The timer is a shell child with a TERM trap; the trap kills and reaps
# its sleeper before the timer itself is reaped. Job control lets the
# timeout signal the backend process group when the host supports it.
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
  entry_id="$1"
  entry_creation="$2"
  entry_pending="$3"
  shift 3
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

# The input is a small event stream. P marks a create that is still
# settling, C changes that exact key to active, and + is an already
# complete entry. Plain id lines remain accepted for compatibility with
# older package-local users of the reaper.
awk '
function key(id, gen) { return id SUBSEP gen }
$1 == "P" && NF >= 3 { active[key($2, $3)] = "P"; next }
$1 == "C" && NF >= 3 { k=key($2, $3); if (k in active) active[k]="A"; next }
$1 == "S" && NF >= 3 { k=key($2, $3); if (k in active) active[k]="S"; next }
$1 == "+" && NF >= 2 { active[key($2, $3)]="A"; next }
$1 == "-" && NF >= 2 { delete active[key($2, $3)]; next }
NF == 1 { active[key($1, $2)]="A"; next }
NF >= 2 && $1 !~ /^[+PCS-]$/ { active[key($1, $2)]="A"; next }
END {
  for (k in active) {
    split(k, fields, SUBSEP)
    print active[k], fields[1], fields[2]
  }
}
' | while IFS=' ' read -r state entry_id entry_creation; do
  [ -n "$entry_id" ] || continue
  [ "$state" = S ] && continue
  pending=0
  [ "$state" = P ] && pending=1
  process_entry "$entry_id" "$entry_creation" "$pending" "$@"
done
`

const (
	maxReaperSpawnFailures       = 3
	defaultReaperTimeoutSeconds  = 30
	defaultReaperPendingAttempts = 30
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
	stdin         io.WriteCloser
	exited        chan struct{}
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
	closed        bool

	// These are test seams. Production values are bounded defaults.
	timeoutSeconds  int
	pendingAttempts int
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
	return nil
}

func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

func (r *reaper) registerEntry(entry reaperEntry) error {
	if err := r.validateEntry(entry); err != nil {
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
	line := prefix + " " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
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
	line := "C " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) completePending(id, creation string) error {
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
	r.entries = append(r.entries[:index], r.entries[index+1:]...)
	stdin, exited, failures := r.stdin, r.exited, r.spawnFailures
	empty := len(r.entries) == 0
	r.mu.Unlock()

	if empty {
		r.stopCurrentProcess()
		return nil
	}
	if failures < maxReaperSpawnFailures && stdin != nil && !channelClosed(exited) {
		if err := writeReaperRemoval(stdin, entry); err == nil {
			r.clearSpawnFailure()
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func writeReaperRemoval(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "- " + entry.id
	if entry.creation != "" {
		line += " " + entry.creation
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) clearSpawnFailure() {
	r.mu.Lock()
	r.spawnFailures = 0
	r.gaveUp = false
	r.mu.Unlock()
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
		r.spawnFailures = 0
		r.gaveUp = false
		r.mu.Unlock()
		return nil
	}
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()

	for r.spawnFailuresValue() < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			r.recordSpawnFailure()
			continue
		}
		replayed := true
		for _, entry := range entries {
			if err := r.writeCurrentEntry(entry); err != nil {
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
	if !r.gaveUpValue() {
		r.mu.Lock()
		r.gaveUp = true
		r.mu.Unlock()
		log.Printf("container-go: reaper giving up after %d consecutive spawn failures (binary=%q)", maxReaperSpawnFailures, r.binary)
	}
	return errors.New("reaper: giving up after repeated spawn failures")
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
	r.mu.Unlock()
}

func (r *reaper) spawnLocked() error {
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
	cmd := exec.Command("/bin/sh", args...)
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
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
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
	cmd, stdin, exited := r.cmd, r.stdin, r.exited
	r.cmd, r.stdin, r.exited = nil, nil, nil
	r.mu.Unlock()
	if cmd == nil {
		return
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if stdin != nil {
		_ = stdin.Close()
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
