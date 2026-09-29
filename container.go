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

// Container is a handle to a container created by Run. When Run
// returns a non-nil handle together with an error under
// CONTAINERGO_KEEP=1, it is a partial handle for a retained failed
// container; its normal lifecycle methods remain usable. WithReuse
// handles refer to a shared container, including retained failure
// handles, so they are not private ownership.
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
	// createdHere marks a WithReuse handle whose generation this process
	// created during the current ensure. Only such a generation may be
	// cleaned up automatically after a post-create failure; an attached
	// generation belongs to its peers.
	createdHere bool
	// creation is the unique generation ID stored in creationLabel.
	// Terminate and the reaper verify it before deleting so a stale
	// handle does not remove a same-name replacement made by this
	// library; see Terminate for the limits of the name-based path.
	creation string
	// uid is the backend's immutable container ID when it has one
	// (Docker). Deletes target it directly, which makes the generation
	// check unnecessary: a replacement never shares it.
	uid string

	mu   sync.Mutex
	info *engineInfo // cached first inspect; immutable fields only
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On a non-reuse failure after creation, the
// container is rolled back and removed by default before returning. With
// CONTAINERGO_KEEP=1, a non-reuse container that this Run can prove it
// created is retained and returned alongside the error; that partial
// handle is revalidated first, and a handle that can no longer be bound
// to this Run's generation is dropped in favor of the verification
// error. The partial handle remains usable for explicit inspection,
// execution, copying, and Terminate. WithReuse switches to get-or-create
// and never force-deletes a shared generation; under KEEP it may return
// a verified shared retained handle with the error. See WithReuse for
// the shared-handle lifecycle.
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
	stdout, _, err := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if keepContainers() {
			retained, retainedErr := retainedFailedCreate(ctx, cfg, err, classified)
			return retained, withCleanupError(classified, retainedErr)
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, withCleanupError(classified, cleanupErr)
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
		if c.uid != "" {
			registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), c.uid, "")
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

// rollback removes a container created by a non-reuse Run when Run
// cannot return it. It honors CONTAINERGO_KEEP=1 by leaving the container
// in place. A failed removal is not hidden: without an immutable ID,
// Terminate refuses to delete when it cannot verify the generation, and
// the caller must know the container was left behind.
//
// A shared WithReuse generation is never removed here: the backend
// published it the moment create returned, so a peer may already have
// adopted it. Shared generations are resolved by the reuse paths, which
// clean up only while a fresh inspect still proves the generation is
// unadopted.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if keepContainers() {
		return cause
	}
	if c.reused {
		refusal := fmt.Errorf("container %s is a shared reuse generation; refusing automatic deletion", c.id)
		return withCleanupError(cause, refusal)
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
// process's managed+session labels is removed. The creation label must
// exist and match this Run's generation; a reuse create must also carry
// the reuse label. A reuse generation is only removed while it is still
// unadopted (created or stopped): a running or otherwise ambiguous
// shared generation is refused and the refusal is returned to Run so the
// container stays visible instead of being force-deleted under a peer.
// A missing container is already clean and returns nil; any other
// failure to inspect or remove it is returned to Run so the container
// left behind is visible to the caller.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if keepContainers() {
		return nil
	}
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(cleanupCtx, cfg.name)
	if err != nil {
		return fmt.Errorf("cleanup container %s: lock name: %w", cfg.name, err)
	}
	defer unlock()
	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectFresh(cleanupCtx)
	if err != nil {
		if isNotFoundFor(cfg.eng, cfg.name, err) {
			return nil
		}
		return fmt.Errorf("cleanup container %s: inspect: %w", cfg.name, err)
	}
	if !failedCreateOwned(cfg, info) {
		return nil
	}
	if err := checkUnadoptedReuse(cfg, info); err != nil {
		return err
	}
	target := cfg.name
	if info.uid != "" {
		target = info.uid
	}
	// The candidate is verified owned and still unadopted: hand it to the
	// watchdog before the delete so a failed or interrupted delete cannot
	// orphan it. Shared generations and diagnostic retention are
	// deliberately excluded.
	watchdogBinary, watchdogTarget := failedCreateWatchdog(cfg)
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	if err := ctr.delete(delCtx, target); err != nil {
		if watchdogTarget != "" {
			protectFailedCreate(cfg, watchdogBinary, watchdogTarget)
		}
		return fmt.Errorf("cleanup container %s: %w", cfg.name, err)
	}
	if watchdogTarget != "" {
		retireFailedCreateReaper(cfg, watchdogBinary, watchdogTarget)
	}
	return nil
}

// checkUnadoptedReuse refuses automatic deletion of a shared generation
// that a peer may already have adopted. Only created and stopped
// generations are unadopted; running and every other backend state are
// ambiguous and must be left to an explicit Terminate or Prune.
func checkUnadoptedReuse(cfg *config, info *engineInfo) error {
	if !cfg.reuse {
		return nil
	}
	switch info.state {
	case StateCreated, StateStopped:
		return nil
	default:
		return fmt.Errorf("cleanup container %s: %s reuse generation may already be adopted; refusing automatic deletion",
			cfg.name, info.state)
	}
}

// failedCreateWatchdog reports the identity the watchdog needs to
// reclaim a verified failed-create candidate, or "" when the candidate
// must not be watchdog-owned: shared generations outlive the creating
// process by design, CONTAINERGO_KEEP=1 asks the caller to inspect the
// container, and only real CLI children can be reaped.
func failedCreateWatchdog(cfg *config) (binary, target string) {
	if keepContainers() || cfg.reuse || cfg.name == "" || !creationRE.MatchString(cfg.creation) {
		return "", ""
	}
	er, ok := cfg.runner.(cli.ExternalRunner)
	if !ok || !er.External() {
		return "", ""
	}
	binary = er.ExternalBinary()
	if binary == "" {
		binary = cfg.eng.binary()
	}
	// Name-addressed registration: the reaper inspects the name, matches
	// this generation, and then deletes the backend's immutable ID when
	// the backend reports one.
	return binary, cfg.name
}

// protectFailedCreate hands a verified failed-create candidate to the
// watchdog after automatic deletion failed, so the container cannot
// outlive the process unnoticed. Best effort by design: the caller is
// already reporting the delete failure.
func protectFailedCreate(cfg *config, binary, target string) {
	registerWithGlobalReaper(binary, cfg.eng.reaperSubcommand(), target, cfg.creation)
}

// retireFailedCreateReaper withdraws the candidate again once automatic
// deletion succeeded, so the watchdog never acts on a resolved entry.
func retireFailedCreateReaper(cfg *config, binary, target string) {
	unregisterFromGlobalReaper(binary, cfg.eng.reaperSubcommand(), target, cfg.creation)
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
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err := c.runner.Run(stopCtx, c.eng.stopArgs(c.id, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. It is an explicit operation
// and is not disabled by CONTAINERGO_KEEP. Removing a container that no
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
	if c.uid != "" {
		return c.delete(ctx, c.uid)
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
	if isNotFoundFor(c.eng, c.id, err) {
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
	if err == nil || isDeleteNotFound(c.eng, target, err) {
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
	if c.uid == "" {
		c.uid = info.uid
	}
	return info, nil
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(c.id)...)
	if err != nil {
		classified := c.classify(ctx, err)
		if isNotFoundFor(c.eng, c.id, classified) {
			return nil, wrapNotFound(classified)
		}
		return nil, classified
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
