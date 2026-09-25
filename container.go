package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// network is the explicitly requested backend network. An empty
	// value means the backend's daemon-selected default.
	network         string
	networkExplicit bool
	// defaultNetwork is Docker's authoritative daemon default identity
	// (bridge on Linux, nat on Windows), learned lazily for implicit
	// compatibility checks.
	defaultNetwork string
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
	uid string

	mu        sync.Mutex
	info      *engineInfo // immutable identity snapshot; dynamic data is never cached
	inspectMu sync.Mutex  // serializes inspect target selection and UID publication
	// nameInspect is limited to short-lived name lookups used by reuse
	// discovery and failed-create cleanup. It never publishes an inspected
	// Docker UID to a caller-visible handle.
	nameInspect bool
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, the container is
// removed before returning. WithReuse switches to get-or-create; see
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
	if err := cfg.eng.checkConfig(ctx, cfg); err != nil {
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
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, errors.Join(classified, cleanupErr)
	}

	uid := cfg.eng.parseRunID(stdout)
	if requiresImmutableID(cfg.eng) && !validImmutableID(cfg.eng, uid) {
		identityErr := identityError("Docker run returned no valid immutable container ID")
		cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr)
		return nil, errors.Join(identityErr, cleanupErr)
	}

	c := &Container{
		id:              cfg.name,
		runner:          cfg.runner,
		eng:             cfg.eng,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		creation:        cfg.creation,
		uid:             uid,
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
// removal is not hidden: without an immutable ID, Terminate refuses to
// delete when it cannot verify the generation, and the caller must know
// the container was left behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (container %s left behind: %v)", cause, c.id, err)
	}
	return cause
}

// cleanupFailedCreate removes only a container whose exact failed-create
// generation and ownership labels have been freshly verified. A Docker
// inspect by name is used only to discover a candidate; deletion always
// targets its validated immutable UID. A missing generation, session, or
// reuse marker is a refusal, never permission to fall back to the name.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil
	}
	if !validCreationID(cfg.creation) {
		return identityError("failed create has no valid creation generation")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	if !requiresImmutableID(cfg.eng) {
		unlock, err := lockName(cleanupCtx, cfg.name)
		if err != nil {
			return fmt.Errorf("cleanup %s: lock name: %w", cfg.name, err)
		}
		defer unlock()
	}
	info, err := namedContainer(cfg, cfg.name).inspectFresh(cleanupCtx)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup %s: inspect: %w", cfg.name, err)
	}
	if info == nil || info.labels[managedLabel] != "true" {
		return nil
	}
	if info.labels[sessionLabel] != sessionID() {
		return nil
	}
	if !validCreationID(info.labels[creationLabel]) || info.labels[creationLabel] != cfg.creation {
		return nil
	}
	if cfg.reuse {
		if info.labels[reuseLabel] != "true" || info.state == StateRunning {
			return nil
		}
	}
	if !requiresImmutableID(cfg.eng) && info.uid != "" {
		return identityError("name-addressed cleanup inspect returned an immutable ID")
	}
	target, err := verifiedDeleteTarget(cfg.eng, info, cfg.name)
	if err != nil {
		return err
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	_, _, err = cfg.runner.Run(delCtx, cfg.eng.deleteArgs(target)...)
	if err == nil || isNotFound(err) {
		return nil
	}
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

// ID returns the logical container name. Docker handles also retain the
// immutable backend ID used for internal operations.
func (c *Container) ID() string { return c.id }

func (c *Container) classify(ctx context.Context, err error) error {
	return cli.Classify(ctx, c.runner, err, c.eng.probe())
}

// State returns the current lifecycle state.
func (c *Container) State(ctx context.Context) (State, error) {
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return StateUnknown, err
	}
	return info.state, nil
}

