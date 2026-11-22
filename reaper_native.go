package container

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
	"github.com/hirokazumiyaji/container-go/internal/strictjson"
)

// The native reaper is a watchdog loop run by re-executing this executable
// with an internal marker instead of starting /bin/sh. The parent side is
// unchanged: entries arrive on stdin, the loop does nothing until EOF, and EOF
// starts the bounded reap. Inspect output is decoded with the same parsers the
// library uses for the backend, so neither sed nor awk is needed.
const (
	// reaperInternalEnv marks the self-executed reaper child. It is set by
	// the parent only, and the marker is removed from backend commands.
	reaperInternalEnv     = "CONTAINERGO_INTERNAL_REAPER"
	reaperInternalCommand = "containergo-internal-reaper"

	nativeReaperInspectTimeout   = 10 * time.Second
	nativeReaperLockTimeout      = 30 * time.Second
	nativeReaperPendingRetryWait = time.Second
	// WaitDelay keeps a killed backend command from stalling Wait when a
	// descendant inherited its output descriptors.
	nativeReaperWaitDelay = time.Second
	// nativeReaperMaxInspectBytes bounds inspect output kept in memory. A
	// larger response is treated as unreadable rather than staged; the
	// delete gate then fails closed.
	nativeReaperMaxInspectBytes = 8 << 20

	defaultReaperBudgetSeconds = 120
	defaultReaperParallel      = 4
)

var (
	errReaperLockSkipped = errors.New("reaper: lock path is not a usable lock file")
	errReaperLockTimeout = errors.New("reaper: lock acquisition timed out")
)

// reaperDockerInspectFields are the fields the native reaper needs from a
// Docker inspect entry. It is narrower than dockerInspectFields because the
// reaper only identifies an entry by its immutable ID and creation label,
// and a backend may omit state fields on an entry that is already gone.
var reaperDockerInspectFields = []string{"Id"}

// reaperModulePath identifies the module that contains the reaper init hook.
// A -buildmode=plugin child reports the host executable's build information,
// so the module must appear there before the child can be trusted to re-enter
// this package instead of running the host main.
const reaperModulePath = "github.com/hirokazumiyaji/container-go"

// nativeReaperSupported guards self-execution on hosts where re-running
// os.Executable() would not start a process that contains this package. There
// is no /bin/sh fallback, so an unsupported host has no reaper and
// registration is a no-op; explicit cleanup paths remain.
func nativeReaperSupported() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		// Without build metadata the executable may be a library host
		// rather than a main package, so fail closed instead of assuming
		// re-execution is safe.
		return false
	}
	return nativeReaperSupportedInfo(info)
}

// nativeReaperSupportedInfo applies every self-execution prerequisite to
// info. The module check rejects a plugin host that did not link this package:
// the plugin's reported build information belongs to the host, and re-running
// that executable with the internal marker would start the host main again.
func nativeReaperSupportedInfo(info *debug.BuildInfo) bool {
	if !nativeReaperBuildInfoSupported(info) {
		return false
	}
	return nativeReaperPlatformSupported()
}

func nativeReaperBuildInfoSupported(info *debug.BuildInfo) bool {
	if info == nil || !nativeReaperBuildInfoIncludesModule(info) {
		return false
	}
	mode := "exe"
	for _, setting := range info.Settings {
		if setting.Key == "-buildmode" {
			mode = setting.Value
			break
		}
	}
	return nativeReaperBuildModeSupported(mode)
}

func nativeReaperBuildInfoIncludesModule(info *debug.BuildInfo) bool {
	if info.Main.Path == reaperModulePath {
		return true
	}
	for _, dep := range info.Deps {
		if dep != nil && dep.Path == reaperModulePath {
			return true
		}
	}
	return false
}

func nativeReaperBuildModeSupported(mode string) bool {
	switch mode {
	case "", "exe", "pie":
		return true
	}
	return false
}

type nativeReaperConfig struct {
	binary          string
	subcommand      string
	key             string
	timeout         time.Duration
	pendingAttempts int
	deleteFlags     []string
	// lockTimeout is a test seam; zero uses nativeReaperLockTimeout.
	lockTimeout time.Duration
}

type nativeReaperEntry struct {
	id       string
	creation string
	lockPath string
	pending  bool
	shared   bool
}

type nativeReaper struct {
	nativeReaperConfig

	budget   time.Duration
	parallel int
	stderr   io.Writer

	stderrMu sync.Mutex
}

