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
	// Terminate and the reaper verify it before deleting so a stale
	// handle does not remove a same-name replacement made by this
	// library; see Terminate for the limits of the name-based path.
	creation string
	// uid is the backend's immutable container ID when it has one
	// (Docker). Backend operations target it directly, so a stale handle
	// cannot affect a same-name replacement.
	uid string

	mu        sync.Mutex
	info      *engineInfo // cached first inspect; immutable fields only
	inspectMu sync.Mutex  // serializes inspect and protects uid after publication
	// nameInspect is limited to short-lived lookups used by reuse and
	// failed-create cleanup. A Docker handle returned to callers must not
	// fall back to its logical name for State or any other operation.
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
		cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, classified
	}
	uid := cfg.eng.parseRunID(stdout)
	if requiresImmutableID(cfg.eng) && !validImmutableID(cfg.eng, uid) {
		err := fmt.Errorf("run %s: Docker run returned no valid immutable container ID", cfg.name)
		cleanupFailedCreate(ctx, cfg, err, err)
		return nil, err
	}

	c := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		creation:  cfg.creation,
		uid:       uid,
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

// runCreateLocked serializes Apple Container's name-addressed create with
// generation-checked deletes and the watchdog reaper. The attempted result
// is separate from the error so a lock failure does not trigger cleanup for
// a create command that was never issued.
func runCreateLocked(ctx context.Context, cfg *config, args ...string) (stdout []byte, attempted bool, err error) {
	if cfg.eng.name() != "apple" {
		stdout, _, err = cfg.runner.Run(ctx, args...)
		return stdout, true, err
	}
	unlock, err := lockName(ctx, cfg.name)
	if err != nil {
		return nil, false, err
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
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (container %s left behind: %v)", cause, c.id, err)
	}
	return cause
}

