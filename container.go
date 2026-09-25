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
	// A non-empty generation is checked by Terminate before a
	// generation-guarded name delete; empty legacy handles still take
	// the name path. See Terminate and issues #83/#84 for the limits.
	creation string
	// uid is the backend's immutable container ID when it has one
	// (Docker). Deletes target it directly, which makes the generation
	// check unnecessary: a replacement never shares it.
	uid string

	mu   sync.Mutex
	info *engineInfo // cached first inspect; currently also retains dynamic IP and bindings
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, a non-reuse Run
// attempts to remove the container before returning. A WithReuse wait
// failure leaves the shared container in place; see WithReuse for the
// shared-handle lifecycle.
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
		cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, classified
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
	// test runner there is nothing external to clean up. An immutable
	// ID can be used as a deletion target, but this checkout still
	// rejects full Docker IDs during reaper registration; #73 is required
	// before the reaper can register them.
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
	return c, nil
}

// rollback removes a container Run created but cannot return. A failed
// removal is not hidden: the caller must know when the container was
// left behind. Without an immutable ID, a valid generation is checked
// before name deletion; an empty generation currently takes the legacy
// name path, so the generation guarantee is not universal.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (container %s left behind: %v)", cause, c.id, err)
	}
	return cause
}

// cleanupFailedCreate best-effort considers the container this Run left
// behind after a failed create. It skips name conflicts and only considers
// a container carrying this process's managed+session labels. If the
// creation label is present it must also match; an absent label is
// currently accepted in this best-effort path, not treated as proof that
// the handle owns the live name. Issue #83 tracks closing that fail-open
// case.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) {
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockName(cleanupCtx, cfg.name)
	if err != nil {
		return
	}
	defer unlock()
	info, err := namedContainer(cfg, cfg.name).inspectFresh(cleanupCtx)
	if err != nil {
		return
	}
	if info.labels[managedLabel] != "true" {
		return
	}
	if sess, ok := info.labels[sessionLabel]; !ok || sess != sessionID() {
		return
	}
	if cfg.creation != "" {
		if actual, ok := info.labels[creationLabel]; ok && actual != cfg.creation {
			return
		}
	}
	target := cfg.name
	if info.uid != "" {
		target = info.uid
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	_, _, _ = cfg.runner.Run(delCtx, cfg.eng.deleteArgs(target)...)
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

// ID returns the handle's logical container ID, which is the configured
// name. Docker also retains an immutable backend ID internally, but that
// backend identity is not exposed through this method.
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
// period before the process is killed. For a non-nil timeout, the
// current backends convert the value to whole seconds by truncating
// fractional values; negative and extreme durations are not rejected,
// and the library adds the duration to its query budget without
// saturation. No library-side maximum is enforced; see #89.
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error {
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err := c.runner.Run(stopCtx, c.eng.stopArgs(c.id, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. Removing a container that no
// longer exists is a success. A handle with an immutable ID deletes by
// it, so a same-name replacement is never touched. For a name-addressed
// handle with a non-empty creation generation, Terminate requires a
// matching fresh inspect and holds the per-name lock across inspect and
// delete. A handle with an empty generation currently takes the legacy
// name-delete path, so it does not provide that replacement check; issues
// #83 and #84 track the reuse and generation gaps. An external
// `container delete` plus re-create inside the lock window is not
// detectable by name.
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

// ContainerIP returns the address reported for the container. Apple
// inspect data uses the first attached network. For Docker, selection
// among multiple networks is unspecified when no top-level address is
// present. With the Docker backend on Docker Desktop this address is
// usually not reachable from the host; prefer Endpoint. On this checkout
// the first successful endpoint inspect is cached, so a later network
// change can leave this value stale; issue #85 tracks refreshing dynamic
// endpoint data.
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
// Endpoint-related values may come from the first cached inspect on this
// checkout and can be stale; issue #85 tracks refreshing dynamic data.
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
// to the port clients should dial. On this checkout a daemon-assigned
// binding may come from the first cached inspect and can be stale; issue
// #85 tracks refreshing dynamic endpoint data.
func (c *Container) MappedPort(ctx context.Context, port string) (int, error) {
	_, p, err := c.resolve(ctx, port)
	return p, err
}

// Endpoint returns "host:port" for a declared container port. On this
// checkout the first successful endpoint inspect is cached, so an IP or
// daemon-assigned host port can be stale after a network or binding change;
// issue #85 tracks refreshing dynamic endpoint data.
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

// cachedInfo returns the first successful inspect result. The current
// implementation stores the whole record, including dynamic IP and port
// bindings, so endpoint-related callers can observe stale data after a
// lifecycle or network change. Issue #85 tracks separating immutable
// identity from dynamic endpoint data and refreshing the latter.
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
