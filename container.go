package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
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
	// including waiting for another process' name lock.
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
	// check unnecessary: a replacement never shares it.
	uid string
	// requestedPlatform is retained so identity-checked inspects can
	// resolve Docker's OS-only top-level Platform field to a complete OCI
	// platform when the caller explicitly selected one.
	requestedPlatform string
	// bootstrap permits a just-created handle to resolve a missing Docker
	// UID from a generation-verified inspect. It is never enabled on a
	// handle returned by WithReuse.
	bootstrap bool
	// identityOptional is used only for short-lived name lookups before a
	// reuse/cleanup caller has established which generation it owns.
	identityOptional bool
	// imageIdentity/image are the immutable image target pinned before
	// create. image is retained as the concise handle field used by the
	// image-identity contract; both are kept in sync for compatibility.
	imageIdentity imageIdentity
	image         imageIdentity

	// inspectMu protects uid and serializes target-bound inspects. The
	// identity cache is protected separately by mu.
	inspectMu sync.RWMutex
	mu        sync.Mutex
	info      *engineInfo // immutable identity snapshot
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
	// Resolve the image once, before create, and pass the immutable
	// reference to the backend. A mutable tag may be used only when the
	// caller explicitly opted into that compatibility escape hatch.
	imageIdentity, err := cfg.ensureImageRef(runCtx, image)
	if err != nil {
		return nil, err
	}
	// Register before the create command. If the process dies between the
	// backend acknowledging the run and Run receiving stdout, the pending
	// reaper entry rechecks the name for a bounded settling window.
	reaperBin := ""
	if er, ok := cfg.runner.(cli.ExternalRunner); ok && er.External() && !keepContainers() {
		reaperBin = er.ExternalBinary()
		if reaperBin == "" {
			reaperBin = cfg.eng.binary()
		}
		if validCreationGeneration(cfg.creation) {
			if err := preRegisterWithGlobalReaper(reaperBin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation); err != nil {
				log.Printf("container-go: reaper pre-registration failed: %v", err)
			}
		}
	}
	runImage := image
	if imageIdentity.reference != "" {
		runImage = imageIdentity.reference
	}
	stdout, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, runImage, envFile)...)
	if err != nil {
		if !attempted {
			if reaperBin != "" {
				retireReaperEntry(reaperBin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation, "")
			}
			return nil, err
		}
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if keepContainers() {
			retained, retainedErr := retainedFailedCreate(ctx, cfg, err, classified)
			return retained, withCleanupError(classified, retainedErr)
		}
		cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified)
		if cleanupErr == nil && reaperBin != "" {
			retireReaperEntry(reaperBin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation, "")
		}
		return nil, withCleanupError(classified, cleanupErr)
	}

	uid := cfg.eng.parseRunID(stdout)
	if reaperBin != "" && validCreationGeneration(cfg.creation) {
		if cfg.eng.name() == "docker" && validDockerUID(uid) {
			if err := promotePendingDockerIDWithGlobalReaper(reaperBin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation, uid); err != nil {
				log.Printf("container-go: reaper immutable-ID promotion failed: %v", err)
			}
		} else if err := completePendingWithGlobalReaper(reaperBin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation); err != nil {
			log.Printf("container-go: reaper completion failed: %v", err)
		}
	}
	if cfg.eng.name() == "docker" && !validDockerUID(uid) {
		identityErr := fmt.Errorf("run %s: Docker run returned no valid immutable container ID", cfg.name)
		cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr)
		if cleanupErr == nil && reaperBin != "" {
			retireReaperEntry(reaperBin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation, "")
		}
		return nil, withCleanupError(identityErr, cleanupErr)
	}
	c := &Container{
		id:                cfg.name,
		runner:            cfg.runner,
		eng:               cfg.eng,
		exposed:           cfg.exposed,
		published:         cfg.published,
		creation:          cfg.creation,
		uid:               uid,
		requestedPlatform: cfg.platform,
		imageIdentity:     imageIdentity,
		image:             imageIdentity,
	}
	// A successful create is not enough: the backend may have resolved a
	// tag to a different local image. Bind the handle only after checking
	// the container's immutable image identity against the pre-create pin.
	if imageIdentity.pinned {
		info, inspectErr := c.inspectFresh(ctx)
		if inspectErr != nil {
			return rollbackResult(ctx, c, inspectErr)
		}
		if err := verifyContainerImageIdentity(c, info); err != nil {
			return rollbackResult(ctx, c, err)
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

// rollback removes a container Run created but cannot return. KEEP skips
// deletion and cancels any watchdog ownership. A failed removal is not
// hidden: without an immutable ID, Terminate refuses to delete when it
// cannot verify the generation, and the caller must know the container
// was left behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if c == nil {
		return cause
	}
	if keepContainers() {
		c.inspectMu.RLock()
		uid := c.uid
		c.inspectMu.RUnlock()
		if err := cancelReuseReaperHandoffWithGeneration(c.runner, c.eng, c.id, c.creation, uid, c.id); err != nil {
			return withCleanupError(cause, err)
		}
		return cause
	}
	if c.reused {
		return cause
	}
	if err := c.Terminate(context.WithoutCancel(ctx)); err != nil {
		return withCleanupError(cause, fmt.Errorf("container %s left behind: %w", c.id, err))
	}
	return cause
}

