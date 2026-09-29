package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	managedLabel    = "com.github.hirokazumiyaji.container-go"
	sessionLabel    = "com.github.hirokazumiyaji.container-go.session"
	reuseLabel      = "com.github.hirokazumiyaji.container-go.reuse"
	reuseGroupLabel = "com.github.hirokazumiyaji.container-go.reuse-group"
	creationLabel   = "com.github.hirokazumiyaji.container-go.creation"

	queryTimeout = 30 * time.Second
	// runTimeout also covers an implicit image pull.
	runTimeout = 10 * time.Minute
)

// reuseAttachTimeout bounds waiting for another process's create to
// reach a usable state during WithReuse get-or-create. It applies to
// attach polling only; a leader's own image pull and create carry an
// independent runTimeout budget. Vars (not consts) so tests can shrink
// them.
var (
	reuseAttachTimeout = 60 * time.Second
	reusePollInterval  = 100 * time.Millisecond
	// terminateTimeout bounds the complete generation-checked termination,
	// including waiting for another process' name lock. It is a var so tests
	// can exercise the bounded cleanup contract without a long wait.
	terminateTimeout = queryTimeout
	// cleanupFailedCreateTimeout bounds lock acquisition, ownership
	// inspection, reaper registration, and deletion as one operation.
	cleanupFailedCreateTimeout = queryTimeout
)

// sessionID identifies all containers created by this process.
var sessionID = sync.OnceValue(func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("container: read random session id: %v", err))
	}
	return hex.EncodeToString(b[:])
})

func newContainerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("container: read random container name: %v", err))
	}
	return "containergo-" + hex.EncodeToString(b[:])
}

func newCreationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("container: read random creation id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// State is a container lifecycle state.
type State string

const (
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateStopping State = "stopping"
	StateCreated  State = "created"
	StateUnknown  State = "unknown"
)

// Container is a handle to a container created by Run.
type Container struct {
	id        string
	runner    cli.Runner
	eng       engine
	exposed   []portSpec
	published []publishSpec
	// reused marks a WithReuse handle. Cleanup, TerminateContainer,
	// and the watchdog reaper skip these so shared containers survive
	// process exit. Explicit Terminate still removes them.
	reused bool
	// creation is the unique generation ID stored in creationLabel.
	// Terminate and the reaper verify it before deleting so a stale
	// handle does not remove a same-name replacement made by this
	// library; see Terminate for the limits of the name-based path.
	creation string
	// uid is the backend's immutable container ID when it has one
	// (Docker). It is not trusted until inspectFresh has bound and
	// verified it under inspectMu.
	uid      string
	uidBound bool

	mu        sync.Mutex
	info      *engineInfo // cached first inspect; immutable fields only
	inspectMu sync.Mutex  // serializes target selection and UID binding
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, it verifies and
// removes the owned generation; cleanup failures are joined to the
// original error. WithReuse switches to get-or-create; see
// WithReuse for the shared-handle lifecycle.
func Run(ctx context.Context, image string, opts ...Option) (*Container, error) {
	cfg := newConfig()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}
	if !imageRE.MatchString(image) {
		return nil, fmt.Errorf("invalid image reference %q", image)
	}
	if cfg.reuse && cfg.name == "" {
		return nil, fmt.Errorf("WithReuse requires WithName")
	}
	if cfg.reuseGroup != "" && !cfg.reuse {
		return nil, fmt.Errorf("WithReuseGroup requires WithReuse")
	}
	if cfg.eng == nil {
		eng, err := detectEngine()
		if err != nil {
			return nil, err
		}
		cfg.eng = eng
	}
	applyEngineBinary(cfg)
	if err := cfg.eng.checkConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.reuse {
		return reuseRun(ctx, image, cfg)
	}
	if cfg.name == "" {
		cfg.name = newContainerName()
	}
	cfg.creation = newCreationID()

	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}

	runCtx, cancel := withDefaultTimeout(ctx, runTimeout)
	defer cancel()
	// The pull policy brings the image into the local store before the
	// run command; both share the aggregated flight so concurrent Runs
	// of the same image pull once.
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
	}
	stdout, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		if !attempted {
			return nil, err
		}
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified); cleanupErr != nil {
			return nil, withCleanupError(classified, &CleanupError{Container: cfg.name, Err: cleanupErr})
		}
		return nil, classified
	}

	uid := cfg.eng.parseRunID(stdout)
	c := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		creation:  cfg.creation,
		uid:       uid,
		// parseRunID only proves the output shape; inspectFresh binds it.
		uidBound: false,
	}
	var initialInfo *engineInfo
	if c.eng.immutableID() {
		initialInfo, err = c.cachedInfo(runCtx)
		if err != nil {
			return nil, c.rollback(context.WithoutCancel(runCtx), fmt.Errorf("verify created container: %w", err))
		}
	}
	// Register only after the backend has returned and an immutable backend
	// has verified its UID/generation binding. Reaper trouble is best-effort,
	// and normal Run/Cleanup paths remain available if registration fails.
	if er, ok := cfg.runner.(cli.ExternalRunner); ok && er.External() && !keepContainers() {
		bin := er.ExternalBinary()
		if bin == "" {
			bin = cfg.eng.binary()
		}
		target := c.id
		if c.eng.immutableID() {
			target = c.uid
		}
		registerWithGlobalReaper(runCtx, bin, cfg.eng.reaperSubcommand(), target, cfg.creation)
	}

	for _, f := range cfg.files {
		if err := c.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return nil, c.rollback(ctx, err)
		}
	}
	if cfg.waitStrategy != nil {
		if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: c}); err != nil {
			cleanupCtx := context.WithoutCancel(ctx)
			tail := c.logTail(cleanupCtx)
			err = fmt.Errorf("container %s failed to become ready: %w", c.id, err)
			if tail != "" {
				err = fmt.Errorf("%w\ncontainer logs:\n%s", err, tail)
			}
			return nil, c.rollback(ctx, err)
		}
	}
	if cfg.waitStrategy != nil || (c.eng.immutableID() && c.uidBound) {
		if _, err := c.verifyHandleIdentity(runCtx, initialInfo, true, false); err != nil {
			return nil, c.rollback(context.WithoutCancel(runCtx), fmt.Errorf("verify before return: %w", err))
		}
	}
	return c, nil
}

