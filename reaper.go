package container

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// The reaper is a self-executed Go child holding the write end of a
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
// IDs enter the child only as stdin data validated as an Apple Container
// name or a full Docker ID. Inspect output is decoded in memory by the
// child with the same parsers the library uses, so raw JSON (which may
// contain environment secrets) is never staged in a host file.
// Backend-specific delete flags are retained for the normal cleanup and
// reaper paths.
//
// The event stream is accumulated until stdin EOF before any delete
// runs, so registration while the parent lives cannot force-delete a
// live container. After EOF the reap is bounded overall and a bounded
// worker pool starts the next entry as soon as any running entry ends:
// the reaper is insurance and must finish even when the backend answers
// nothing, rather than paying the container count times the per-entry
// timeout. If the overall budget expires with entries still queued, the
// number left unstarted is reported on stderr without naming IDs.

const (
	maxReaperSpawnFailures       = 3
	defaultReaperTimeoutSeconds  = 30
	defaultReaperPendingAttempts = 30

	initialReaperSpawnBackoff = time.Second
	maxReaperSpawnBackoff     = 30 * time.Second
)

var (
	errReaperSpawnCooldown = errors.New("reaper: spawn retry cooldown active")
	errReaperSpawnFailed   = errors.New("reaper: giving up after repeated spawn failures")
)

type reaperEntry struct {
	id       string
	creation string
	lockPath string
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
	job           reaperJob
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
	gaveUpLogged  bool
	closed        bool

	// A failed recovery burst is retried only after a bounded cooldown.
	// retryPending allows one immediate recovery attempt before the
	// cooldown, which lets a transient launch failure recover without
	// turning every subsequent registration into a hot retry loop.
	retryPending bool
	recovering   bool
	retryAt      time.Time
	retryLevel   int

	// These are test seams. Production values are bounded defaults.
	env             []string
	timeoutSeconds  int
	pendingAttempts int
	spawnCommand    func() (*exec.Cmd, error)
	now             func() time.Time
	retryBackoff    func(int) time.Duration
}

func newReaperWithEnv(binary, subcommand string, deleteFlags []string, env []string) *reaper {
	return &reaper{
		binary:          binary,
		subcommand:      subcommand,
		deleteFlags:     append([]string(nil), deleteFlags...),
		env:             append([]string(nil), env...),
		timeoutSeconds:  defaultReaperTimeoutSeconds,
		pendingAttempts: defaultReaperPendingAttempts,
	}
}

func newReaper(binary, subcommand string, deleteFlags ...string) *reaper {
	return newReaperWithEnv(binary, subcommand, deleteFlags, nil)
}

func validateReaperEntry(entry reaperEntry) error {
	if !nameRE.MatchString(entry.id) && !domain.IsImmutableContainerID(entry.id) {
		return fmt.Errorf("reaper: invalid container id %q", entry.id)
	}
	if entry.creation != "" && !domain.IsCreationID(entry.creation) {
		return fmt.Errorf("reaper: invalid creation id %q", entry.creation)
	}
	if entry.pending && entry.creation == "" {
		return errors.New("reaper: pending entry requires a creation id")
	}
	if strings.ContainsAny(entry.lockPath, "\t\r\n") {
		return errors.New("reaper: lock path contains a control character")
	}
	return nil
}

func (r *reaper) register(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation})
}

func (r *reaper) registerPending(id, creation string) error {
	return r.registerEntry(reaperEntry{id: id, creation: creation, pending: true})
}

func (r *reaper) prepareEntry(entry reaperEntry) (reaperEntry, error) {
	if err := r.validateEntry(entry); err != nil {
		return reaperEntry{}, err
	}
	// A full Docker ID is already immutable; a supplied generation is
	// redundant and must not turn the entry into a name lookup.
	if r.subcommand == "rm" && domain.IsImmutableContainerID(entry.id) {
		entry.creation = ""
		entry.pending = false
	}
	// Apple is name-addressed, and Docker may temporarily address a
	// generation by name before run returns its immutable ID. Both use
	// the same lock file as Go's generation-checked name operations.
	if r.subcommand == "delete" || !domain.IsImmutableContainerID(entry.id) {
		lockPath, err := reaperNameLockPath(entry.id)
		if err != nil {
			return reaperEntry{}, fmt.Errorf("reaper: prepare name lock: %w", err)
		}
		entry.lockPath = lockPath
	}
	if err := validateReaperEntry(entry); err != nil {
		return reaperEntry{}, err
	}
	return entry, nil
}