// cleanupFailedCreate removes only a container that can be tied to this
// exact create. It never treats an ambiguous running generation as an
// owned failure: a peer may have adopted it while the create command was
// still reporting an error.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if keepContainers() {
		return nil
	}
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	unlock, err := lockNameForBackend(cleanupCtx, cfg.eng, cfg.name)
	if err != nil {
		return fmt.Errorf("cleanup container %s: lock name: %w", cfg.name, err)
	}
	defer unlock()

	ctr := namedContainer(cfg, cfg.name)
	info, err := ctr.inspectFreshLocked(cleanupCtx)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("cleanup container %s: inspect: %w", cfg.name, err)
	}
	if !failedCreateOwned(cfg, info) {
		return nil
	}
	if info.state != StateStopped && info.state != StateCreated {
		return fmt.Errorf("cleanup container %s: %s generation may already be adopted; refusing automatic deletion", cfg.name, info.state)
	}
	if cfg.eng.name() == "docker" {
		if !validDockerUID(info.uid) {
			return fmt.Errorf("cleanup container %s: inspect returned no valid Docker ID", cfg.name)
		}
		return ctr.delete(cleanupCtx, info.uid)
	}
	if cfg.eng.name() == "apple" {
		if info.uid != "" {
			return fmt.Errorf("cleanup container %s: Apple inspect returned an unexpected ID", cfg.name)
		}
		return ctr.delete(cleanupCtx, cfg.name)
	}
	return fmt.Errorf("cleanup container %s: unknown backend", cfg.name)
}