// runNativeReaper reads the event stream, reaps every retained entry after
// EOF, and reports progress on stderr. Its return value is a process exit
// status: the reaper is best effort, so a reap failure is never one.
func runNativeReaper(cfg nativeReaperConfig, stdin io.Reader, stderr io.Writer) int {
	if cfg.binary == "" || (cfg.subcommand != "delete" && cfg.subcommand != "rm") {
		_, _ = fmt.Fprintln(stderr, "container-go: native reaper: invalid configuration")
		return 2
	}
	if cfg.key == "" {
		cfg.key = creationLabel
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultReaperTimeoutSeconds * time.Second
	}
	if cfg.pendingAttempts <= 0 {
		cfg.pendingAttempts = 1
	}
	if cfg.lockTimeout <= 0 {
		cfg.lockTimeout = nativeReaperLockTimeout
	}
	cfg.deleteFlags = append([]string(nil), cfg.deleteFlags...)
	n := &nativeReaper{
		nativeReaperConfig: cfg,
		budget:             reaperBudgetDuration(),
		parallel:           reaperParallelLimit(),
		stderr:             stderr,
	}
	n.reap(readNativeReaperEntries(stdin))
	return 0
}

func reaperBudgetDuration() time.Duration {
	seconds := int64(positiveEnvInt("REAPER_BUDGET_SECONDS", defaultReaperBudgetSeconds))
	// A huge value must not overflow time.Duration into a negative budget:
	// that would make every entry look expired and reap nothing.
	const maxSeconds = int64(math.MaxInt64) / int64(time.Second)
	if seconds > maxSeconds {
		seconds = maxSeconds
	}
	return time.Duration(seconds) * time.Second
}

func reaperParallelLimit() int {
	return positiveEnvInt("REAPER_PARALLEL", defaultReaperParallel)
}

func positiveEnvInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (n *nativeReaper) diagnostic(format string, args ...any) {
	n.stderrMu.Lock()
	defer n.stderrMu.Unlock()
	_, _ = fmt.Fprintf(n.stderr, format+"\n", args...)
}

// reap starts entries through a bounded worker pool. The overall budget is
// armed when EOF reached the loop and is checked before a start and again
// after waiting for a free worker, so a long-lived parent cannot spend it.
func (n *nativeReaper) reap(entries []nativeReaperEntry) {
	if len(entries) == 0 {
		return
	}
	startedAt := time.Now()
	expired := func() bool { return time.Since(startedAt) >= n.budget }
	sem := make(chan struct{}, n.parallel)
	var wg sync.WaitGroup
	remaining := 0
	for i, entry := range entries {
		if expired() {
			remaining = len(entries) - i
			break
		}
		sem <- struct{}{}
		if expired() {
			<-sem
			remaining = len(entries) - i
			break
		}
		wg.Add(1)
		go func(entry nativeReaperEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			n.processEntry(entry)
		}(entry)
	}
	if remaining > 0 {
		n.diagnostic("container-go: reaper budget expired with %d entries remaining", remaining)
	}
	wg.Wait()
}

// processEntry applies the delete gate: the entry must be
// a validated name or immutable ID, a name-addressed entry must hold the
// shared name lock, and a generation entry must observe its exact creation
// generation before anything is deleted.
func (n *nativeReaper) processEntry(entry nativeReaperEntry) {
	if entry.id == "" || entry.shared || (entry.pending && entry.creation == "") {
		return
	}
	if !nameRE.MatchString(entry.id) && !domain.IsImmutableContainerID(entry.id) {
		return
	}
	if entry.creation != "" && !domain.IsCreationID(entry.creation) {
		return
	}
	if strings.ContainsAny(entry.lockPath, "\t\r\n") {
		return
	}
	immutable := domain.IsImmutableContainerID(entry.id)
	if entry.lockPath == "" {
		// A Docker delete is bound to the immutable ID the inspect gate
		// returned, so an unlocked Docker name is as safe as the ID it is
		// rechecked into; an immutable Docker ID is self-guarding. Apple
		// deletes by name and therefore always needs its shared lock, and
		// a Docker entry without a creation cannot be checked at all.
		if n.subcommand != "rm" || (entry.creation == "" && !immutable) {
			return
		}
	} else {
		release, err := lockReaperName(entry.lockPath, n.lockTimeout)
		switch {
		case errors.Is(err, errReaperLockSkipped):
			return
		case errors.Is(err, errReaperLockTimeout):
			n.diagnostic("container-go: reaper lock timed out; skipping one entry")
			return
		case err != nil:
			n.diagnostic("container-go: reaper lock failed; skipping one entry")
			return
		}
		defer release()
	}
	if entry.creation == "" {
		if n.subcommand == "rm" && !immutable {
			return
		}
		n.delete(entry.id)
		return
	}
	target, ok := n.inspectTarget(entry)
	if !ok {
		return
	}
	n.delete(target)
}

// inspectTarget rechecks the creation generation. A pending create may not
// be observable yet and retries; a completed generation that is absent or
// different is never deleted, because the name may already belong to a
// replacement.
func (n *nativeReaper) inspectTarget(entry nativeReaperEntry) (string, bool) {
	for tries := 0; ; {
		out, ok := n.inspect(entry.id)
		var uid, creation string
		if ok {
			uid, creation = parseNativeReaperInspect(out, n.subcommand, n.key)
		}
		if ok && creation == entry.creation {
			if n.subcommand == "rm" {
				if !domain.IsImmutableContainerID(uid) {
					return "", false
				}
				return uid, true
			}
			return entry.id, true
		}
		if ok && (!entry.pending || creation != "") {
			return "", false
		}
		if !entry.pending {
			return "", false
		}
		tries++
		if tries >= n.pendingAttempts {
			return "", false
		}
		time.Sleep(nativeReaperPendingRetryWait)
	}
}