// cleanupFailedCreate best-effort removes the container this Run left
// behind after a failed create. It never deletes a pre-existing
// same-name container: name conflicts are skipped, and only a container
// carrying this process's managed+session labels is removed. The
// creation label must exist and match this Run's generation; a reuse
// create must also carry the reuse label. Docker additionally requires
// a valid ID from that same fresh inspect and never falls back to the
// logical name.
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
	if cfg.reuse && info.labels[reuseLabel] != "true" {
		return
	}
	if !creationRE.MatchString(cfg.creation) {
		return
	}
	actual, ok := info.labels[creationLabel]
	if !ok || actual != cfg.creation {
		return
	}
	target, err := verifiedDeleteTarget(cfg.eng, info, cfg.name)
	if err != nil {
		return
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

// ID returns the logical container name. Docker also has an immutable
// backend ID, but changing the public result would break callers that use
// the name for display and diagnostics.
func (c *Container) ID() string { return c.id }

// operationTarget returns the strongest backend identity currently bound
// to this handle. It is intentionally a small compatibility helper: Apple
// Container has no immutable ID and therefore returns its logical name.
// Callers that issue a backend operation use verifiedOperationTarget below
// so a Docker handle never falls back to that name.
func (c *Container) operationTarget() string {
	c.inspectMu.Lock()
	uid := c.uid
	c.inspectMu.Unlock()
	if requiresImmutableID(c.eng) && validImmutableID(c.eng, uid) {
		return uid
	}
	return c.id
}

func (c *Container) verifiedOperationTarget() (string, error) {
	c.inspectMu.Lock()
	uid := c.uid
	c.inspectMu.Unlock()
	if requiresImmutableID(c.eng) {
		if !validImmutableID(c.eng, uid) {
			return "", fmt.Errorf("container %s: invalid immutable container ID %q", c.id, uid)
		}
		return uid, nil
	}
	if c.id == "" {
		return "", fmt.Errorf("container has no logical name")
	}
	return c.id, nil
}

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
	target, err := c.verifiedOperationTarget()
	if err != nil {
		return err
	}
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err = c.runner.Run(stopCtx, c.eng.stopArgs(target, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. Removing a container that no
// longer exists is a success. A handle with a verified immutable ID
// deletes by that ID, so a same-name replacement is never touched. A
// name-addressed handle must carry a valid creation generation and match
// a fresh inspect under the per-name lock. Handles created before
// generation tracking are deliberately not name-deleted: there is no
// identity with which to prove that a same-name replacement is not the
// object the caller meant.
func (c *Container) Terminate(ctx context.Context) error {
	c.inspectMu.Lock()
	uid := c.uid
	c.inspectMu.Unlock()
	if uid != "" {
		if !validImmutableID(c.eng, uid) {
			return fmt.Errorf("terminate %s: invalid immutable container ID %q", c.id, uid)
		}
		return c.delete(ctx, uid)
	}
	if !validCreationID(c.creation) {
		return fmt.Errorf("terminate %s: refusing name delete without a valid creation generation", c.id)
	}
	_, err := c.terminateByName(ctx, nil, false)
	return err
}

// terminateByName performs the locked inspect/delete critical section for
// a name-addressed operation. expected is the generation/identity seen by
// the caller before this fresh inspect; a nil expected value uses c's
// creation generation. stoppedOnly prevents deletion unless the fresh
// object is actually stopped.
func (c *Container) terminateByName(ctx context.Context, expected *engineInfo, stoppedOnly bool) (bool, error) {
	return c.terminateByNameWithImage(ctx, expected, stoppedOnly, "")
}

func (c *Container) terminateByNameWithImage(ctx context.Context, expected *engineInfo, stoppedOnly bool, image string) (bool, error) {
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return false, fmt.Errorf("terminate %s: lock name: %w", c.id, err)
	}
	defer unlock()

	fresh, err := c.inspectFresh(ctx)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("terminate %s: verify generation: %w", c.id, err)
	}
	if expected != nil {
		if err := sameContainerIdentity(c.eng, expected, fresh); err != nil {
			return false, err
		}
	} else {
		if !validCreationID(c.creation) {
			return false, fmt.Errorf("terminate %s: refusing name delete without a valid creation generation", c.id)
		}
		if fresh.labels[creationLabel] != c.creation {
			return false, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		if requiresImmutableID(c.eng) && !validImmutableID(c.eng, fresh.uid) {
			return false, fmt.Errorf("terminate %s: refusing Docker name fallback without a valid inspect ID", c.id)
		}
	}
	if image != "" && !imagesCompatible(image, fresh.image) {
		return false, fmt.Errorf("terminate %s: image %q does not match existing %q", c.id, image, fresh.image)
	}
	if stoppedOnly && fresh.state != StateStopped {
		return false, nil
	}
	target, err := verifiedDeleteTarget(c.eng, fresh, c.id)
	if err != nil {
		return false, err
	}
	return true, c.delete(ctx, target)
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

	uid := c.uid
	if uid != "" && !validImmutableID(c.eng, uid) {
		return nil, fmt.Errorf("container %s has invalid immutable ID %q", c.id, uid)
	}
	if requiresImmutableID(c.eng) && uid == "" && !c.nameInspect {
		return nil, fmt.Errorf("container %s: refusing Docker name inspect without an immutable ID", c.id)
	}
	target := c.inspectTargetLocked()
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		return nil, wrapNotFound(c.classify(ctx, err))
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		return nil, err
	}
	if uid != "" && requiresImmutableID(c.eng) && info.uid != uid {
		return nil, fmt.Errorf("%w: inspected Docker ID changed", ErrContainerNotFound)
	}
	if c.uid == "" {
		c.uid = info.uid
	}
	return info, nil
}

// inspectTarget prefers an immutable ID so a same-name replacement cannot
// satisfy a Docker inspect. A non-empty malformed value is retained as the
// target and rejected by the caller rather than falling back to the name.
func (c *Container) inspectTarget() string {
	c.inspectMu.Lock()
	defer c.inspectMu.Unlock()
	return c.inspectTargetLocked()
}

func (c *Container) inspectTargetLocked() string {
	if c.uid != "" {
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
