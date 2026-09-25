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
	// check unnecessary: a replacement never shares it. Access it only
	// through immutableID/rememberImmutableID after construction.
	uid   string
	uidMu sync.RWMutex

	// nameInspect is set only on temporary lookup containers used to
	// reconcile a reuse/failed-create name. A normal Docker handle must
	// never inspect by its logical name.
	nameInspect bool

	mu   sync.Mutex
	info *engineInfo // cached first inspect; immutable fields only
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, the container is
// normally removed before returning. With CONTAINERGO_KEEP=1, a verified
// failed create may return a partial handle with its error so the caller
// can inspect or explicitly terminate it. WithReuse switches to
// get-or-create; see WithReuse for the shared-handle lifecycle.
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
		if keepContainers() {
			retained, retainedErr := retainedFailedCreate(ctx, cfg, err, classified)
			return retained, withCleanupError(classified, retainedErr)
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, withCleanupError(classified, cleanupErr)
	}

	runID := cfg.eng.parseRunID(stdout)
	c := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		creation:  cfg.creation,
	}
	c.rememberImmutableID(runID)
	if usesImmutableIDs(cfg.eng) {
		if uid := c.immutableID(); !dockerIDRE.MatchString(uid) {
			// Without the daemon-assigned ID there is no safe Docker
			// delete target. Do not fall back to the user-visible name.
			return nil, fmt.Errorf("run %s: backend did not return a full 64-hex container ID", cfg.name)
		}
	}
	// The reaper only backs real CLI containers; with an injected
	// test runner there is nothing external to clean up. With an
	// immutable ID the reaper deletes by it and needs no generation.
	if er, ok := cfg.runner.(cli.ExternalRunner); ok && er.External() && !keepContainers() {
		bin := er.ExternalBinary()
		if bin == "" {
			bin = cfg.eng.binary()
		}
		if usesImmutableIDs(cfg.eng) {
			registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), c.immutableID(), "")
		} else {
			registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation)
		}
	}

	for _, f := range cfg.files {
		if err := c.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return rollbackResult(ctx, c, err)
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
			return rollbackResult(ctx, c, err)
		}
	}
	return c, nil
}

// runCreateLocked serializes a name-addressed create with guarded
// delete/prune paths. The attempted result is separate from the error so
// callers do not run failed-create cleanup when acquiring the lock itself
// failed and no create command was issued.
func runCreateLocked(ctx context.Context, cfg *config, args ...string) (stdout []byte, attempted bool, err error) {
	if !usesNameAddressedDeletes(cfg.eng) {
		stdout, _, err = cfg.runner.Run(ctx, args...)
		return stdout, true, err
	}

	lockCtx, lockCancel := withDefaultTimeout(ctx, queryTimeout)
	defer lockCancel()
	unlock, err := lockName(lockCtx, cfg.name)
	if err != nil {
		return nil, false, fmt.Errorf("create %s: lock name: %w", cfg.name, err)
	}
	defer unlock()

	stdout, _, err = cfg.runner.Run(ctx, args...)
	return stdout, true, err
}

// rollback removes a container Run created but cannot return. A failed
// removal is not hidden: without an immutable ID, Terminate refuses to
// delete when it cannot verify the generation, and the caller must know
// the container was left behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if keepContainers() {
		return cause
	}
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		cleanupErr := fmt.Errorf("container %s left behind: %w", c.id, err)
		return withCleanupError(cause, cleanupErr)
	}
	return cause
}