func (n *nativeReaper) inspect(id string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), nativeReaperInspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, n.binary, "inspect", id)
	cmd.Env = nativeReaperBackendEnv()
	cmd.WaitDelay = nativeReaperWaitDelay
	out := newBoundedBuffer(nativeReaperMaxInspectBytes)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil || out.Truncated() {
		return nil, false
	}
	data, _ := io.ReadAll(out.reader())
	return data, true
}

func (n *nativeReaper) delete(target string) {
	ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
	defer cancel()
	args := make([]string, 0, 2+len(n.deleteFlags)+1)
	args = append(args, n.subcommand, "--force")
	args = append(args, n.deleteFlags...)
	args = append(args, target)
	cmd := exec.CommandContext(ctx, n.binary, args...)
	cmd.Env = nativeReaperBackendEnv()
	cmd.WaitDelay = nativeReaperWaitDelay
	_ = cmd.Run()
}

// nativeReaperBackendEnv removes the self-execution marker before a backend
// call, so a backend that happens to be a Go program cannot re-enter the
// reaper loop.
func nativeReaperBackendEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, reaperInternalEnv+"=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// parseNativeReaperInspect reuses the ordinary inspect decoders. Apple
// output nests labels under configuration.labels; Docker output nests them
// under Config.Labels. The parser keeps only the immutable top-level Docker
// ID from the top-level object, matching the other inspect parsers.
func parseNativeReaperInspect(data []byte, subcommand, key string) (uid, creation string) {
	switch subcommand {
	case "delete":
		containers, err := inspect.Decode(data)
		if err != nil || len(containers) == 0 {
			return "", ""
		}
		return "", containers[0].Configuration.Labels[key]
	case "rm":
		containers, err := strictjson.Array[dockerInspect](data, reaperDockerInspectFields)
		if err != nil || len(containers) == 0 {
			return "", ""
		}
		uid = containers[0].ID
		if !domain.IsImmutableContainerID(uid) {
			uid = ""
		}
		return uid, containers[0].Config.Labels[key]
	}
	return "", ""
}

type nativeReaperKey struct {
	id       string
	creation string
}

// readNativeReaperEntries accumulates the event stream until EOF. A pending
// entry is retained as pending until a completion event for the same
// identity replaces it, and a removal event deletes it so a removed
// container is not resurrected by a later replay.
func readNativeReaperEntries(r io.Reader) []nativeReaperEntry {
	order := make([]nativeReaperKey, 0)
	entries := make(map[nativeReaperKey]nativeReaperEntry)
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// A line with no trailing newline is a write interrupted by
			// the parent's death, so discard it: a partial ID must never
			// be executed.
			break
		}
		event, id, creation, lock, ok := parseNativeReaperEvent(line)
		if !ok || id == "" {
			continue
		}
		key := nativeReaperKey{id: id, creation: creation}
		existing, exists := entries[key]
		switch event {
		case "P":
			entries[key] = nativeReaperEntry{id: id, creation: creation, lockPath: lock, pending: true}
			if !exists {
				order = append(order, key)
			}
		case "C":
			if exists {
				existing.pending = false
				existing.shared = false
				if lock != "" {
					existing.lockPath = lock
				}
				entries[key] = existing
			}
		case "S":
			if exists {
				existing.pending = false
				existing.shared = true
				if lock != "" {
					existing.lockPath = lock
				}
				entries[key] = existing
			}
		case "+":
			entries[key] = nativeReaperEntry{id: id, creation: creation, lockPath: lock}
			if !exists {
				order = append(order, key)
			}
		case "-":
			delete(entries, key)
			if exists {
				order = removeNativeReaperKey(order, key)
			}
		}
	}
	result := make([]nativeReaperEntry, 0, len(order))
	for _, key := range order {
		if entry, ok := entries[key]; ok {
			result = append(result, entry)
		}
	}
	return result
}

func removeNativeReaperKey(order []nativeReaperKey, key nativeReaperKey) []nativeReaperKey {
	for i, existing := range order {
		if existing == key {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

// parseNativeReaperEvent parses one tab-separated event line written by
// writeReaperEntry and its siblings: <event>\t<id>[\t<creation>][\t<lock>].
// The parent is the only writer, so no other encoding is accepted.
func parseNativeReaperEvent(line string) (event, id, creation, lock string, ok bool) {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return "", "", "", "", false
	}
	switch fields[0] {
	case "+", "P", "C", "S", "-":
	default:
		return "", "", "", "", false
	}
	event, id = fields[0], fields[1]
	if len(fields) >= 3 {
		creation = fields[2]
	}
	if len(fields) >= 4 {
		lock = fields[3]
	}
	if creation == "-" {
		creation = ""
	}
	return event, id, creation, lock, true
}