// Stop stops the container. A nil timeout uses the CLI's default grace
// period before the process is killed.
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error {
	target, err := c.verifiedOperationTarget()
	if err != nil {
		return err
	}
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err = c.runner.Run(stopCtx, c.eng.stopArgs(target, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. A Docker handle must carry a
// validated immutable UID; a name-only handle is never an acceptable
// fallback. Apple Container has no immutable UID, so its name path is
// allowed only with a valid generation and a fresh locked inspect.
func (c *Container) Terminate(ctx context.Context) error {
	if requiresImmutableID(c.eng) {
		target, err := c.verifiedOperationTarget()
		if err != nil {
			return fmt.Errorf("terminate %s: %w", c.id, err)
		}
		return c.delete(ctx, target)
	}
	if c.eng.name() == "apple" {
		c.inspectMu.Lock()
		creation := c.creation
		uid := c.uid
		c.inspectMu.Unlock()
		if uid != "" {
			return identityError("Apple inspect unexpectedly returned a container ID")
		}
		if !validCreationID(creation) {
			return identityError("Apple handle has no valid creation generation")
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
		if err := sameContainerIdentity(c.eng, &engineInfo{labels: map[string]string{creationLabel: creation}}, info); err != nil {
			return err
		}
		return c.delete(ctx, c.id)
	}
	target, err := c.verifiedOperationTarget()
	if err != nil {
		return err
	}
	return c.delete(ctx, target)
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
	info, err := c.inspectDynamic(ctx)
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
// Docker host networking returns the daemon host but does not infer a
// listening port; Docker none networking has no reachable host.
func (c *Container) Host(ctx context.Context) (string, error) {
	// Inspect before returning any address, including an explicitly
	// published direct-IP endpoint. A requested publish string is not
	// proof that the handle still names the same live container.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", err
	}
	if c.eng.directIP() {
		if len(c.published) > 0 {
			return c.published[0].connectAddr(), nil
		}
		if info.ip == "" {
			return "", fmt.Errorf("container %s has no reported IP address", c.id)
		}
		return info.ip, nil
	}
	if err := c.attachDefaultNetwork(ctx, info); err != nil {
		return "", err
	}
	if err := c.validateNetworkInfo(info); err != nil {
		return "", err
	}
	switch info.networkMode {
	case dockerNetworkNone:
		return "", fmt.Errorf("%w: container %s uses Docker network mode %q", ErrNoReachableHost, c.id, info.networkMode)
	case dockerNetworkHost:
		if len(c.published) > 0 {
			return "", dockerNetworkEndpointError(info.networkMode)
		}
		return c.eng.defaultHost(), nil
	}
	if len(c.published) > 0 {
		p := c.published[0]
		b, ok := matchingPublishedBinding(info.bound, p)
		if !ok {
			return "", fmt.Errorf("%w: published port %q has no host binding in Docker network mode %q", ErrPortNotExposed, p.raw, info.networkMode)
		}
		host, err := dockerBindingConnectHost(b, c.eng)
		if err != nil {
			return "", err
		}
		return host, nil
	}
	return c.eng.defaultHost(), nil
}

// MappedPort resolves a declared container port ("6379/tcp" or "6379")
// to the port clients should dial.
func (c *Container) MappedPort(ctx context.Context, port string) (int, error) {
	_, p, err := c.resolve(ctx, port)
	return p, err
}

// Endpoint returns "host:port" for a port declared via WithExposedPorts
// or WithPublishedPort. It does not infer a host-network service port.
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
	var published *publishSpec
	for i := range c.published {
		p := &c.published[i]
		if p.containerPort == spec.port && p.proto == spec.proto {
			published = p
			break
		}
	}
	if published == nil && !slices.Contains(c.exposed, spec) {
		return "", 0, fmt.Errorf("%w: %s", ErrPortNotExposed, spec)
	}
	// Always perform one fresh identity-checked inspect before returning
	// an endpoint. This is especially important for direct-IP backends:
	// a published address is otherwise just a stale option value and can
	// be returned for a replacement container without any backend read.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", 0, err
	}
	if c.eng.directIP() {
		if published != nil {
			return published.connectAddr(), published.hostPort, nil
		}
		if info.ip == "" {
			return "", 0, fmt.Errorf("container %s has no reported IP address", c.id)
		}
		return info.ip, spec.port, nil
	}
	// Published-port mode: the backend assigned a host port at start.
	// Docker may discard -p in host mode, so the request is never used
	// as proof that a binding exists; inspect must confirm it.
	if err := c.attachDefaultNetwork(ctx, info); err != nil {
		return "", 0, err
	}
	if err := c.validateNetworkInfo(info); err != nil {
		return "", 0, err
	}
	if err := dockerNetworkEndpointError(info.networkMode); err != nil {
		return "", 0, err
	}
	if published != nil {
		b, ok := matchingPublishedBinding(info.bound, *published)
		if !ok {
			return "", 0, fmt.Errorf("%w: published port %q has no host binding in Docker network mode %q", ErrPortNotExposed, published.raw, info.networkMode)
		}
		host, err := dockerBindingConnectHost(b, c.eng)
		if err != nil {
			return "", 0, err
		}
		return host, b.hostPort, nil
	}
	if b, ok := matchingExposedBinding(info.bound, spec); ok {
		host, err := dockerBindingConnectHost(b, c.eng)
		if err != nil {
			return "", 0, err
		}
		return host, b.hostPort, nil
	}
	return "", 0, fmt.Errorf("%w: %s has no host binding in Docker network mode %q", ErrPortNotExposed, spec, info.networkMode)
}

// validateNetworkInfo prevents a requested network mode from being
// mistaken for the mode the daemon actually used. Apple Container does
// not report networkMode, so this check is intentionally Docker-specific.
func (c *Container) validateNetworkInfo(info *engineInfo) error {
	if c.eng.name() != "docker" {
		return nil
	}
	requested := c.network
	if !c.networkExplicit {
		requested = ""
	}
	return dockerNetworkModeErrorDefault(requested, info.networkMode, info.networkNames, info.defaultNetwork)
}

// cachedInfo returns the immutable identity snapshot. Dynamic fields are
// intentionally not cached; endpoint and lifecycle callers use
// inspectDynamic instead.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
	if c.nameInspect {
		return c.inspectFresh(ctx)
	}
	c.mu.Lock()
	cached := c.info
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.rememberIdentity(info); err != nil {
		return nil, err
	}
	c.mu.Lock()
	cached = c.info
	c.mu.Unlock()
	return cached, nil
}

// inspectDynamic reads lifecycle, network, and port data on every call.
// These values can change after a container has been inspected.
func (c *Container) inspectDynamic(ctx context.Context) (*engineInfo, error) {
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	c.applyCachedDefaultNetwork(info)
	if c.nameInspect {
		return info, nil
	}
	if err := c.rememberIdentity(info); err != nil {
		return nil, err
	}
	return info, nil
}

func (c *Container) rememberIdentity(info *engineInfo) error {
	identity := immutableInfo(info)
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil && !sameImmutableInfo(c.info, identity) {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.uid != "" && c.uid != identity.uid {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.uid == "" && c.info != nil && c.info.uid != "" && c.info.uid != identity.uid {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.creation != "" {
		actual := identity.labels[creationLabel]
		if actual != "" && actual != c.creation {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		// Once a name-only handle has observed its generation, losing
		// the label is also evidence of a replacement.
		if actual == "" && c.uid == "" && c.info != nil && c.info.labels[creationLabel] != "" {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
	}
	if c.info == nil {
		c.info = identity
	} else {
		c.info = mergeImmutableInfo(c.info, identity)
	}
	return nil
}

func immutableInfo(info *engineInfo) *engineInfo {
	if info == nil {
		return nil
	}
	labels := make(map[string]string, len(info.labels))
	for key, value := range info.labels {
		labels[key] = value
	}
	return &engineInfo{uid: info.uid, image: info.image, labels: labels}
}

func sameImmutableInfo(a, b *engineInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.uid != "" && b.uid != "" && a.uid != b.uid {
		return false
	}
	if a.image != "" && b.image != "" && a.image != b.image {
		return false
	}
	for key, value := range a.labels {
		if other, ok := b.labels[key]; ok && other != value {
			return false
		}
	}
	return true
}

func mergeImmutableInfo(old, current *engineInfo) *engineInfo {
	merged := immutableInfo(old)
	if merged.uid == "" {
		merged.uid = current.uid
	}
	if merged.image == "" {
		merged.image = current.image
	}
	for key, value := range current.labels {
		if _, ok := merged.labels[key]; !ok {
			merged.labels[key] = value
		}
	}
	return merged
}

// inspectTargetLocked returns the strongest target currently bound to
// the handle. The caller must hold inspectMu. A temporary name lookup is
// the only path allowed to use a logical name for Docker.
func (c *Container) inspectTargetLocked() string {
	if c.uid != "" {
		return c.uid
	}
	return c.id
}

func (c *Container) operationTarget() string {
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	if requiresImmutableID(c.eng) && !validImmutableID(c.eng, c.uid) {
		return ""
	}
	if c.eng.name() == "apple" && !validCreationID(c.creation) {
		return ""
	}
	return c.inspectTargetLocked()
}

// verifiedOperationTarget is the safe target for lifecycle, copy, exec,
// and log operations. In particular, a Docker handle with an empty or
// malformed UID fails before a CLI call instead of falling back to its
// name. Apple handles likewise require a valid generation before using a
// name-addressed operation.
func (c *Container) verifiedOperationTarget() (string, error) {
	c.inspectMu.Lock()
	uid := c.uid
	creation := c.creation
	c.inspectMu.Unlock()
	if requiresImmutableID(c.eng) {
		if !validImmutableID(c.eng, uid) {
			return "", identityError("Docker operation has no valid immutable container ID")
		}
		return uid, nil
	}
	if c.eng.name() == "apple" {
		if uid != "" {
			return "", identityError("Apple handle unexpectedly carries a container ID")
		}
		if !validCreationID(creation) {
			return "", identityError("Apple operation has no valid creation generation")
		}
	}
	if c.id == "" {
		return "", identityError("container has no logical name")
	}
	return c.operationTarget(), nil
}

func configNeedsDefaultNetwork(cfg *config) bool {
	return cfg != nil && cfg.eng != nil && cfg.eng.name() == "docker" &&
		(!cfg.networkExplicit || cfg.network == dockerNetworkDefault)
}

func (c *Container) needsDefaultNetwork() bool {
	return c.eng.name() == "docker" &&
		(!c.networkExplicit || c.network == dockerNetworkDefault)
}

func (c *Container) ensureDefaultNetwork(ctx context.Context) (string, error) {
	if !c.needsDefaultNetwork() {
		return "", nil
	}
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	if c.defaultNetwork != "" {
		return c.defaultNetwork, nil
	}
	name, err := dockerDefaultNetwork(ctx, c.runner, c.eng)
	if err != nil {
		return "", err
	}
	c.defaultNetwork = name
	return name, nil
}

func (c *Container) attachDefaultNetwork(ctx context.Context, info *engineInfo) error {
	if !c.needsDefaultNetwork() {
		return nil
	}
	name, err := c.ensureDefaultNetwork(ctx)
	if err != nil {
		return err
	}
	info.defaultNetwork = name
	return nil
}

// attachDefaultNetworkForConfig deliberately does not consult a shared
// reuse handle's cache. Each concurrent caller must resolve daemon
// metadata with its own request and runner; otherwise the first caller's
// network selection can decide a later caller's compatibility.
func attachDefaultNetworkForConfig(ctx context.Context, cfg *config, info *engineInfo) error {
	if !configNeedsDefaultNetwork(cfg) {
		return nil
	}
	name, err := dockerDefaultNetwork(ctx, cfg.runner, cfg.eng)
	if err != nil {
		return err
	}
	info.defaultNetwork = name
	return nil
}

func (c *Container) applyCachedDefaultNetwork(info *engineInfo) {
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	if c.defaultNetwork != "" {
		info.defaultNetwork = c.defaultNetwork
	}
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	return c.inspectFreshLocked(ctx)
}

func (c *Container) inspectFreshLocked(ctx context.Context) (*engineInfo, error) {
	uid := c.uid
	if requiresImmutableID(c.eng) {
		if uid != "" && !validImmutableID(c.eng, uid) {
			return nil, identityError("Docker handle has an invalid immutable container ID")
		}
		if uid == "" && !c.nameInspect {
			return nil, identityError("refusing Docker name inspect without a verified immutable ID")
		}
	}
	if !c.nameInspect && c.eng.name() == "apple" && !validCreationID(c.creation) {
		return nil, identityError("Apple handle has no valid creation generation")
	}

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
	if requiresImmutableID(c.eng) {
		if !validImmutableID(c.eng, info.uid) {
			return nil, identityError("Docker inspect returned no valid immutable container ID")
		}
		if uid != "" && uid != info.uid {
			return nil, identityError("Docker inspect returned a different immutable container ID")
		}
	}
	if !c.nameInspect && c.eng.name() == "apple" {
		if info.uid != "" {
			return nil, identityError("Apple inspect unexpectedly returned a container ID")
		}
		if !validCreationID(c.creation) || !validCreationID(info.labels[creationLabel]) || info.labels[creationLabel] != c.creation {
			return nil, identityError("Apple inspect returned a different creation generation")
		}
	}
	// A name-addressed discovery deliberately returns the inspected UID to
	// its caller as data only. It is never published into c.uid here; the
	// caller must first validate managed/reuse/session ownership.
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