// cleanupFailedCreate removes the container this Run left behind after
// a failed create. It never deletes a pre-existing same-name container:
// name conflicts are skipped, and only a container carrying this
// process's managed+session labels is removed. When the creation
// generation is known it must also match. A missing container is already
// clean and returns nil; any other failure to inspect or remove it is
// returned to Run so the container left behind is visible to the caller.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if keepContainers() {
		return nil
	}
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()

	var unlock func()
	var err error
	if usesNameAddressedDeletes(cfg.eng) {
		unlock, err = lockName(cleanupCtx, cfg.name)
		if err != nil {
			return fmt.Errorf("cleanup container %s: lock name: %w", cfg.name, err)
		}
		defer unlock()
	}
	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectTargetFreshRetry(cleanupCtx, cfg.name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("cleanup container %s: inspect: %w", cfg.name, err)
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
	if !creationRE.MatchString(cfg.creation) {
		return nil
	}
	actual, ok := info.labels[creationLabel]
	if !ok || actual != cfg.creation {
		return nil
	}
	if cfg.reuse && info.state == StateRunning {
		return fmt.Errorf("cleanup container %s: running reuse generation may already be adopted; refusing automatic deletion", cfg.name)
	}
	if cfg.reuse && info.state != StateStopped && info.state != StateCreated {
		return fmt.Errorf("cleanup container %s: reuse generation is %s; refusing automatic deletion", cfg.name, info.state)
	}
	target := cfg.name
	if usesImmutableIDs(cfg.eng) {
		if !dockerIDRE.MatchString(info.uid) {
			return fmt.Errorf("cleanup container %s: backend did not report a full immutable ID", cfg.name)
		}
		target = info.uid
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	if err := ctr.delete(delCtx, target); err != nil {
		return fmt.Errorf("cleanup container %s: %w", cfg.name, err)
	}
	return nil
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
	info, err := c.inspectCurrent(ctx)
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
	stopWithTarget := c.withCurrentTarget
	if usesNameAddressedDeletes(c.eng) && !creationRE.MatchString(c.creation) {
		// Preserve the legacy logical-name behavior for an unbound
		// diagnostic handle, while every generation-bound handle still
		// gets the full identity check.
		stopWithTarget = c.withHandleTarget
	}
	return stopWithTarget(stopCtx, func(target string) error {
		_, _, err := c.runner.Run(stopCtx, c.eng.stopArgs(target, timeout)...)
		return wrapNotFound(c.classify(ctx, err))
	})
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
	ctx, cancel := withDefaultTimeout(ctx, terminateTimeout)
	defer cancel()
	if usesImmutableIDs(c.eng) {
		uid := c.immutableID()
		if !dockerIDRE.MatchString(uid) {
			return fmt.Errorf("terminate %s: missing full immutable container ID; refusing name fallback", c.id)
		}
		return c.delete(ctx, uid)
	}
	if !usesNameAddressedDeletes(c.eng) {
		return c.delete(ctx, c.id)
	}
	if !creationRE.MatchString(c.creation) {
		return fmt.Errorf("terminate %s: missing creation generation; refusing name-addressed delete", c.id)
	}
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return fmt.Errorf("terminate %s: lock name: %w", c.id, err)
	}
	defer unlock()
	info, err := c.inspectTargetFreshRetry(ctx, c.id)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("terminate %s: verify generation: %w", c.id, err)
	}
	if err := c.identityMatches(info); err != nil {
		return fmt.Errorf("terminate %s: %w", c.id, err)
	}
	return c.delete(ctx, c.id)
}

func (c *Container) delete(ctx context.Context, target string) error {
	delCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err := c.runner.Run(delCtx, c.eng.deleteArgs(target)...)
	if err == nil {
		return nil
	}
	classified := c.classify(ctx, err)
	if isNotFound(classified) {
		return nil
	}
	return classified
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

// cachedInfo returns an identity-safe inspect result. Docker snapshots
// are reusable because its UID is immutable; Apple snapshots are refreshed
// under the name lock so a replacement generation cannot supply stale
// network or port data.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil && !usesNameAddressedDeletes(c.eng) {
		if !usesImmutableIDs(c.eng) ||
			(dockerIDRE.MatchString(c.immutableID()) && c.info.uid == c.immutableID()) {
			return c.info, nil
		}
	}
	info, err := c.inspectCurrent(ctx)
	if err != nil {
		return nil, err
	}
	c.info = info
	c.rememberImmutableID(info.uid)
	return info, nil
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
