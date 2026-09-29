package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
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
	// (Docker). Deletes target it directly, which makes the generation
	// check unnecessary: a replacement never shares it.
	//
	// It is promoted from the first inspect, so it is written long after the
	// handle is published. Guarded by uidMu rather than mu, because readers
	// on the Terminate path must not hold the inspect lock.
	uid   string
	uidMu sync.RWMutex

	mu   sync.Mutex
	info *engineInfo // cached first inspect; immutable fields only
}

// immutableID returns the backend's immutable container ID, or "" when the
// backend has none. The ID is only ever promoted from empty to a real value,
// so a caller that observes "" may re-read after a failed operation.
func (c *Container) immutableID() string {
	c.uidMu.RLock()
	defer c.uidMu.RUnlock()
	return c.uid
}

// setImmutableID records the backend's immutable container ID. It never
// downgrades an existing value, so a concurrent promotion cannot clear it.
func (c *Container) setImmutableID(uid string) {
	if uid == "" {
		return
	}
	c.uidMu.Lock()
	if c.uid == "" {
		c.uid = uid
	}
	c.uidMu.Unlock()
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, the container is
// normally removed before returning. If a post-create environment-file
// cleanup cannot be completed, Run returns the usable handle together
// with the joined error so the container is not orphaned. WithReuse
// switches to get-or-create; see WithReuse for the shared-handle
// lifecycle.
func Run(ctx context.Context, image string, opts ...Option) (result *Container, retErr error) {
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
	// Establish the run budget before env-file security preflight. The
	// preflight takes the root advisory lock and may otherwise outlive a
	// canceled caller while another process is holding that lock.
	runCtx, cancel := withDefaultTimeout(ctx, runTimeout)
	defer cancel()

	// Reject Windows before an image pull or any env-file storage setup.
	// chmod's 0600/0700 bits do not provide per-user secrecy there.
	if len(cfg.env) > 0 {
		if err := ensureEnvFileSecurity(); err != nil {
			return nil, err
		}
		// Validate the private root and reclaim provably stale directories
		// before spending time on an image pull or backend create. The
		// security cleanup has its own finite budget in addition to the
		// caller's run deadline.
		cleanupCtx, cleanupCancel := context.WithTimeout(runCtx, envFileSecurityTimeout)
		err := cleanupStaleEnvFilesContext(cleanupCtx)
		cleanupCancel()
		if err != nil {
			// A trusted directory whose cleanup is still pending must not
			// turn a shared reuse attach into a failed flight. Other
			// security-scan failures remain fail-closed.
			if !cfg.reuse || !isRetryablePendingEnvCleanupError(err) {
				return nil, err
			}
		}
	}
	if cfg.reuse {
		if err := runCtx.Err(); err != nil {
			return nil, err
		}
		return reuseRun(runCtx, image, cfg)
	}
	if cfg.name == "" {
		cfg.name = newContainerName()
	}
	cfg.creation = newCreationID()

	// The pull policy brings the image into the local store before the
	// run command; both share the aggregated flight so concurrent Runs
	// of the same image pull once.
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
	}

	// Do not put secrets on disk while an image pull may be waiting for
	// minutes. The backend reads this file only while handling run. If the
	// first removal fails, envDir remains set so the deferred path retries
	// before returning; the first error is still preserved below.
	var envFile, envDir string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFileContext(runCtx, cfg.env)
		if err != nil {
			if dir != "" {
				// A late root-lock error can return a published directory
				// together with the write error. Keep ownership until a
				// bounded retry has also failed.
				defer func() {
					if retryErr := retryEnvFileCleanupWithError(&dir); retryErr != nil {
						retErr = joinEnvFileCleanupError(retErr, retryErr)
					}
				}()
				return nil, joinEnvFileCleanupError(err, cleanupEnvFileWithRetry(dir))
			}
			return nil, err
		}
		envFile, envDir = path, dir
		defer func() {
			if retryErr := retryEnvFileCleanupWithError(&envDir); retryErr != nil {
				retErr = joinEnvFileCleanupError(retErr, retryErr)
			}
		}()
	}
	stdout, _, runErr := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, image, envFile)...)
	envCleanupErr := cleanupEnvFileAfterUseContext(runCtx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}
	if runErr != nil {
		classified := cli.Classify(ctx, cfg.runner, runErr, cfg.eng.probe())
		if cleanupErr := cleanupFailedCreate(ctx, cfg, runErr, classified); cleanupErr != nil {
			return nil, joinEnvFileCleanupError(withCleanupError(classified, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
		}
		return nil, joinEnvFileCleanupError(classified, envCleanupErr)
	}

	c := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		creation:  cfg.creation,
		uid:       cfg.eng.parseRunID(stdout),
	}
	// The reaper only backs real CLI containers; with an injected
	// test runner there is nothing external to clean up. With an
	// immutable ID the reaper deletes by it and needs no generation.
	if er, ok := cfg.runner.(cli.ExternalRunner); ok && er.External() && !keepContainers() {
		bin := er.ExternalBinary()
		if bin == "" {
			bin = cfg.eng.binary()
		}
		if uid := c.immutableID(); uid != "" {
			registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), uid, "")
		} else {
			registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation)
		}
	}
	// Do not perform file copies or readiness polling while the env file
	// still exists. The handle is usable, and the deferred retry retains
	// ownership for a later safe cleanup attempt.
	if envCleanupErr != nil {
		return c, envCleanupErr
	}

	for _, f := range cfg.files {
		if err := c.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return nil, c.rollback(ctx, joinEnvFileCleanupError(err, envCleanupErr))
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
			return nil, c.rollback(ctx, joinEnvFileCleanupError(err, envCleanupErr))
		}
	}
	return c, envCleanupErr
}