// runCreateLocked serializes a name-addressed create with the guarded
// prune/delete and watchdog-reaper paths. The attempted result is separate
// from the error so callers do not run failed-create cleanup when acquiring
// the lock itself failed and no create command was issued.
func runCreateLocked(ctx context.Context, cfg *config, args ...string) (stdout []byte, attempted bool, err error) {
	if cfg.eng.nameAddressedDeletes() {
		unlock, lockErr := lockName(ctx, cfg.name)
		if lockErr != nil {
			return nil, false, fmt.Errorf("create %s: lock name: %w", cfg.name, lockErr)
		}
		defer unlock()
	}
	stdout, _, err = cfg.runner.Run(ctx, args...)
	return stdout, true, err
}

// rollback removes a container Run created but cannot return. A failed
// removal is not hidden: without an immutable ID, Terminate refuses to
// delete when it cannot verify the generation, and the caller must know
// the container was left behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		// %v would flatten the cleanup failure into text, leaving only the
		// original recoverable through errors.Is. Join it instead.
		return withCleanupError(cause, &CleanupError{Container: c.id, Err: err})
	}
	return cause
}

// cleanupFailedCreate removes the container this Run left behind after a
// failed create. It never deletes a pre-existing same-name container: name
// conflicts are skipped, and only a container carrying this process's
// managed+session labels and the exact creation generation is removed. All
// operational work shares one bounded context and is returned to Run so a
// failed cleanup cannot be mistaken for a clean failure.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if keepContainers() {
		return nil
	}
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil
	}
	if !creationRE.MatchString(cfg.creation) {
		return fmt.Errorf("cleanup %s: %w: failed create has no valid creation generation", cfg.name, ErrGenerationReplaced)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupFailedCreateTimeout)
	defer cancel()
	if cfg.eng.nameAddressedDeletes() {
		unlock, err := lockName(cleanupCtx, cfg.name)
		if err != nil {
			return fmt.Errorf("cleanup %s: lock name: %w", cfg.name, err)
		}
		defer unlock()
	}
	cleanupContainer := namedContainer(cfg, cfg.name)
	cleanupContainer.creation = cfg.creation
	info, err := cleanupContainer.inspectFresh(cleanupCtx)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup %s: inspect: %w", cfg.name, err)
	}
	if info.labels[managedLabel] != "true" {
		return nil
	}
	if sess, ok := info.labels[sessionLabel]; !ok || sess != sessionID() {
		return nil
	}
	if cfg.reuse && info.labels[reuseLabel] != "true" {
		return nil
	}
	actual, ok := info.labels[creationLabel]
	if !ok || actual != cfg.creation {
		return nil
	}
	if cfg.reuse && info.state == StateRunning {
		return fmt.Errorf("cleanup %s: running reuse generation may already be adopted; refusing automatic deletion", cfg.name)
	}

	target := cfg.name
	reaperCreation := cfg.creation
	if info.uid != "" {
		if !isImmutableContainerID(cfg.eng, info.uid) {
			return fmt.Errorf("cleanup %s: inspect returned unverified immutable ID %q", cfg.name, info.uid)
		}
		target = info.uid
		reaperCreation = ""
	} else if !cfg.eng.nameAddressedDeletes() {
		return fmt.Errorf("cleanup %s: %w: inspect returned no verified immutable ID", cfg.name, ErrGenerationReplaced)
	}

	// Registration happens only after exact ownership verification. This
	// closes the process-death window between discovering an owned failed
	// create and deleting it without ever registering a peer's container.
	if er, ok := cfg.runner.(cli.ExternalRunner); ok && er.External() {
		bin := er.ExternalBinary()
		if bin == "" {
			bin = cfg.eng.binary()
		}
		registerWithGlobalReaper(cleanupCtx, bin, cfg.eng.reaperSubcommand(), target, reaperCreation)
	}

	_, _, err = cfg.runner.Run(cleanupCtx, cfg.eng.deleteArgs(target)...)
	if err == nil || isNotFound(err) {
		return nil
	}
	err = cli.Classify(cleanupCtx, cfg.runner, err, cfg.eng.probe())
	return fmt.Errorf("cleanup %s: delete %s: %w", cfg.name, target, err)
}