func failedCreateOwned(cfg *config, info *engineInfo) bool {
	if info == nil || info.labels[managedLabel] != "true" ||
		info.labels[sessionLabel] != sessionID() {
		return false
	}
	if !validCreationGeneration(cfg.creation) ||
		info.labels[creationLabel] != cfg.creation {
		return false
	}
	if cfg.reuse {
		if info.labels[reuseLabel] != "true" || !imageFromInfo(info).pinned {
			return false
		}
	}
	return validateReuseIdentity(cfg.eng, info) == nil
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
// backend ID, but changing the public result would break callers that
// use the name for display and diagnostics.
func (c *Container) ID() string { return c.id }

// operationTarget returns the strongest backend target currently bound
// to this handle. Apple uses its name only after generation verification;
// a missing or malformed Docker UID returns an empty string rather than
// falling back to a name.
func (c *Container) operationTarget() string {
	c.inspectMu.RLock()
	uid := c.uid
	c.inspectMu.RUnlock()
	if validDockerUID(uid) {
		return uid
	}
	if c.eng.name() == "apple" {
		c.inspectMu.RLock()
		valid := validCreationGeneration(c.creation)
		c.inspectMu.RUnlock()
		if valid {
			return c.id
		}
	}
	return ""
}

// targetForInspect returns the target to inspect. A just-created handle
// may use its name once to discover a Docker UID, but only while its
// valid generation label can bind the result. Returned reuse handles do
// not get that bootstrap exception.
func (c *Container) targetForInspect() (string, error) {
	if c.identityOptional {
		return c.id, nil
	}
	c.inspectMu.RLock()
	uid := c.uid
	creation := c.creation
	bootstrap := c.bootstrap
	c.inspectMu.RUnlock()
	switch c.eng.name() {
	case "docker":
		if validDockerUID(uid) {
			return uid, nil
		}
		if bootstrap && validCreationGeneration(creation) {
			return c.id, nil
		}
		return "", generationReplaced(c.id)
	case "apple":
		if uid != "" {
			return "", generationReplaced(c.id)
		}
		if !validCreationGeneration(creation) {
			return "", generationReplaced(c.id)
		}
		return c.id, nil
	default:
		return "", errIdentity("unknown backend cannot inspect a container identity")
	}
}

// verifiedOperationTarget closes the name-addressed operation race for
// Apple handles. Docker operations use a validated immutable UID. For
// Apple, the generation check and the operation are serialized by the
// cooperating-process name lock; an external CLI remains outside that
// coordination, so every use of a returned name still checks the
// generation again.
func (c *Container) verifiedOperationTarget(ctx context.Context) (string, func(), error) {
	noop := func() {}
	if c.identityOptional {
		return "", noop, generationReplaced(c.id)
	}
	c.inspectMu.RLock()
	uid := c.uid
	creation := c.creation
	c.inspectMu.RUnlock()
	if c.eng.name() == "docker" {
		if !validDockerUID(uid) {
			return "", noop, generationReplaced(c.id)
		}
		if c.reused {
			info, err := c.inspectFresh(ctx)
			if err != nil {
				return "", noop, err
			}
			if err := c.validateHandleInfo(info); err != nil {
				return "", noop, err
			}
		}
		return uid, noop, nil
	}
	if c.eng.name() != "apple" {
		return "", noop, errIdentity("unknown backend cannot address a container")
	}
	if !validCreationGeneration(creation) {
		return "", noop, generationReplaced(c.id)
	}
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return "", noop, fmt.Errorf("lock name: %w", err)
	}
	info, err := c.inspectFreshLocked(ctx)
	if err != nil {
		unlock()
		// inspectFreshLocked already joins the replacement sentinel
		// with a missing reused target; do not duplicate its text.
		return "", noop, err
	}
	if err := c.validateHandleInfo(info); err != nil {
		unlock()
		return "", noop, err
	}
	return c.id, unlock, nil
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
	target, unlock, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err = c.runner.Run(stopCtx, c.eng.stopArgs(target, timeout)...)
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
	c.inspectMu.RLock()
	uid := c.uid
	creation := c.creation
	bootstrap := c.bootstrap
	c.inspectMu.RUnlock()
	if c.eng.name() == "docker" {
		if validDockerUID(uid) {
			if c.reused {
				info, err := c.inspectFresh(ctx)
				if err != nil {
					if isNotFound(err) {
						return retireContainerReaper(c)
					}
					return fmt.Errorf("terminate %s: verify reused Docker identity: %w", c.id, err)
				}
				if err := validateReuseIdentity(c.eng, info); err != nil ||
					info.uid != uid || info.labels[managedLabel] != "true" ||
					info.labels[reuseLabel] != "true" || !validCreationGeneration(info.labels[creationLabel]) ||
					info.labels[creationLabel] != creation {
					return generationReplaced(c.id)
				}
			}
			return c.deleteAndRetire(ctx, uid)
		}
		// A just-created handle may resolve a missing run ID once, but
		// only through a generation-verified inspect. Never fall back to
		// the name for a returned or reused handle.
		if !bootstrap || !validCreationGeneration(creation) {
			return fmt.Errorf("terminate %s: %w: handle has no valid immutable ID", c.id, ErrGenerationReplaced)
		}
		verifyCtx, verifyCancel := withDefaultTimeout(ctx, queryTimeout)
		defer verifyCancel()
		if _, err := c.inspectFresh(verifyCtx); err != nil {
			if isNotFound(err) {
				return retireContainerReaper(c)
			}
			return fmt.Errorf("terminate %s: verify immutable ID: %w", c.id, err)
		}
		c.inspectMu.RLock()
		uid = c.uid
		c.inspectMu.RUnlock()
		if !validDockerUID(uid) {
			return fmt.Errorf("terminate %s: %w: inspect returned no valid immutable ID", c.id, ErrGenerationReplaced)
		}
		return c.deleteAndRetire(ctx, uid)
	}
	if c.eng.name() != "apple" {
		return errIdentity("unknown backend cannot terminate a container")
	}
	// A name-addressed handle without a valid generation cannot prove
	// that it still owns the name. Never turn an invalid identity into an
	// unconditional delete.
	if !validCreationGeneration(creation) {
		return fmt.Errorf("terminate %s: %w: handle has no valid creation generation", c.id, ErrGenerationReplaced)
	}
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return fmt.Errorf("terminate %s: lock name: %w", c.id, err)
	}
	defer unlock()
	info, err := c.inspectFreshLocked(ctx)
	if isNotFound(err) {
		return retireContainerReaper(c)
	}
	if err != nil {
		return fmt.Errorf("terminate %s: verify generation: %w", c.id, err)
	}
	if err := validateReuseIdentity(c.eng, info); err != nil ||
		info.labels[managedLabel] != "true" ||
		!validCreationGeneration(info.labels[creationLabel]) ||
		info.labels[creationLabel] != creation {
		return generationReplaced(c.id)
	}
	if c.reused && info.labels[reuseLabel] != "true" {
		return generationReplaced(c.id)
	}
	// The locked, identity-bound inspect has already rejected a missing,
	// malformed, or different Apple generation. The only safe target is
	// the name under this lock.
	return c.deleteAndRetire(ctx, c.id)
}