func (r *reaper) registerEntry(entry reaperEntry) error {
	entry, err := r.prepareEntry(entry)
	if err != nil {
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
			} else if existing.lockPath == "" && entry.lockPath != "" {
				existing.lockPath = entry.lockPath
				r.entries[i] = existing
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
	line := prefix + "\t" + entry.id
	if entry.creation != "" {
		line += "\t" + entry.creation
	} else {
		line += "\t-"
	}
	if entry.lockPath != "" {
		line += "\t" + entry.lockPath
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
	line := "C\t" + entry.id
	if entry.creation != "" {
		line += "\t" + entry.creation
	} else {
		line += "\t-"
	}
	if entry.lockPath != "" {
		line += "\t" + entry.lockPath
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) completePending(id, creation string) error {
	entry, err := r.prepareEntry(reaperEntry{id: id, creation: creation})
	if err != nil {
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
	index := -1
	for i, existing := range r.entries {
		if existing.id == id && existing.creation == creation {
			index = i
			entry = existing
			break
		}
	}
	if index < 0 {
		r.mu.Unlock()
		return nil
	}
	closed := r.closed
	stdin, exited, failures := r.stdin, r.exited, r.spawnFailures
	r.mu.Unlock()

	// The caller has decided this entry must not be reaped, whether the
	// container was deleted, reused, or never created. Drop it from the
	// replay set before notifying the child so a failed pipe write cannot
	// resurrect the entry.
	r.removeEntryLocked(entry)
	if closed {
		return nil
	}
	if stdin == nil || channelClosed(exited) {
		if r.entriesEmpty() {
			// With no retained targets there is no fail-closed state to
			// preserve; a later registration may start a fresh bounded
			// recovery window immediately.
			r.clearSpawnFailure()
			r.stopCurrentProcess()
		}
		return nil
	}
	if failures < maxReaperSpawnFailures {
		if err := writeReaperRemoval(stdin, entry); err == nil {
			r.clearSpawnFailure()
			if r.entriesEmpty() {
				r.stopCurrentProcess()
			}
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) removeEntryLocked(entry reaperEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.entries {
		if existing.id == entry.id && existing.creation == entry.creation {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return
		}
	}
}

func (r *reaper) entriesEmpty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries) == 0
}

func writeReaperRemoval(stdin io.Writer, entry reaperEntry) error {
	if stdin == nil {
		return io.ErrClosedPipe
	}
	line := "-\t" + entry.id
	if entry.creation != "" {
		line += "\t" + entry.creation
	} else {
		line += "\t-"
	}
	n, err := io.WriteString(stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (r *reaper) clearSpawnFailure() {
	r.mu.Lock()
	r.clearSpawnFailureLocked()
	r.mu.Unlock()
}

func (r *reaper) clearSpawnFailureLocked() {
	r.spawnFailures = 0
	r.gaveUp = false
	r.gaveUpLogged = false
	r.retryPending = false
	r.recovering = false
	r.retryAt = time.Time{}
	r.retryLevel = 0
}

func (r *reaper) nowLocked() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *reaper) retryBackoffLocked(level int) time.Duration {
	if level < 1 {
		level = 1
	}
	if r.retryBackoff != nil {
		delay := r.retryBackoff(level)
		if delay < 0 {
			return 0
		}
		if delay > maxReaperSpawnBackoff {
			return maxReaperSpawnBackoff
		}
		return delay
	}
	delay := initialReaperSpawnBackoff
	for i := 1; i < level; i++ {
		if delay >= maxReaperSpawnBackoff/2 {
			return maxReaperSpawnBackoff
		}
		delay *= 2
	}
	if delay > maxReaperSpawnBackoff {
		return maxReaperSpawnBackoff
	}
	return delay
}

// retryReadyLocked permits one immediate recovery burst after the first
// failed burst, then applies exponential backoff if that recovery also
// fails. A cooldown only suppresses a best-effort reaper attempt; entries
// remain in memory and no destructive operation can run without a child.
func (r *reaper) retryReadyLocked() bool {
	if !r.gaveUp {
		// Keep recovery possible for a state assembled by an older
		// caller that reached the limit without setting gaveUp.
		if r.spawnFailures >= maxReaperSpawnFailures {
			r.spawnFailures = 0
		}
		return true
	}
	if r.retryPending {
		r.retryPending = false
		r.recovering = true
		r.spawnFailures = 0
		r.gaveUp = false
		r.retryAt = time.Time{}
		return true
	}
	if !r.retryAt.IsZero() && r.nowLocked().Before(r.retryAt) {
		return false
	}
	r.recovering = true
	r.spawnFailures = 0
	r.gaveUp = false
	r.retryAt = time.Time{}
	return true
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
		r.clearSpawnFailureLocked()
		r.mu.Unlock()
		return nil
	}
	if !r.retryReadyLocked() {
		r.mu.Unlock()
		return errReaperSpawnCooldown
	}
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()

	var lastErr error
	for r.spawnFailuresValue() < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			lastErr = err
			r.recordSpawnFailure()
			continue
		}
		replayed := true
		for _, entry := range entries {
			if err := r.writeCurrentEntry(entry); err != nil {
				lastErr = err
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
	r.mu.Lock()
	logGiveUp := !r.gaveUpLogged
	r.gaveUpLogged = true
	r.mu.Unlock()
	if logGiveUp {
		log.Printf("container-go: reaper giving up temporarily after %d consecutive spawn failures (binary=%q); a later registration will retry", maxReaperSpawnFailures, r.binary)
	}
	if lastErr == nil {
		lastErr = errReaperSpawnFailed
	}
	return fmt.Errorf("%w: %v", errReaperSpawnFailed, lastErr)
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
	if r.spawnFailures < maxReaperSpawnFailures {
		r.mu.Unlock()
		return
	}
	r.gaveUp = true
	if r.recovering {
		r.recovering = false
		r.retryPending = false
		if r.retryLevel < 32 {
			r.retryLevel++
		}
		r.retryAt = r.nowLocked().Add(r.retryBackoffLocked(r.retryLevel))
	} else {
		// Leave one immediate recovery burst available. If that burst
		// fails, recordSpawnFailure takes the cooldown path above.
		r.retryPending = true
		r.retryAt = time.Time{}
	}
	r.mu.Unlock()
}

func (r *reaper) spawnLocked() error {
	var cmd *exec.Cmd
	if r.spawnCommand != nil {
		var err error
		cmd, err = r.spawnCommand()
		if err != nil {
			return err
		}
	} else {
		if !nativeReaperSupported() {
			return errors.New("reaper: self-execution is unavailable in this build")
		}
		timeout := r.timeoutSeconds
		if timeout <= 0 {
			timeout = defaultReaperTimeoutSeconds
		}
		attempts := r.pendingAttempts
		if attempts <= 0 {
			attempts = 1
		}
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("reaper: locate executable: %w", err)
		}
		args := []string{reaperInternalCommand, r.binary, r.subcommand, creationLabel, strconv.Itoa(timeout), strconv.Itoa(attempts)}
		args = append(args, r.deleteFlags...)
		cmd = exec.Command(exe, args...)
		env := append(append([]string(nil), r.env...), reaperInternalEnv+"=1")
		cmd.Env = cli.MergeEnv(os.Environ(), env)
	}
	if cmd == nil {
		return errors.New("reaper: nil spawn command")
	}
	configureReaperProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	job := attachReaperJob(cmd)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	r.mu.Lock()
	r.cmd, r.stdin, r.exited, r.job = cmd, stdin, exited, job
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
	job := r.job
	r.cmd, r.stdin, r.exited = nil, nil, nil
	r.job = reaperJob{}
	r.mu.Unlock()
	if cmd == nil {
		return
	}
	// Stop the child before closing its stdin. Closing first hands the
	// child an EOF, and a still-running child would start reaping entries
	// this call is replacing. Only closeStdin is an intentional EOF.
	if !channelClosed(exited) {
		// On Windows the job owns every process the reaper started, so it is
		// terminated before the direct kill and no backend command can be
		// started in between. On Unix this is a no-op and killReaperProcess
		// signals the process group.
		job.terminate()
		killReaperProcess(cmd)
	}
	if exited != nil {
		<-exited
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	job.close()
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

// reaperUnavailable reports a host where this process cannot be re-executed
// as a reaper: -buildmode=c-shared, c-archive, or plugin, or a setuid/setgid
// Unix binary. There is no /bin/sh fallback, so registration stays a silent
// no-op instead of running a watchdog that cannot exist. Abnormal-exit
// cleanup then relies on the explicit Terminate, Cleanup, and Prune paths.
func reaperUnavailable() bool {
	return !nativeReaperSupported()
}

// getGlobalReaperTarget returns nil when this host has no usable reaper.
func getGlobalReaperTarget(key, binary, subcommand string, deleteFlags []string, env []string) *reaper {
	if reaperUnavailable() {
		return nil
	}
	if key == "" {
		key = binary
	}
	globalReapersMu.Lock()
	defer globalReapersMu.Unlock()
	r, ok := globalReapers[key]
	if !ok {
		r = newReaperWithEnv(binary, subcommand, deleteFlags, env)
		globalReapers[key] = r
	}
	return r
}

func registerWithGlobalReaperTarget(key, binary, subcommand string, deleteFlags []string, env []string, id, creation string) error {
	r := getGlobalReaperTarget(key, binary, subcommand, deleteFlags, env)
	if r == nil {
		return nil
	}
	if err := r.register(id, creation); err != nil {
		log.Printf("container-go: reaper registration failed (key=%q, binary=%q): %v", key, binary, err)
		return err
	}
	return nil
}

// registerWithGlobalReaper best-effort registers a completed container
// with the process-wide reaper. Reaper trouble never fails startup.
func registerWithGlobalReaper(binary, subcommand string, deleteFlags []string, id, creation string) error {
	return registerWithGlobalReaperTarget(binary, binary, subcommand, deleteFlags, nil, id, creation)
}

func preRegisterWithGlobalReaperTarget(key, binary, subcommand string, deleteFlags []string, env []string, id, creation string) error {
	r := getGlobalReaperTarget(key, binary, subcommand, deleteFlags, env)
	if r == nil {
		return nil
	}
	if err := r.registerPending(id, creation); err != nil {
		log.Printf("container-go: reaper pre-registration failed (key=%q, binary=%q): %v", key, binary, err)
		return err
	}
	return nil
}

func completePreRegistrationWithGlobalReaperKey(key, id, creation string) error {
	globalReapersMu.Lock()
	r, ok := globalReapers[key]
	globalReapersMu.Unlock()
	if !ok {
		return nil
	}
	if err := r.completePending(id, creation); err != nil {
		log.Printf("container-go: reaper create-completion update failed (key=%q): %v", key, err)
		return err
	}
	return nil
}

func markSharedWithGlobalReaperKey(key, id, creation string) error {
	globalReapersMu.Lock()
	r, ok := globalReapers[key]
	globalReapersMu.Unlock()
	if !ok {
		return nil
	}
	if err := r.markShared(id, creation); err != nil {
		log.Printf("container-go: reaper shared-state update failed (key=%q): %v", key, err)
		return err
	}
	return nil
}

func unregisterWithGlobalReaperKey(key, id, creation string) error {
	globalReapersMu.Lock()
	r, ok := globalReapers[key]
	globalReapersMu.Unlock()
	if !ok {
		return nil
	}
	if err := r.unregister(id, creation); err != nil {
		log.Printf("container-go: reaper unregister failed (key=%q): %v", key, err)
		return err
	}
	return nil
}