// rollback removes a container Run created but cannot return. A failed removal
// is not hidden: without an immutable ID, Terminate refuses to delete when it
// cannot verify the generation, and the caller must know the container was left
// behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		// %v would flatten the cleanup failure into text, leaving only the
		// original recoverable through errors.Is. Join it instead.
		return withCleanupError(cause, &CleanupError{Container: c.id, Err: err})
	}
	return cause
}

// cleanupFailedCreate removes the container this Run left behind after a
// failed create, and reports whether it succeeded.
//
// It never deletes a pre-existing same-name container: name conflicts are
// skipped, and only a container carrying this process's managed+session labels
// is removed. When the creation generation is known it must also match. Those
// skips are successes, because there is nothing of ours left to remove.
//
// Every other failure is returned, so the caller can tell a caller-visible
// "the container is still there" apart from a silent best-effort attempt. A
// container that cannot even be inspected may still exist, so that case is a
// failure too rather than an assumption that it is gone.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(cleanupCtx, cfg.name)
	if err != nil {
		return fmt.Errorf("lock name for cleanup: %w", err)
	}
	defer unlock()
	info, err := namedContainer(cfg, cfg.name).inspectFresh(cleanupCtx)
	if err != nil {
		// A container that cannot be inspected may still be running, so this
		// is reported rather than treated as "nothing to clean up".
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect before cleanup: %w", err)
	}
	if info.labels[managedLabel] != "true" {
		return nil
	}
	if sess, ok := info.labels[sessionLabel]; !ok || sess != sessionID() {
		return nil
	}
	if cfg.creation != "" {
		if actual, ok := info.labels[creationLabel]; ok && actual != cfg.creation {
			return nil
		}
	}
	target := cfg.name
	if info.uid != "" {
		target = info.uid
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	if _, _, err := cfg.runner.Run(delCtx, cfg.eng.deleteArgs(target)...); err != nil {
		// The container passed the ownership checks, so it exists and is
		// ours. A failure here means it is still running.
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete %s: %w", target, err)
	}
	return nil
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
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err := c.runner.Run(stopCtx, c.eng.stopArgs(c.id, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. Removing a container that no
// longer exists is a success. A handle with an immutable ID deletes by
// it, so a same-name replacement is never touched. Without one (Apple
// Container) the delete goes by name: the creation generation must
// match a fresh inspect, and inspect and delete run under the per-name
// lock so no other process using this library can delete and recreate
// the name in between; an external `container delete` plus re-create
// inside that window is not detectable by name (see lockName). An
// inspect failure other than not-found aborts the delete rather than
// risk a replacement.
func (c *Container) Terminate(ctx context.Context) error {
	if uid := c.immutableID(); uid != "" {
		return c.delete(ctx, uid)
	}
	if c.creation == "" {
		return c.delete(ctx, c.id)
	}
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return fmt.Errorf("terminate %s: lock name: %w", c.id, err)
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
	if info.uid != "" {
		return c.delete(ctx, info.uid)
	}
	return c.delete(ctx, c.id)
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
	// Delegate, so the "never downgrade a promoted value" invariant lives in
	// one place rather than being reimplemented here.
	c.setImmutableID(info.uid)
	return info, nil
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(c.id)...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	return c.eng.parseInspect(stdout, c.id)
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