func (c *Container) deleteAndRetire(ctx context.Context, target string) error {
	err := c.delete(ctx, target)
	if err != nil {
		return err
	}
	if retireErr := retireContainerReaper(c); retireErr != nil {
		return retireErr
	}
	return nil
}

func retireContainerReaper(c *Container) error {
	if c == nil {
		return nil
	}
	er, ok := c.runner.(cli.ExternalRunner)
	if !ok || !er.External() {
		return nil
	}
	binary := er.ExternalBinary()
	if binary == "" {
		binary = c.eng.binary()
	}
	c.inspectMu.RLock()
	uid := c.uid
	c.inspectMu.RUnlock()
	return unregisterHandoffWithGlobalReaper(binary, c.eng.reaperSubcommand(), c.id, c.creation, uid)
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
	return ipFromInfo(c.id, info)
}

func ipFromInfo(id string, info *engineInfo) (string, error) {
	if info.ip == "" {
		return "", fmt.Errorf("container %s has no reported IP address", id)
	}
	return info.ip, nil
}

// Host returns the address clients should connect to. When multiple
// published ports use different host IPs, prefer Endpoint for the
// specific port — Host returns only the first published binding's
// address (or the container IP / default host when nothing is published).
func (c *Container) Host(ctx context.Context) (string, error) {
	// Resolve against a fresh identity-checked inspect even for an
	// explicitly published address. This prevents a stale handle from
	// silently returning an endpoint for a replacement.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", err
	}
	if len(c.published) > 0 {
		addr := c.published[0].connectAddr()
		if !c.eng.directIP() {
			addr = dockerConnectHost(addr, c.eng)
		}
		return addr, nil
	}
	if c.eng.directIP() {
		return ipFromInfo(c.id, info)
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

	// Always inspect once before returning an endpoint. Explicit
	// published bindings still need an identity check; otherwise a stale
	// handle could advertise the replacement's address.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", 0, err
	}
	if published != nil {
		if c.reused && !hasPublishedBinding(info.bound, *published) {
			return "", 0, fmt.Errorf("published port %s is no longer bound as requested", published.raw)
		}
		addr := published.connectAddr()
		if !c.eng.directIP() {
			addr = dockerConnectHost(addr, c.eng)
		}
		return addr, published.hostPort, nil
	}
	if c.eng.directIP() {
		ip, err := ipFromInfo(c.id, info)
		if err != nil {
			return "", 0, err
		}
		return ip, spec.port, nil
	}
	// Published-port mode: the backend assigned a host port at start.
	for _, b := range info.bound {
		if b.containerPort == spec.port && b.proto == spec.proto {
			return dockerConnectHost(b.hostAddr, c.eng), b.hostPort, nil
		}
	}
	return "", 0, fmt.Errorf("%w: %s has no host binding", ErrPortNotExposed, spec)
}

// cachedInfo returns the first successful inspect result. Callers that
// need current state, addresses, or bindings use inspectDynamic.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
	c.mu.Lock()
	cached := c.info
	c.mu.Unlock()
	if cached != nil {
		if err := c.bindInspectInfo(cached); err != nil {
			return nil, err
		}
		return cached, nil
	}
	return c.inspectFresh(ctx)
}

// inspectDynamic reads current state and verifies the immutable identity
// of the target before returning it.
func (c *Container) inspectDynamic(ctx context.Context) (*engineInfo, error) {
	return c.inspectFresh(ctx)
}