// writeEnvFile stores env vars in a 0600 file under a private temporary
// directory, keeping values out of the process table.
func writeEnvFile(env map[string]string) (path, dir string, err error) {
	dir, err = os.MkdirTemp("", "containergo-env-")
	if err != nil {
		return "", "", err
	}
	var b []byte
	for _, k := range sortedKeys(env) {
		b = append(b, k+"="+env[k]+"\n"...)
	}
	path = filepath.Join(dir, "env")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return path, dir, nil
}

// ID returns the container ID (identical to its name).
func (c *Container) ID() string { return c.id }

func (c *Container) classify(ctx context.Context, err error) error {
	return cli.Classify(ctx, c.runner, err, c.eng.probe())
}

// State returns the current lifecycle state.
func (c *Container) State(ctx context.Context) (State, error) {
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return StateUnknown, err
	}
	return info.state, nil
}

// Stop stops the container. A nil timeout uses the CLI's default grace
// period before the process is killed.
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error {
	target, err := c.operationTarget(ctx)
	if err != nil {
		return err
	}
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err = c.runner.Run(stopCtx, c.eng.stopArgs(target, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. Removing a container that no
// longer exists is a success. A handle with an immutable ID deletes by
// it, so a same-name replacement is never touched. Without one (Apple
// Container) the delete goes by name only when the handle has a valid
// creation generation: a missing or malformed generation fails with
// ErrGenerationReplaced. The generation must match a fresh inspect, and
// inspect and delete run under the stable per-name lock so no other
// process using the guarded protocol can mutate the name in between.
// A direct `container` CLI call, an unguarded library operation, or
// another external actor can still change state, labels, generation, or
// the name target after inspect;
// that point-in-time limitation is not detectable by name (see
// lockName). An inspect failure other than not-found aborts the delete
// rather than risk a replacement.
func (c *Container) Terminate(ctx context.Context) error {
	ctx, cancel := withDefaultTimeout(ctx, terminateTimeout)
	defer cancel()
	if c.eng.immutableID() {
		// Even an ID printed by `run` is bound only after a fresh
		// generation-checked inspect. This keeps a stale or forged handle
		// from turning an immutable-looking string into an unchecked delete.
		fresh, err := c.inspectFresh(ctx)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("terminate %s: verify immutable identity: %w", c.id, err)
		}
		c.inspectMu.Lock()
		target := c.uid
		bound := c.uidBound && verifiedImmutableID(c.eng, target)
		c.inspectMu.Unlock()
		if !bound || fresh.uid != target {
			return fmt.Errorf("%w: %s changed immutable ID", ErrGenerationReplaced, c.id)
		}
		return c.delete(ctx, target)
	}
	if !creationRE.MatchString(c.creation) {
		if c.eng.nameAddressedDeletes() {
			return fmt.Errorf("%w: %s has no valid creation generation", ErrGenerationReplaced, c.id)
		}
		return fmt.Errorf("%w: %s has no verified immutable container ID", ErrGenerationReplaced, c.id)
	}

	unlock := func() {}
	if c.eng.nameAddressedDeletes() {
		var err error
		unlock, err = lockName(ctx, c.id)
		if err != nil {
			return fmt.Errorf("terminate %s: lock name: %w", c.id, err)
		}
	}
	defer unlock()

	info, err := c.inspectFresh(ctx)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("terminate %s: verify generation: %w", c.id, err)
	}
	// An absent generation cannot prove ownership of this handle, so
	// it counts as a replacement too.
	if info.labels[creationLabel] != c.creation {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.eng.nameAddressedDeletes() {
		return c.delete(ctx, c.id)
	}
	if info.uid != "" && isImmutableContainerID(c.eng, info.uid) {
		return c.delete(ctx, info.uid)
	}
	return fmt.Errorf("%w: %s has no verified immutable container ID", ErrGenerationReplaced, c.id)
}

func isImmutableContainerID(eng engine, id string) bool {
	return verifiedImmutableID(eng, id)
}

func (c *Container) delete(ctx context.Context, target string) error {
	delCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err := c.runner.Run(delCtx, c.eng.deleteArgs(target)...)
	if err == nil || isNotFound(err) {
		return nil
	}
	return c.classify(ctx, err)
}

// ContainerIP returns the container's address on its first attached
// network. With the Docker backend on Docker Desktop this address is
// usually not reachable from the host; prefer Endpoint.
func (c *Container) ContainerIP(ctx context.Context) (string, error) {
	info, err := c.cachedInfo(ctx)
	if err != nil {
		return "", err
	}
	if info.ip == "" {
		return "", fmt.Errorf("container %s has no reported IP address", c.id)
	}
	return info.ip, nil
}

// Host returns the address clients should connect to. When multiple
// published ports use different host IPs, prefer Endpoint for the
// specific port — Host returns only the first published binding's
// address (or the container IP / default host when nothing is published).
func (c *Container) Host(ctx context.Context) (string, error) {
	if len(c.published) > 0 {
		addr := c.published[0].connectAddr()
		if !c.eng.directIP() {
			addr = dockerConnectHost(addr, c.eng)
		}
		return addr, nil
	}
	if c.eng.directIP() {
		return c.ContainerIP(ctx)
	}
	return c.eng.defaultHost(), nil
}

// MappedPort resolves a declared container port ("6379/tcp" or "6379")
// to the port clients should dial.
func (c *Container) MappedPort(ctx context.Context, port string) (int, error) {
	_, p, err := c.resolve(ctx, port)
	return p, err
}

// Endpoint returns "host:port" for a declared container port.
func (c *Container) Endpoint(ctx context.Context, port string) (string, error) {
	host, p, err := c.resolve(ctx, port)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// resolve maps a container port to the (host, port) pair to dial.
func (c *Container) resolve(ctx context.Context, port string) (string, int, error) {
	spec, err := parsePortSpec(port)
	if err != nil {
		return "", 0, err
	}
	for _, p := range c.published {
		if p.containerPort == spec.port && p.proto == spec.proto {
			addr := p.connectAddr()
			if !c.eng.directIP() {
				addr = dockerConnectHost(addr, c.eng)
			}
			return addr, p.hostPort, nil
		}
	}
	if !slices.Contains(c.exposed, spec) {
		return "", 0, fmt.Errorf("%w: %s", ErrPortNotExposed, spec)
	}
	if c.eng.directIP() {
		ip, err := c.ContainerIP(ctx)
		if err != nil {
			return "", 0, err
		}
		return ip, spec.port, nil
	}
	// Published-port mode: the backend assigned a host port at start.
	info, err := c.cachedInfo(ctx)
	if err != nil {
		return "", 0, err
	}
	for _, b := range info.bound {
		if b.containerPort == spec.port && b.proto == spec.proto {
			return dockerConnectHost(b.hostAddr, c.eng), b.hostPort, nil
		}
	}
	return "", 0, fmt.Errorf("%w: %s has no host binding", ErrPortNotExposed, spec)
}

// cachedInfo returns the first successful inspect result. Only fields
// that cannot change while the container exists (labels, network
// address, port bindings) should be read from it.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil {
		return c.info, nil
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	c.info = info
	return info, nil
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()

	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	target := c.inspectTargetLocked()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		return nil, err
	}
	if c.eng.immutableID() {
		if !verifiedImmutableID(c.eng, info.uid) {
			return nil, fmt.Errorf("%w: %s inspect returned invalid immutable ID", ErrGenerationReplaced, c.id)
		}
		if c.uid != "" && c.uid != info.uid {
			return nil, fmt.Errorf("%w: %s changed immutable ID", ErrGenerationReplaced, c.id)
		}
		if c.creation != "" && (!creationRE.MatchString(c.creation) || info.labels[creationLabel] != c.creation) {
			return nil, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		c.uid = info.uid
		c.uidBound = true
	}
	return info, nil
}

func (c *Container) verifyHandleIdentity(ctx context.Context, expected *engineInfo, requireSession, requireRunning bool) (*engineInfo, error) {
	fresh, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	if fresh.labels[managedLabel] != "true" {
		return nil, fmt.Errorf("%w: %s is no longer managed", ErrGenerationReplaced, c.id)
	}
	if requireSession && (fresh.labels[sessionLabel] == "" || fresh.labels[sessionLabel] != sessionID()) {
		return nil, fmt.Errorf("%w: %s session changed", ErrGenerationReplaced, c.id)
	}
	if c.eng.nameAddressedDeletes() &&
		(!creationRE.MatchString(c.creation) || fresh.labels[creationLabel] != c.creation) {
		return nil, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.reused && fresh.labels[reuseLabel] != "true" {
		return nil, fmt.Errorf("%w: %s reuse ownership changed", ErrGenerationReplaced, c.id)
	}
	if c.eng.immutableID() && (!verifiedImmutableID(c.eng, fresh.uid) || (expected != nil && fresh.uid != expected.uid)) {
		return nil, fmt.Errorf("%w: %s immutable identity changed", ErrGenerationReplaced, c.id)
	}
	if expected != nil {
		for _, label := range []string{managedLabel, sessionLabel, reuseLabel, reuseGroupLabel, creationLabel} {
			if expected.labels[label] != fresh.labels[label] {
				return nil, fmt.Errorf("%w: %s ownership label %s changed", ErrGenerationReplaced, c.id, label)
			}
		}
	}
	if requireRunning && fresh.state != StateRunning {
		return nil, fmt.Errorf("container %s state changed to %s before return", c.id, fresh.state)
	}
	return fresh, nil
}

func (c *Container) operationTarget(ctx context.Context) (string, error) {
	if !c.eng.immutableID() {
		return c.id, nil
	}
	c.inspectMu.Lock()
	if c.uidBound && verifiedImmutableID(c.eng, c.uid) {
		target := c.uid
		c.inspectMu.Unlock()
		return target, nil
	}
	c.inspectMu.Unlock()
	if _, err := c.inspectFresh(ctx); err != nil {
		return "", err
	}
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	if !c.uidBound || !verifiedImmutableID(c.eng, c.uid) {
		return "", fmt.Errorf("%w: %s has no verified immutable ID", ErrGenerationReplaced, c.id)
	}
	return c.uid, nil
}

func (c *Container) inspectTargetLocked() string {
	if c.eng.immutableID() && verifiedImmutableID(c.eng, c.uid) {
		return c.uid
	}
	return c.id
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func durationOrZero(d *time.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return *d
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitNonEmptyLines splits CLI stdout on newlines and drops blank lines.
func splitNonEmptyLines(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