// bindInspectInfo validates the identity represented by info and records
// an immutable snapshot. It is deliberately strict for reused handles:
// missing or malformed generation/UID values are replacement errors.
func (c *Container) validateHandleInfo(info *engineInfo) error {
	if err := validateReuseIdentity(c.eng, info); err != nil {
		return generationReplaced(c.id)
	}
	if info.labels[managedLabel] != "true" ||
		!validCreationGeneration(info.labels[creationLabel]) {
		return generationReplaced(c.id)
	}
	c.inspectMu.RLock()
	creation := c.creation
	c.inspectMu.RUnlock()
	if !validCreationGeneration(creation) || info.labels[creationLabel] != creation {
		return generationReplaced(c.id)
	}
	if c.reused && info.labels[reuseLabel] != "true" {
		return generationReplaced(c.id)
	}
	expected := c.imageIdentity
	if !expected.pinned {
		expected = c.image
	}
	if expected.pinned {
		observed := containerImageIdentity(c.eng, info)
		if !observed.pinned || !requestedImageIdentitiesCompatible(expected, observed) {
			return generationReplaced(c.id)
		}
	}
	return nil
}

func (c *Container) bindInspectInfo(info *engineInfo) error {
	if info == nil {
		return withGenerationReplaced(c.id, errIdentity("empty inspect result"))
	}
	c.inspectMu.RLock()
	uid := c.uid
	creation := c.creation
	bootstrap := c.bootstrap
	c.inspectMu.RUnlock()

	switch c.eng.name() {
	case "docker":
		if !validDockerUID(info.uid) {
			return withGenerationReplaced(c.id, errIdentity("Docker inspect returned no valid immutable container ID"))
		}
		if !validCreationGeneration(creation) || !validCreationGeneration(info.labels[creationLabel]) ||
			info.labels[creationLabel] != creation {
			return withGenerationReplaced(c.id, errIdentity("Docker inspect returned no matching creation generation"))
		}
		if uid != "" {
			if !validDockerUID(uid) || info.uid != uid {
				return generationReplaced(c.id)
			}
		} else if !bootstrap {
			return generationReplaced(c.id)
		}
		if uid == "" {
			c.inspectMu.Lock()
			c.uid = info.uid
			c.bootstrap = false
			c.inspectMu.Unlock()
		}
	case "apple":
		if info.uid != "" {
			return withGenerationReplaced(c.id, errIdentity("Apple inspect unexpectedly returned a container ID"))
		}
		if !validCreationGeneration(creation) ||
			!validCreationGeneration(info.labels[creationLabel]) ||
			info.labels[creationLabel] != creation {
			return generationReplaced(c.id)
		}
	default:
		return errIdentity("unknown backend cannot bind a container identity")
	}
	if info.labels[managedLabel] != "true" || (c.reused && info.labels[reuseLabel] != "true") {
		return generationReplaced(c.id)
	}
	if c.eng.name() == "docker" && strings.TrimSpace(dockerCreation(info)) == "" {
		return generationReplaced(c.id)
	}
	c.mu.Lock()
	if c.info != nil && !sameEngineIdentity(c.eng, c.info, info) {
		c.mu.Unlock()
		return generationReplaced(c.id)
	}
	if c.info == nil {
		c.info = info
	}
	c.mu.Unlock()
	return nil
}
func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	if !c.identityOptional && c.eng.name() == "apple" {
		unlock, err := lockName(ctx, c.id)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: lock name: %w", c.id, err)
		}
		info, err := c.inspectFreshLocked(ctx)
		unlock()
		return info, err
	}
	return c.inspectFreshLocked(ctx)
}

func (c *Container) inspectFreshLocked(ctx context.Context) (*engineInfo, error) {
	target, err := c.targetForInspect()
	if err != nil {
		return nil, err
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		classified := wrapNotFound(c.classify(ctx, err))
		if c.reused && isNotFound(classified) {
			return nil, withGenerationReplaced(c.id, classified)
		}
		return nil, classified
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		if c.reused {
			return nil, withGenerationReplaced(c.id, err)
		}
		return nil, err
	}
	if c.requestedPlatform != "" {
		info.platform, err = resolveContainerPlatform(qCtx, c.eng, c.runner, info, c.requestedPlatform)
		if err != nil {
			return nil, fmt.Errorf("resolve container platform: %w", err)
		}
		if err := checkPlatformCompatibility(c.requestedPlatform, info.platform); err != nil {
			return nil, fmt.Errorf("container platform: %w", err)
		}
	}
	if !c.identityOptional {
		if err := c.bindInspectInfo(info); err != nil {
			return nil, err
		}
	}
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
