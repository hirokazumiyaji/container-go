package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/internal/ports"
	"github.com/hirokazumiyaji/container-go/wait"
)

const (
	managedLabel    = domain.LabelManaged
	sessionLabel    = domain.LabelSession
	reuseLabel      = domain.LabelReuse
	reuseGroupLabel = domain.LabelReuseGroup
	creationLabel   = domain.LabelCreation

	queryTimeout = 30 * time.Second
	// runTimeout also covers an implicit image pull.
	runTimeout = 10 * time.Minute

	// dynamicInspectTTL bounds how long Host/Endpoint/MappedPort/ContainerIP
	// may reuse a dynamic inspect snapshot. It matches the wait package's
	// lifecycle probe cadence (once per second) so readiness polls do not
	// spawn a backend CLI process on every 100ms Endpoint retry, while
	// network and port data are still not retained indefinitely.
	dynamicInspectTTL = time.Second
)

var (
	// terminateTimeout bounds the complete generation-checked termination,
	// including waiting for another process' name lock. It is a var so tests
	// can exercise the bounded cleanup contract without a long wait.
	terminateTimeout = queryTimeout
)

// contextLock is a zero-value mutex whose acquisition can be canceled.
// It protects identity publication and serializes backend inspects without
// allowing a canceled caller to wait indefinitely for a long inspect.
type contextLock struct {
	once sync.Once
	gate chan struct{}
}

func (l *contextLock) channel() chan struct{} {
	l.once.Do(func() {
		l.gate = make(chan struct{}, 1)
		l.gate <- struct{}{}
	})
	return l.gate
}

func (l *contextLock) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gate := l.channel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
		if err := ctx.Err(); err != nil {
			gate <- struct{}{}
			return err
		}
		return nil
	}
}

func (l *contextLock) Unlock() {
	l.channel() <- struct{}{}
}

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
type State = domain.State

const (
	StateRunning    State = domain.StateRunning
	StateStopped    State = domain.StateStopped
	StateStopping   State = domain.StateStopping
	StateCreated    State = domain.StateCreated
	StateRestarting State = domain.StateRestarting
	StatePaused     State = domain.StatePaused
	StateUnknown    State = domain.StateUnknown
)

// lockPack holds the mutable, synchronized fields of a Container. Shared
// copies share one pack so promotion and inspect caches stay coherent
// without copying mutex values.
type lockPack struct {
	uidMu          sync.RWMutex
	uid            string
	mu             sync.Mutex
	info           *engineInfo      // immutable identity snapshot; excludes network/port/state
	dynamic        *engineInfo      // short-TTL network/port/state snapshot for endpoint reads
	dynamicAt      time.Time        // wall time when dynamic was stored; zero if unset
	dynamicGen     uint64           // bumped by clearDynamic so in-flight refreshes cannot repopulate
	now            func() time.Time // optional test clock; nil means time.Now
	dynamicMu      contextLock      // serializes dynamic refresh so older inspects cannot overwrite newer ones
	inspectMu      contextLock      // serializes inspect target selection and UID publication
	defaultNetwork string           // Docker daemon default identity, learned lazily
	copyVersionMu  contextLock      // serializes the Docker copy-out version check
	copyVersionOK  atomic.Bool      // a successful copy-out version check is reusable for this handle
	attachMu       sync.Mutex
	attachCancel   context.CancelFunc
	attachDone     chan struct{}
	// attachOutput is the WithAttachOutput destination of the current
	// stream. It is retained so a restart can re-attach the same writer.
	attachOutput io.Writer
	// attachErr accumulates the terminal failures across the attach streams
	// of this handle, kept for AttachedError. Reattaching does not clear it so
	// a caller that retained the previous stream's AttachedDone can still
	// observe why that stream ended; later failures are joined to it.
	attachErr error
	// attachGate carries the right to write to the attach destination. It is
	// a channel rather than a mutex so a replacement stream can give up when
	// the sink has wedged its predecessor instead of blocking behind it.
	attachGate chan struct{}
}

func (p *lockPack) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

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
	// reused marks a WithReuse handle. Cleanup, TerminateContainer,
	// and the watchdog reaper skip these so shared containers survive
	// process exit. Stop/CopyToContainer/CopyFileFromContainer also
	// refuse with ErrSharedContainer unless allowDestructive is set
	// via Shared. Explicit Terminate still removes them.
	reused bool
	// allowDestructive lifts Stop/Copy refusals on a reused handle
	// without clearing reused, so Cleanup/TerminateContainer still skip.
	allowDestructive bool
	// creation is the unique generation ID stored in creationLabel.
	// Terminate and the reaper verify it before deleting so a stale
	// handle does not remove a same-name replacement made by this
	// library; see Terminate for the limits of the name-based path.
	creation string

	// image is the image identity resolved immediately before create.
	// reference is what was passed to run; it is immutable-pinned
	// unless the caller explicitly allowed the mutable-tag fallback.
	image imageIdentity

	// nameInspect is limited to short-lived name lookups used by reuse
	// discovery and failed-create cleanup. It never publishes an inspected
	// Docker UID to a caller-visible handle.
	nameInspect bool

	queryTimeout time.Duration
	probeTimeout time.Duration

	sync *lockPack
	// dockerMeta is the Client-owned Docker metadata cache shared by the
	// handles it creates. Package-level Run leaves it nil so separate
	// operations cannot reuse another target's metadata.
	dockerMeta              *dockerMetadataCache
	diagnosticRedactorValue *cli.Redactor
	secretsMu               sync.RWMutex
	targetHost              string
}

// TargetHost returns the client-facing default host configured when this
// container was created.
func (c *Container) TargetHost() string {
	if c.targetHost != "" {
		return c.targetHost
	}
	if c.eng != nil {
		return c.eng.defaultHost()
	}
	return "127.0.0.1"
}

func (c *Container) effectiveQueryTimeout() time.Duration {
	if c.queryTimeout > 0 {
		return c.queryTimeout
	}
	return queryTimeout
}

func (c *Container) effectiveProbeTimeout() time.Duration {
	if c.probeTimeout > 0 {
		return c.probeTimeout
	}
	return 0
}

func (c *Container) pack() *lockPack {
	ptr := (*unsafe.Pointer)(unsafe.Pointer(&c.sync))
	if p := atomic.LoadPointer(ptr); p != nil {
		return (*lockPack)(p)
	}
	created := &lockPack{}
	if atomic.CompareAndSwapPointer(ptr, nil, unsafe.Pointer(created)) {
		return created
	}
	return (*lockPack)(atomic.LoadPointer(ptr))
}

func (c *Container) refusesSharedOps() bool {
	return c.reused && !c.allowDestructive
}

// immutableID returns the backend's immutable container ID, or "" when the
// backend has none. The ID is only ever promoted from empty to a real value,
// so a caller that observes "" may re-read after a failed operation.
func (c *Container) immutableID() string {
	c.pack().uidMu.RLock()
	defer c.pack().uidMu.RUnlock()
	return c.pack().uid
}

func (c *Container) infoSnapshot() *engineInfo {
	c.pack().mu.Lock()
	defer c.pack().mu.Unlock()
	return c.pack().info
}

// setImmutableID records the backend's immutable container ID. It never
// downgrades an existing value, so a concurrent promotion cannot clear it.
func (c *Container) setImmutableID(uid string) {
	if uid == "" {
		return
	}
	c.pack().uidMu.Lock()
	if c.pack().uid == "" {
		c.pack().uid = uid
	}
	c.pack().uidMu.Unlock()
}

// applyConfigDiagnostics installs the per-handle redactor and freezes the
// target host. Every construction site must use it so a new handle cannot
// silently omit either field.
func (c *Container) applyConfigDiagnostics(cfg *config) {
	c.diagnosticRedactorValue = newDiagnosticMatcher(cfg.diagnosticValues())
	c.targetHost = cfg.eng.defaultHost()
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, the container is
// normally removed before returning. If a post-create environment-file
// cleanup cannot be completed, Run returns the usable handle together
// with the joined error so the container is not orphaned. WithReuse
// switches to get-or-create; see WithReuse for the shared-handle
// lifecycle.
func Run(ctx context.Context, image string, opts ...Option) (result *Container, retErr error) {
	// Validate the public image before applying options. Option functions
	// are user-provided code and may have side effects or return errors;
	// an invalid image must have a deterministic, side-effect-free result.
	if err := validateImageReference(image); err != nil {
		return nil, err
	}
	cfg := newConfig()
	defer func() {
		if retErr != nil {
			retErr = cfg.publicError(retErr)
		}
	}()
	for i, opt := range opts {
		if opt == nil {
			return nil, validationErrorf("Run", i, "option %d is nil", i)
		}
		if err := opt(cfg); err != nil {
			// Capture values from options that ran before a later option
			// failed, while the validation error itself remains value-free.
			cfg.rememberDiagnosticValues()
			return nil, optionBoundaryError(err)
		}
		cfg.rememberDiagnosticValues()
	}
	// Validate the complete wait tree at the public Run boundary. This
	// must happen before image inspection/pull and before a container can
	// be created, so a bad readiness policy cannot be masked by an absent
	// image or leave a partially-created container behind.
	if cfg.waitStrategy != nil {
		if err := wait.ValidateWithPorts(cfg.waitStrategy, runWaitPorts(cfg)); err != nil {
			return nil, err
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.eng == nil {
		backend := os.Getenv(backendEnv)
		if backend == "" && (cfg.dockerHost != nil || cfg.dockerContext != nil || cfg.dockerConfigDir != nil) {
			backend = "docker"
		}
		eng, err := detectEngineFor(runtime.GOOS, backend)
		if err != nil {
			return nil, cfg.publicError(err)
		}
		cfg.eng = eng
	}
	// cfg.validate cannot see this combination before the engine is selected:
	// with no explicit engine it runs while cfg.eng is still nil.
	if cfg.dockerHost != nil || cfg.dockerContext != nil || cfg.dockerConfigDir != nil {
		if _, isDocker := cfg.eng.(dockerEngine); !isDocker {
			return nil, validationErrorf("WithDockerHost", nil, "Docker target options require Docker backend")
		}
	}
	if de, ok := cfg.eng.(dockerEngine); ok {
		if cfg.dockerHost != nil {
			de.host = *cfg.dockerHost
			de.explicitHost = true
			de.explicitTarget = true
		}
		if cfg.dockerContext != nil {
			de.context = *cfg.dockerContext
			de.explicitContext = true
			de.explicitTarget = true
		}
		if cfg.dockerConfigDir != nil {
			de.configDir = *cfg.dockerConfigDir
			de.explicitConfigDir = true
			de.explicitTarget = true
		}
		if cfg.platform != "" {
			de.defaultPlatform = cfg.platform
			de.explicitDefaultPlatform = true
			de.explicitTarget = true
		}
		cfg.eng = de
	}
	applyEngineBinary(cfg)
	applyEngineRunnerEnv(cfg)
	// Reject Windows before the backend is asked anything: Docker context
	// resolution runs the CLI, and chmod's 0600/0700 bits do not provide
	// per-user secrecy there.
	if len(cfg.env) > 0 {
		if err := ensureEnvFileSecurity(); err != nil {
			return nil, err
		}
	}
	if err := cfg.eng.checkConfig(ctx, cfg); err != nil {
		return nil, err
	}
	// Establish the run budget before env-file security preflight. The
	// preflight takes the root advisory lock and may otherwise outlive a
	// canceled caller while another process is holding that lock.
	runCtx, cancel := withDefaultTimeout(ctx, cfg.effectiveRunTimeout())
	defer cancel()

	if len(cfg.env) > 0 {
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
	materializeAppleRunPlatform(cfg)
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
	// of the same image pull once. The resolved reference, rather than
	// the caller's mutable tag, is passed to run.
	resolvedImage, err := cfg.ensureImageRef(runCtx, image)
	if err != nil {
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

	// Register the exact logical name and generation before the backend
	// create starts. If the process is killed while `run` is in flight,
	// the reaper can wait for the pending generation instead of missing
	// the container entirely.
	preRegisterRunWithGlobalReaper(cfg)
	stdout, _, attempted, runErr := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, resolvedImage.reference, envFile)...)
	envCleanupErr := cleanupEnvFileAfterUseContext(runCtx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}
	if runErr != nil {
		if !attempted {
			if target, ok := runReaperTarget(cfg); ok {
				_ = unregisterWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
			}
			return nil, joinEnvFileCleanupError(runErr, envCleanupErr)
		}
		classified := classifyErrorForWithProbe(ctx, cfg.runner, runErr, cfg.eng, cfg.probeTimeout, "run", cfg.name)
		if cleanupErr := cleanupFailedCreate(ctx, cfg, runErr, classified); cleanupErr != nil {
			return nil, joinEnvFileCleanupError(withCleanupError(classified, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
		}
		return nil, joinEnvFileCleanupError(classified, envCleanupErr)
	}

	uid := cfg.eng.parseRunID(stdout)
	if requiresImmutableID(cfg.eng) && !validImmutableID(cfg.eng, uid) {
		identityErr := identityError("Docker run returned no valid immutable container ID")
		if cleanupErr := cleanupFailedCreate(ctx, cfg, identityErr, identityErr); cleanupErr != nil {
			return nil, joinEnvFileCleanupError(withCleanupError(identityErr, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
		}
		return nil, joinEnvFileCleanupError(identityErr, envCleanupErr)
	}

	c := &Container{
		id:              cfg.name,
		runner:          cfg.runner,
		eng:             cfg.eng,
		queryTimeout:    cfg.queryTimeout,
		probeTimeout:    cfg.probeTimeout,
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		creation:        cfg.creation,
		image:           resolvedImage,
		sync:            &lockPack{uid: uid},
		dockerMeta:      cfg.dockerMeta,
	}
	c.applyConfigDiagnostics(cfg)
	// The reaper only backs real CLI containers. Complete the pending
	// name/generation record, then retain Docker's immutable-ID record as
	// an additional deletion target. Both operations are best effort: a
	// reaper problem must not turn a successful backend create into a
	// failed Run call.
	if target, ok := runReaperTarget(cfg); ok {
		if err := verifyCreatedOwnership(ctx, c, cfg); err != nil {
			// An uncertain inspect is not proof that the generation is
			// absent. Leave the pending record in place; cleanup only
			// unregisters it after a confirmed foreign/absent/delete
			// outcome.
			cleanupErr := cleanupFailedCreate(ctx, cfg, err, err)
			if cleanupErr != nil {
				return nil, joinEnvFileCleanupError(withCleanupError(err, &CleanupError{Container: cfg.name, Err: cleanupErr}), envCleanupErr)
			}
			return nil, joinEnvFileCleanupError(err, envCleanupErr)
		}
		// completePending updates the in-memory state before any pipe
		// write. Never register a second active record as a fallback:
		// that could turn a failed state transition into a duplicate
		// destructive target.
		_ = completePreRegistrationWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
		if uid := c.immutableID(); uid != "" {
			_ = registerWithGlobalReaperTarget(target.reaperKey, target.binary, target.subcommand, target.deleteFlags, target.env, uid, "")
		}
	}
	if err := verifyAppleCreatedImage(ctx, c, resolvedImage); err != nil {
		return nil, c.rollback(ctx, joinEnvFileCleanupError(err, envCleanupErr))
	}
	// Start the attached stream before the env-cleanup early return. Log
	// streaming does not read the env file, and a handle returned with a
	// cleanup warning must still honor WithAttachOutput.
	if cfg.attachOutput != nil {
		c.startAttached(cfg.attachOutput, time.Time{})
	}
	// Do not perform file copies or readiness polling while the env file
	// still exists. The handle is usable, and the deferred retry retains
	// ownership for a later safe cleanup attempt.
	if envCleanupErr != nil {
		return c, envCleanupErr
	}

	if err := c.copyFiles(ctx, cfg.files); err != nil {
		return nil, c.rollback(ctx, joinEnvFileCleanupError(err, envCleanupErr))
	}
	if cfg.waitStrategy != nil {
		if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: c}); err != nil {
			cleanupCtx := context.WithoutCancel(ctx)
			waitValues := wait.DiagnosticValues(cfg.waitStrategy)
			tail, tailErr := c.logTailWithError(cleanupCtx, waitValues...)
			err = c.publicError(fmt.Errorf("container %s failed to become ready: %w", c.id, err), waitValues...)
			if tailErr != nil {
				err = c.publicError(fmt.Errorf("%w; container log tail unavailable: %w", err, tailErr), waitValues...)
			} else if tail != "" {
				err = c.publicError(fmt.Errorf("%w; container logs: %s", err, tail), waitValues...)
			}
			return nil, c.rollback(ctx, joinEnvFileCleanupError(err, envCleanupErr))
		}
	}
	return c, envCleanupErr
}

func runWaitPorts(cfg *config) []string {
	ports := make([]string, 0, len(cfg.exposed)+len(cfg.published))
	for _, exposed := range cfg.exposed {
		ports = append(ports, exposed.String())
	}
	for _, published := range cfg.published {
		ports = append(ports, strconv.Itoa(published.containerPort)+"/"+published.proto)
	}
	return ports
}

type runReaperTargetInfo struct {
	binary      string
	subcommand  string
	deleteFlags []string
	reaperKey   string
	env         []string
}

func runReaperTarget(cfg *config) (runReaperTargetInfo, bool) {
	if keepContainers() {
		return runReaperTargetInfo{}, false
	}
	er, ok := cfg.runner.(cli.ExternalRunner)
	if !ok || !er.External() {
		return runReaperTargetInfo{}, false
	}
	binary := er.ExternalBinary()
	if binary == "" {
		if cfg.binary != "" {
			binary = cfg.binary
		} else {
			binary = cfg.eng.binary()
		}
	}
	var env []string
	if eenv, ok := cfg.runner.(interface{ ExternalEnv() []string }); ok {
		env = eenv.ExternalEnv()
	}
	if len(env) == 0 {
		if de, ok := cfg.eng.(dockerEngine); ok {
			env = de.pinnedTargetEnv()
		}
	}
	reaperKey := binary
	if cfg.eng != nil && cfg.eng.targetID() != "" {
		reaperKey = binary + "\x00" + cfg.eng.targetID()
	}
	return runReaperTargetInfo{
		binary:      binary,
		subcommand:  cfg.eng.reaperSubcommand(),
		deleteFlags: append([]string(nil), cfg.eng.reaperDeleteFlags()...),
		reaperKey:   reaperKey,
		env:         env,
	}, true
}

func preRegisterRunWithGlobalReaper(cfg *config) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	_ = preRegisterWithGlobalReaperTarget(target.reaperKey, target.binary, target.subcommand, target.deleteFlags, target.env, cfg.name, cfg.creation)
}

func unregisterReuseFromGlobalReaper(cfg *config) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	_ = unregisterWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
}

// unregisterContainerReaper removes only the records proven to belong to
// a successfully deleted identity. It is intentionally best effort: an
// absent global reaper is already safe, while a failed unregister leaves
// the record available for replay rather than inventing a deletion.
func unregisterContainerReaper(cfg *config, name, creation, uid string) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	if uid != "" && cfg.eng.name() == "docker" {
		_ = unregisterWithGlobalReaperKey(target.reaperKey, uid, "")
	}
	if name != "" {
		_ = unregisterWithGlobalReaperKey(target.reaperKey, name, creation)
	}
}

func protectReuseReaper(cfg *config) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	// markShared updates the in-memory state before attempting the pipe
	// write. Thus even a failed transition fails closed: the record is
	// non-destructive and is replayed as shared on the next child.
	_ = markSharedWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
}

// runCreateLocked serializes an Apple name-addressed create with the
// generation-checked prune/delete paths. attempted is false when the
// lock could not be acquired, so callers must not run failed-create
// cleanup for a command that was never issued.
func runCreateLocked(ctx context.Context, cfg *config, args ...string) (stdout []byte, stderr []byte, attempted bool, err error) {
	if cfg.eng.name() != "apple" {
		stdout, stderr, err = cfg.runner.Run(ctx, args...)
		return stdout, stderr, true, err
	}
	unlock, err := lockName(ctx, cfg.name)
	if err != nil {
		return nil, nil, false, fmt.Errorf("create %s: lock name: %w", cfg.name, err)
	}
	defer unlock()
	stdout, stderr, err = cfg.runner.Run(ctx, args...)
	return stdout, stderr, true, err
}

func waitForReusePoll(ctx context.Context, interval ...time.Duration) error {
	d := reusePollInterval
	if len(interval) > 0 && interval[0] > 0 {
		d = interval[0]
	}
	return ports.RealClock{}.SleepWithContext(ctx, d)
}

// verifyCreatedOwnership validates the labels and backend identity before
// an external reaper is allowed to retain a newly created container. A
// successful `run` command alone is not proof that the library's ownership
// labels were attached.
func verifyCreatedOwnership(ctx context.Context, c *Container, cfg *config) error {
	if c == nil || cfg == nil || !validCreationID(c.creation) {
		return fmt.Errorf("verify created container: invalid creation generation")
	}
	verifyCtx, cancel := withDefaultTimeout(ctx, cfg.effectiveQueryTimeout())
	defer cancel()

	var info *engineInfo
	for {
		var err error
		inspectContainer := c
		if cfg.eng.name() == "docker" {
			uid := c.immutableID()
			if !dockerIDRE.MatchString(uid) {
				return fmt.Errorf("verify created container %s: invalid immutable container ID %q", c.id, uid)
			}
			inspectContainer = &Container{id: uid, runner: c.runner, eng: c.eng, queryTimeout: c.queryTimeout, probeTimeout: c.probeTimeout, sync: &lockPack{uid: uid}}
		}
		unlock := func() {}
		if cfg.eng.name() == "apple" {
			unlock, err = lockName(verifyCtx, c.id)
			if err != nil {
				return fmt.Errorf("verify created container %s: lock name: %w", c.id, err)
			}
		}
		if cfg.eng.name() == "apple" {
			info, err = inspectContainer.inspectFreshLocked(verifyCtx)
		} else {
			info, err = inspectContainer.inspectFresh(verifyCtx)
		}
		if unlock != nil {
			unlock()
		}
		if err == nil {
			break
		}
		if !isNotFoundFor(cfg.eng, err) && !transientReuseInspectError(err) {
			return fmt.Errorf("verify created container %s: %w", c.id, err)
		}
		if err := waitForReusePoll(verifyCtx, cfg.effectiveReusePollInterval()); err != nil {
			return fmt.Errorf("verify created container %s: %w", c.id, err)
		}
	}
	if info == nil || info.labels[managedLabel] != "true" {
		return fmt.Errorf("verify created container %s: managed ownership label is missing", c.id)
	}
	if sess, ok := info.labels[sessionLabel]; !ok || sess != sessionID() {
		return fmt.Errorf("verify created container %s: session ownership label does not match", c.id)
	}
	if info.labels[creationLabel] != c.creation {
		return fmt.Errorf("verify created container %s: creation generation does not match", c.id)
	}
	if cfg.reuse && info.labels[reuseLabel] != "true" {
		return fmt.Errorf("verify created container %s: reuse ownership label is missing", c.id)
	}
	if cfg.reuseGroup != "" && info.labels[reuseGroupLabel] != cfg.reuseGroup {
		return fmt.Errorf("verify created container %s: reuse group label does not match", c.id)
	}
	if cfg.eng.name() == "docker" {
		if info.uid != c.immutableID() {
			return fmt.Errorf("verify created container %s: immutable ID does not match", c.id)
		}
	} else if info.uid != "" {
		return fmt.Errorf("verify created container %s: unexpected immutable ID", c.id)
	}
	c.pack().mu.Lock()
	c.pack().info = info
	c.pack().mu.Unlock()
	c.setImmutableID(info.uid)
	return nil
}

// rollback removes a container Run created but cannot return. A failed removal
// is not hidden: without an immutable ID, Terminate refuses to delete when it
// cannot verify the generation, and the caller must know the container was left
// behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	c.stopAttached()
	if keepContainers() {
		return cause
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if c.reused {
		cfg := &config{
			runner:       c.runner,
			eng:          c.eng,
			name:         c.id,
			reuse:        true,
			creation:     c.creation,
			queryTimeout: c.queryTimeout,
			probeTimeout: c.probeTimeout,
		}
		deleted, err := rollbackReuse(cleanupCtx, c, cfg)
		if err != nil {
			return withCleanupError(cause, &CleanupError{Container: c.id, Err: err})
		}
		if !deleted {
			return withCleanupError(cause, &CleanupError{Container: c.id, Err: fmt.Errorf("stopped generation was not deleted")})
		}
		return cause
	}
	if err := c.Terminate(cleanupCtx); err != nil {
		// %v would flatten the cleanup failure into text, leaving only the
		// original recoverable through errors.Is. Join it instead.
		return withCleanupError(cause, &CleanupError{Container: c.id, Err: err})
	}
	return cause
}

// cleanupFailedCreate removes the container this Run left behind after a
// failed create. It never deletes a pre-existing same-name container:
// name conflicts are skipped, and only a container carrying this process's
// managed+session labels is removed. A cleanup failure is returned so Run
// can preserve it alongside the original operation error.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) error {
	if keepContainers() {
		return nil
	}
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		// A name conflict proves this create did not publish the
		// pre-registered generation. The name may belong to a peer, so
		// remove only our exact name/generation record.
		unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
		return nil
	}
	if !validCreationID(cfg.creation) {
		return identityError("failed create has no valid creation generation")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.effectiveQueryTimeout())
	defer cancel()
	if !requiresImmutableID(cfg.eng) {
		unlock, err := lockName(cleanupCtx, cfg.name)
		if err != nil {
			if cfg.reuse {
				protectReuseReaper(cfg)
			}
			return fmt.Errorf("cleanup container %s: lock name: %w", cfg.name, err)
		}
		defer unlock()
	}
	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = cfg.creation
	info, err := ctr.inspectFreshLocked(cleanupCtx)
	if err != nil {
		if isNotFoundForOperation(cfg.eng, err, "inspect", cfg.name) {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
		if cfg.reuse {
			protectReuseReaper(cfg)
		}
		return fmt.Errorf("cleanup container %s: inspect: %w", cfg.name, err)
	}
	if info.labels[managedLabel] != "true" {
		unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
		return nil
	}
	if sess, ok := info.labels[sessionLabel]; !ok || sess != sessionID() {
		unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
		return nil
	}
	if cfg.reuse {
		if info.labels[reuseLabel] != "true" {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
		if cfg.reuseGroup != "" && info.labels[reuseGroupLabel] != cfg.reuseGroup {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
		actual, ok := info.labels[creationLabel]
		if !ok || !validCreationID(actual) || actual != cfg.creation {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
		// A running or transitional reuse generation may already have been
		// adopted by another caller. Only a freshly stopped/created
		// generation is eligible for automatic failed-create cleanup.
		if info.state != StateStopped && info.state != StateCreated {
			protectReuseReaper(cfg)
			return fmt.Errorf("cleanup container %s: %s reuse generation may already be adopted; refusing automatic deletion", cfg.name, info.state)
		}
	} else {
		actual, ok := info.labels[creationLabel]
		if !ok || !validCreationID(actual) || actual != cfg.creation {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
	}
	target, err := verifiedDeleteTarget(cfg.eng, info, cfg.name)
	if err != nil {
		if cfg.reuse {
			protectReuseReaper(cfg)
		}
		return fmt.Errorf("cleanup container %s: %w", cfg.name, err)
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.effectiveQueryTimeout())
	defer delCancel()
	if err := ctr.delete(delCtx, target); err != nil {
		return fmt.Errorf("cleanup container %s: %w", cfg.name, err)
	}
	unregisterContainerReaper(cfg, cfg.name, cfg.creation, target)
	return nil
}

// ID returns the logical container name. Docker handles also retain the
// immutable backend ID used for internal operations.
func (c *Container) ID() string { return c.id }

// operationTarget returns the immutable target for backend operations.
// Docker handles prefer the ID returned by run/inspect; name-based handles
// (including Apple Container) retain the original name.
func (c *Container) operationTarget() string {
	if c.eng != nil && c.eng.name() == "docker" {
		if uid := c.immutableUID(); uid != "" {
			return uid
		}
	}
	return c.id
}

// immutableUID returns the Docker immutable ID, or "" on name-based backends.
func (c *Container) immutableUID() string {
	if c.eng == nil || c.eng.name() != "docker" {
		return ""
	}
	return c.immutableID()
}

// setImmutableUID promotes a Docker immutable ID. Name-based backends ignore it.
func (c *Container) setImmutableUID(uid string) {
	if c.eng == nil || c.eng.name() != "docker" {
		return
	}
	c.setImmutableID(uid)
}

func (c *Container) dockerGenerationMatches(info *engineInfo) bool {
	return c.eng != nil && c.eng.name() == "docker" && c.creation != "" &&
		info != nil && info.labels[creationLabel] == c.creation
}

func (c *Container) classify(ctx context.Context, err error) error {
	return classifyErrorWithProbe(ctx, c.runner, err, c.eng, c.probeTimeout)
}

func (c *Container) classifyOperation(ctx context.Context, err error, operation string) error {
	if c.eng == nil {
		return c.classify(ctx, err)
	}
	target := c.operationTarget()
	if target == c.id {
		return classifyErrorForWithProbe(ctx, c.runner, err, c.eng, c.probeTimeout, operation, target)
	}
	return classifyErrorForWithProbe(ctx, c.runner, err, c.eng, c.probeTimeout, operation, target, c.id)
}

func (c *Container) deleteOperation() string {
	if c.eng != nil && c.eng.name() == "docker" {
		return "rm"
	}
	return "delete"
}

// State returns the current lifecycle state.
//
// Unlike Host/Endpoint/MappedPort/ContainerIP, State always performs a
// fresh inspect so wait fail-fast-on-stop and other lifecycle checks are
// not delayed by the short dynamic-inspect TTL.
func (c *Container) State(ctx context.Context) (State, error) {
	info, err := c.inspectDynamicFresh(ctx)
	if err != nil {
		return StateUnknown, err
	}
	return info.state, nil
}

// Stop stops the container. A nil timeout uses the CLI's default grace
// period before the process is killed. A non-nil timeout must be non-negative
// and its rounded-up seconds must fit the backend's supported range. The
// backend-specific limit is checked before invoking its CLI. Because both
// backends accept whole seconds, positive sub-second timeouts are rounded up;
// zero requests immediate termination.
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error {
	if timeout != nil && *timeout < 0 {
		return newValidationErrorWithField("Stop", "timeout", *timeout,
			fmt.Errorf("stop timeout must be >= 0, got %s", *timeout))
	}
	if c.refusesSharedOps() {
		return fmt.Errorf("stop %s: %w", c.id, ErrSharedContainer)
	}
	stopCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout()+durationOrZero(timeout))
	defer cancel()
	target, err := c.verifiedOperationTarget(stopCtx)
	if err != nil {
		return err
	}
	args, err := c.eng.stopArgs(target, timeout)
	if err != nil {
		return fmt.Errorf("stop %s: %w", c.id, err)
	}
	_, _, err = c.runner.Run(stopCtx, args...)
	if err != nil {
		return c.classifyOperation(ctx, err, "stop")
	}
	c.stopAttached()
	c.clearDynamic()
	return nil
}

// Image returns the image reference the container was created with.
func (c *Container) Image() string {
	return c.image.reference
}

// InspectInfo represents a point-in-time snapshot of container state and metadata.
type InspectInfo struct {
	id         string
	name       string
	uid        string
	image      string
	imageID    string
	state      State
	exitCode   int
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time
	labels     map[string]string
	ip         string
	platform   string
	ports      []PortBinding
}

// ID returns the logical container name, which is the handle's address and
// not necessarily the backend's immutable identity. ContainerID reports that
// identity separately when the backend exposes one.
func (i *InspectInfo) ID() string { return i.id }

// Name returns the backend-reported container name without Docker's leading
// slash. Apple Container addresses containers by name, so Name matches ID.
func (i *InspectInfo) Name() string { return i.name }

// ContainerID returns the backend's immutable container ID, such as Docker's
// 64-character hexadecimal ID. It is empty when the backend has no identity
// distinct from the logical name, including Apple Container. ID keeps
// returning the logical name so existing callers are unaffected.
func (i *InspectInfo) ContainerID() string { return i.uid }

func (i *InspectInfo) Image() string   { return i.image }
func (i *InspectInfo) ImageID() string { return i.imageID }
func (i *InspectInfo) State() State    { return i.state }
func (i *InspectInfo) ExitCode() int   { return i.exitCode }

// CreatedAt returns the backend-reported creation time. The zero time is the
// documented unknown marker: the backend omitted the field or reported a form
// this library cannot parse, and callers must not read it as a real instant.
// StartedAt and FinishedAt use the same marker.
func (i *InspectInfo) CreatedAt() time.Time  { return i.createdAt }
func (i *InspectInfo) StartedAt() time.Time  { return i.startedAt }
func (i *InspectInfo) FinishedAt() time.Time { return i.finishedAt }
func (i *InspectInfo) IP() string            { return i.ip }
func (i *InspectInfo) Platform() string      { return i.platform }

func (i *InspectInfo) Labels() map[string]string {
	if i.labels == nil {
		return nil
	}
	clone := make(map[string]string, len(i.labels))
	for k, v := range i.labels {
		clone[k] = v
	}
	return clone
}

func (i *InspectInfo) Ports() []PortBinding {
	if i.ports == nil {
		return nil
	}
	clone := make([]PortBinding, len(i.ports))
	copy(clone, i.ports)
	return clone
}

// Inspect returns a fresh snapshot of the container's metadata and runtime state.
func (c *Container) Inspect(ctx context.Context) (*InspectInfo, error) {
	info, err := c.inspectDynamicFresh(ctx)
	if err != nil {
		return nil, err
	}
	ports := make([]PortBinding, len(info.bound))
	for idx, b := range info.bound {
		ports[idx] = PortBinding{
			ContainerPort: b.containerPort,
			Proto:         b.proto,
			HostAddr:      b.hostAddr,
			HostPort:      b.hostPort,
		}
	}
	return &InspectInfo{
		id:         c.id,
		name:       info.name,
		uid:        info.uid,
		image:      info.image,
		imageID:    info.imageID,
		state:      info.state,
		exitCode:   info.exitCode,
		createdAt:  info.createdAt,
		startedAt:  info.startedAt,
		finishedAt: info.finishedAt,
		labels:     info.labels,
		ip:         info.ip,
		platform:   info.platform,
		ports:      ports,
	}, nil
}

// ExitCode returns the exit code from a fresh container inspect.
func (c *Container) ExitCode(ctx context.Context) (int, error) {
	info, err := c.inspectDynamicFresh(ctx)
	if err != nil {
		return 0, err
	}
	return info.exitCode, nil
}

const waitExitPollInterval = 100 * time.Millisecond

// Wait blocks until the container reaches StateStopped and returns its exit code.
// If the container is already stopped, it returns its exit code immediately.
func (c *Container) Wait(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
		}
		info, err := c.inspectDynamicFresh(ctx)
		if err != nil {
			// A daemon that is briefly unreachable or busy must not fail a
			// wait that is still within its context; only a terminal inspect
			// error ends the wait.
			if !transientReuseInspectError(err) {
				return 0, err
			}
		} else if info.state == StateStopped {
			return info.exitCode, nil
		}
		timer.Reset(waitExitPollInterval)
	}
}

// Start starts a stopped container. A configured WithAttachOutput stream that
// ended with the previous process is re-attached once the container is
// running; a stream that is still active is left alone so output is not
// duplicated.
func (c *Container) Start(ctx context.Context) error {
	return c.start(ctx, true)
}

// start is Start with control over attached-stream reconnection. The restart
// fallback replaces the stream itself after its Stop, so it disables the
// reconnection here to avoid starting two streams.
func (c *Container) start(ctx context.Context, reattach bool) error {
	if c.refusesSharedOps() {
		return fmt.Errorf("start %s: %w", c.id, ErrSharedContainer)
	}
	startCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(startCtx)
	if err != nil {
		return err
	}
	args := c.eng.startArgs(target)
	var output io.Writer
	var resumeSince time.Time
	if reattach {
		output = c.attachedOutput()
		if output != nil {
			// Take the resume point before the command so Docker's --since
			// also covers output the replacement process emits while it
			// starts.
			resumeSince = time.Now()
		}
	}
	_, _, err = c.runner.Run(startCtx, args...)
	if err != nil {
		return wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "start"), "start", target, c.id)
	}
	c.clearDynamic()
	if output != nil && !c.attachStreamActive() {
		c.startAttached(output, resumeSince)
	}
	return nil
}

// Restart restarts the container. On backends without a native restart command
// (such as Apple Container), it gracefully stops then starts the container. In
// both forms a configured WithAttachOutput stream is replaced once the new
// process is running; the writer is not given the pre-operation history again.
func (c *Container) Restart(ctx context.Context, timeout *time.Duration) error {
	if timeout != nil && *timeout < 0 {
		return newValidationErrorWithField("Restart", "timeout", *timeout,
			fmt.Errorf("restart timeout must be >= 0, got %s", *timeout))
	}
	if c.refusesSharedOps() {
		return fmt.Errorf("restart %s: %w", c.id, ErrSharedContainer)
	}
	restartCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout()+durationOrZero(timeout))
	defer cancel()
	target, err := c.verifiedOperationTarget(restartCtx)
	if err != nil {
		return err
	}
	args, err := c.eng.restartArgs(target, timeout)
	if errors.Is(err, ErrUnsupportedCapability) {
		return c.restartAttached(ctx, timeout)
	}
	if err != nil {
		return fmt.Errorf("restart %s: %w", c.id, err)
	}
	// Docker keeps the full log across a restart. Detach first and resume
	// from this instant so the replacement stream captures the new process
	// without replaying lines the old stream already delivered.
	output := c.attachedOutput()
	var resumeSince time.Time
	if output != nil {
		c.stopAttached()
		resumeSince = time.Now()
	}
	_, _, err = c.runner.Run(restartCtx, args...)
	if output != nil {
		c.startAttached(output, resumeSince)
	}
	if err != nil {
		return wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "restart"), "restart", target, c.id)
	}
	c.clearDynamic()
	return nil
}

// restartAttached emulates Restart on a backend without a native restart
// command. Stop ends the attached log stream with the stopped process, so the
// same writer is attached again once the replacement process has started.
func (c *Container) restartAttached(ctx context.Context, timeout *time.Duration) error {
	if err := c.Stop(ctx, timeout); err != nil {
		return err
	}
	output := c.attachedOutput()
	resumeSince := time.Now()
	if err := c.start(ctx, false); err != nil {
		return err
	}
	// Always attempt the replacement even when the previous stream is still
	// winding down: its gate then reports a wedged sink instead of silently
	// leaving the writer unattached.
	if output != nil {
		c.startAttached(output, resumeSince)
	}
	return nil
}

// Kill terminates or sends a signal to the container's main process.
// If no signal is provided, it defaults to SIGKILL. Only a signal that is
// guaranteed to end the main process also ends the WithAttachOutput stream; a
// stream left attached by a catchable signal ends when the process exits.
func (c *Container) Kill(ctx context.Context, signal ...string) error {
	if len(signal) > 1 {
		return newValidationErrorWithField("Kill", "signal", signal,
			fmt.Errorf("at most one signal may be provided"))
	}
	var sig string
	if len(signal) == 1 {
		if signal[0] == "" {
			return newValidationErrorWithField("Kill", "signal", signal[0],
				fmt.Errorf("signal cannot be empty"))
		}
		sig = signal[0]
	}
	if c.refusesSharedOps() {
		return fmt.Errorf("kill %s: %w", c.id, ErrSharedContainer)
	}
	killCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(killCtx)
	if err != nil {
		return err
	}
	args := c.eng.killArgs(target, sig)
	_, _, err = c.runner.Run(killCtx, args...)
	if err != nil {
		return wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "kill"), "kill", target, c.id)
	}
	if terminatingKillSignal(sig) {
		c.stopAttached()
	}
	c.clearDynamic()
	return nil
}

// terminatingKillSignal reports whether Kill is guaranteed to end the
// container's main process. SIGKILL cannot be caught or ignored, so an empty
// signal (the documented SIGKILL default) and its spellings are terminating;
// any other signal may be handled, and its stream then ends when the process
// actually exits.
func terminatingKillSignal(sig string) bool {
	switch strings.ToUpper(sig) {
	case "", "KILL", "SIGKILL", "9":
		return true
	default:
		return false
	}
}

// Pause pauses the processes in the container. Returns ErrUnsupportedCapability
// on backends without pause capability (Apple Container).
func (c *Container) Pause(ctx context.Context) error {
	if c.refusesSharedOps() {
		return fmt.Errorf("pause %s: %w", c.id, ErrSharedContainer)
	}
	pCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(pCtx)
	if err != nil {
		return err
	}
	args, err := c.eng.pauseArgs(target)
	if err != nil {
		return err
	}
	_, _, err = c.runner.Run(pCtx, args...)
	if err != nil {
		return wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "pause"), "pause", target, c.id)
	}
	c.clearDynamic()
	return nil
}

// Unpause unpauses the processes in the container. Returns ErrUnsupportedCapability
// on backends without pause capability (Apple Container).
func (c *Container) Unpause(ctx context.Context) error {
	if c.refusesSharedOps() {
		return fmt.Errorf("unpause %s: %w", c.id, ErrSharedContainer)
	}
	pCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(pCtx)
	if err != nil {
		return err
	}
	args, err := c.eng.unpauseArgs(target)
	if err != nil {
		return err
	}
	_, _, err = c.runner.Run(pCtx, args...)
	if err != nil {
		return wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "unpause"), "unpause", target, c.id)
	}
	c.clearDynamic()
	return nil
}

// Shared returns a handle to the same container with Stop/Copy refusals
// lifted. The returned handle keeps reused set so Cleanup and
// TerminateContainer still skip teardown; only allowDestructive differs.
// Mutable identity state is shared through sync so both handles see the
// same promoted UID and inspect cache.
func (c *Container) Shared() *Container {
	if c == nil {
		return nil
	}
	_ = c.pack()
	c.secretsMu.RLock()
	diagnosticRedactor := c.diagnosticRedactorValue
	c.secretsMu.RUnlock()
	return &Container{
		id: c.id, runner: c.runner, eng: c.eng,
		queryTimeout: c.queryTimeout, probeTimeout: c.probeTimeout,
		exposed: c.exposed, published: c.published,
		network: c.network, networkExplicit: c.networkExplicit,
		reused: c.reused, allowDestructive: true, creation: c.creation,
		image: c.image, nameInspect: c.nameInspect,
		sync: c.sync, dockerMeta: c.dockerMeta,
		diagnosticRedactorValue: diagnosticRedactor,
		targetHost:              c.targetHost,
	}
}

// StopAttached terminates any active background log streaming started by WithAttachOutput.
func (c *Container) StopAttached() {
	c.stopAttached()
}

// attachStopWait bounds how long a cancelled attach stream may take to finish.
// Cancellation stops a stream at its next read, so only a sink wedged inside a
// write can exceed it.
const attachStopWait = 500 * time.Millisecond

func (c *Container) stopAttached() {
	p := c.pack()
	p.attachMu.Lock()
	cancel := p.attachCancel
	done := p.attachDone
	p.attachCancel = nil
	p.attachMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(attachStopWait):
		}
	}
}

// startAttached launches the background stream behind WithAttachOutput. A
// zero resumeSince starts the initial stream and replays the backend's
// available history. A non-zero value starts a replacement stream from that
// instant, so a restart does not hand the writer the same history twice.
// The stream outlives the Run context by design; the goroutine owns its
// derived context so it is cancelled when the stream ends on its own, and it
// closes done so AttachedDone observes completion without reading shared
// state.
func (c *Container) startAttached(output io.Writer, resumeSince time.Time) {
	attachCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p := c.pack()
	p.attachMu.Lock()
	if p.attachGate == nil {
		p.attachGate = make(chan struct{}, 1)
	}
	gate := p.attachGate
	p.attachOutput = output
	// Keep the previous stream's terminal failure: AttachedError promises a
	// caller that observed AttachedDone closed that the failure is still
	// readable after Start or Restart reattaches. Later failures are joined.
	p.attachCancel = cancel
	p.attachDone = done
	p.attachMu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		// The gate keeps a replacement stream from writing to the same
		// caller-supplied io.Writer while its predecessor is still inside a
		// write. A wedged sink never releases it, so give up after the same
		// bound StopAttached uses and report the failure instead of blocking
		// forever.
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			err := c.followLogsTo(attachCtx, output, resumeSince)
			if attachCtx.Err() == nil {
				c.recordAttachError(err)
			}
		case <-time.After(attachStopWait):
			c.recordAttachError(errors.New("logs: previous attached stream is still writing to the output; not starting a replacement"))
		}
	}()
}

// recordAttachError accumulates a terminal stream failure for AttachedError.
// A later failure is joined with the earlier ones so a caller holding an older
// AttachedDone keeps matching its stream's failure; a failure that already
// matches the accumulated value is ignored instead of growing the join.
func (c *Container) recordAttachError(err error) {
	if err == nil {
		return
	}
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	if p.attachErr == nil {
		p.attachErr = err
		return
	}
	if errors.Is(p.attachErr, err) {
		return
	}
	p.attachErr = errors.Join(p.attachErr, err)
}

// attachedOutput returns the destination of the background stream, or nil when
// WithAttachOutput was not used.
func (c *Container) attachedOutput() io.Writer {
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	return p.attachOutput
}

// attachStreamActive reports whether the current attached stream is still
// running. Start uses it to avoid attaching a second follower to a writer that
// still has one.
func (c *Container) attachStreamActive() bool {
	p := c.pack()
	p.attachMu.Lock()
	cancel := p.attachCancel
	done := p.attachDone
	p.attachMu.Unlock()
	// stopAttached clears cancel before it waits. A non-nil done without a
	// cancel belongs to that stopped stream, not to a live one; Start must
	// replace it or the writer stays detached forever.
	if cancel == nil || done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// AttachedDone returns a channel that is closed when the background attached
// log stream finishes. If WithAttachOutput was not used, it returns nil.
func (c *Container) AttachedDone() <-chan struct{} {
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	return p.attachDone
}

// AttachedError returns the terminal failures accumulated across the attach
// streams this handle has followed, or nil if no stream has failed. A stream
// cancelled by StopAttached or by container teardown does not record a
// failure. Reattaching with Start or Restart does not clear earlier failures,
// so a caller that retained a previous stream's AttachedDone channel still
// observes its failure after the channel closes. Failures are accumulated with
// errors.Join, which keeps the latest stream's failure included, so errors.Is
// matches every failure the streams recorded.
func (c *Container) AttachedError() error {
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	return p.attachErr
}

// Terminate force-removes the container. A Docker handle must carry a
// validated immutable UID; a name-only handle is never an acceptable
// fallback. Apple Container has no immutable UID, so its name path is
// allowed only with a valid generation and a fresh locked inspect.
func (c *Container) Terminate(ctx context.Context) error {
	c.stopAttached()
	if requiresImmutableID(c.eng) {
		target, err := c.verifiedOperationTarget(ctx)
		if err != nil {
			return fmt.Errorf("terminate %s: %w", c.id, err)
		}
		return c.delete(ctx, target)
	}
	if c.eng.name() == "apple" {
		uid, creation, err := c.identitySnapshot(ctx)
		if err != nil {
			return err
		}
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
		if contextErr := commandContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		if isNotFound(err) {
			unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, "")
			return nil
		}
		if err != nil {
			if errors.Is(err, ErrGenerationReplaced) {
				unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, "")
			}
			return fmt.Errorf("terminate %s: verify generation: %w", c.id, err)
		}
		if err := sameContainerIdentity(c.eng, &engineInfo{labels: map[string]string{creationLabel: creation}}, info); err != nil {
			if errors.Is(err, ErrGenerationReplaced) {
				unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, "")
			}
			return err
		}
		return c.delete(ctx, c.id)
	}
	target, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return err
	}
	return c.delete(ctx, target)
}

func (c *Container) delete(ctx context.Context, target string) error {
	c.stopAttached()
	delCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	_, _, err := c.runner.Run(delCtx, c.eng.deleteArgs(target)...)
	if contextErr := commandContextError(delCtx, err); contextErr != nil {
		return contextErr
	}
	if err == nil || isNotFoundForOperation(c.eng, err, c.deleteOperation(), target, c.id) {
		unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, target)
		c.clearDynamic()
		c.clearDiagnosticSecrets()
		return nil
	}
	return c.classifyOperation(ctx, err, c.deleteOperation())
}

// ContainerIP returns the container's address on its first attached
// network. With the Docker backend on Docker Desktop this address is
// usually not reachable from the host; prefer Endpoint.
func (c *Container) ContainerIP(ctx context.Context) (string, error) {
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", c.redactError(err)
	}
	if info.ip == "" {
		c.clearDynamic()
		return "", c.redactError(errors.New("container has no reported IP address"), c.id, info.ip)
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
			c.clearDynamic()
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
		return c.TargetHost(), nil
	}
	if len(c.published) > 0 {
		p := c.published[0]
		b, ok := matchingPublishedBinding(info.bound, p, c.eng)
		if !ok {
			c.clearDynamic()
			return "", fmt.Errorf("%w: published port %q has no host binding in Docker network mode %q", ErrPortNotExposed, p.raw, info.networkMode)
		}
		host, err := dockerBindingConnectHost(b, c.eng)
		if err != nil {
			return "", err
		}
		return host, nil
	}
	return c.TargetHost(), nil
}

// MappedPort resolves a declared container port ("6379/tcp" or "6379")
// to the port clients should dial.
func (c *Container) MappedPort(ctx context.Context, port string) (int, error) {
	_, p, err := c.resolve(ctx, port, "MappedPort")
	return p, err
}

// Endpoint returns "host:port" for a port declared via WithExposedPorts
// or WithPublishedPort. It does not infer a host-network service port.
func (c *Container) Endpoint(ctx context.Context, port string) (string, error) {
	host, p, err := c.resolve(ctx, port, "Endpoint")
	if err != nil {
		return "", c.redactError(err, port)
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// resolve maps a container port to the (host, port) pair to dial.
func (c *Container) resolve(ctx context.Context, port, operation string) (string, int, error) {
	spec, err := parsePortSpec(port)
	if err != nil {
		return "", 0, newValidationErrorWithField(operation, "port", port, err)
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
	// Resolve through an identity-checked dynamic inspect (short TTL)
	// before returning an endpoint. This is especially important for
	// direct-IP backends: a published address is otherwise just a stale
	// option value and can be returned for a replacement container
	// without any backend read.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", 0, err
	}
	if c.eng.directIP() {
		if published != nil {
			return published.connectAddr(), published.hostPort, nil
		}
		if info.ip == "" {
			c.clearDynamic()
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
		b, ok := matchingPublishedBinding(info.bound, *published, c.eng)
		if !ok {
			c.clearDynamic()
			return "", 0, fmt.Errorf("%w: published port %q has no host binding in Docker network mode %q", ErrPortNotExposed, published.raw, info.networkMode)
		}
		host, err := dockerBindingConnectHost(b, c.eng)
		if err != nil {
			return "", 0, c.redactError(err, port)
		}
		return host, b.hostPort, nil
	}
	if b, ok := matchingExposedBinding(info.bound, spec, c.eng); ok {
		host, err := dockerBindingConnectHost(b, c.eng)
		if err != nil {
			return "", 0, err
		}
		return host, b.hostPort, nil
	}
	c.clearDynamic()
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.nameInspect {
		return c.inspectFresh(ctx)
	}
	c.pack().mu.Lock()
	cached := c.pack().info
	c.pack().mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.rememberIdentity(ctx, info); err != nil {
		return nil, err
	}
	c.pack().mu.Lock()
	cached = c.pack().info
	c.pack().mu.Unlock()
	return cached, nil
}

// inspectDynamic returns lifecycle, network, and port data, reusing a
// short-TTL snapshot on lockPack when the data is still usable. Callers
// that must observe stop/replace immediately (State, post-reuse checks)
// use inspectDynamicFresh instead.
func (c *Container) inspectDynamic(ctx context.Context) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.nameInspect {
		if info := c.cachedDynamic(); info != nil {
			return info, nil
		}
		if err := c.pack().dynamicMu.Lock(ctx); err != nil {
			return nil, err
		}
		defer c.pack().dynamicMu.Unlock()
		// A concurrent refresh may have populated the snapshot while we
		// waited; reuse it instead of launching another inspect.
		if info := c.cachedDynamic(); info != nil {
			return info, nil
		}
	}
	return c.refreshDynamic(ctx)
}

// inspectDynamicFresh always performs an identity-checked inspect and
// refreshes the short-TTL dynamic snapshot shared with Shared() handles.
func (c *Container) inspectDynamicFresh(ctx context.Context) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.nameInspect {
		if err := c.pack().dynamicMu.Lock(ctx); err != nil {
			return nil, err
		}
		defer c.pack().dynamicMu.Unlock()
	}
	return c.refreshDynamic(ctx)
}

func (c *Container) cachedDynamic() *engineInfo {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !usableDynamicSnapshot(p.dynamic) {
		return nil
	}
	if !p.currentTime().Before(p.dynamicAt.Add(dynamicInspectTTL)) {
		return nil
	}
	info := cloneEngineInfo(p.dynamic)
	if p.defaultNetwork != "" {
		info.defaultNetwork = p.defaultNetwork
	}
	return info
}

func (c *Container) refreshDynamic(ctx context.Context) (*engineInfo, error) {
	gen := c.dynamicGeneration()
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.applyCachedDefaultNetwork(ctx, info); err != nil {
		return nil, err
	}
	if c.nameInspect {
		return info, nil
	}
	if err := c.rememberIdentity(ctx, info); err != nil {
		return nil, err
	}
	c.storeDynamic(info, gen)
	return info, nil
}

func (c *Container) dynamicGeneration() uint64 {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dynamicGen
}

func (c *Container) storeDynamic(info *engineInfo, gen uint64) {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Delete/Terminate bump dynamicGen while an inspect may still be in
	// flight. Drop that result so endpoint reads cannot revive a deleted
	// container's IP/port under a fresh TTL.
	if p.dynamicGen != gen {
		return
	}
	p.dynamic = cloneEngineInfo(info)
	p.dynamicAt = p.currentTime()
}

func (c *Container) clearDynamic() {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dynamic = nil
	p.dynamicAt = time.Time{}
	p.dynamicGen++
}

// usableDynamicSnapshot reports whether a cached inspect has network or
// port payload worth returning. Callers that still cannot resolve (empty
// IP, missing host binding) clear the snapshot so the next read re-inspects
// immediately instead of waiting out the TTL.
func usableDynamicSnapshot(info *engineInfo) bool {
	if info == nil {
		return false
	}
	if info.ip != "" {
		return true
	}
	for _, b := range info.bound {
		if b.hostPort > 0 {
			return true
		}
	}
	return false
}

func cloneEngineInfo(info *engineInfo) *engineInfo {
	if info == nil {
		return nil
	}
	out := *info
	if info.labels != nil {
		out.labels = make(map[string]string, len(info.labels))
		for key, value := range info.labels {
			out.labels[key] = value
		}
	}
	if info.networkNames != nil {
		out.networkNames = append([]string(nil), info.networkNames...)
	}
	if info.networkAliases != nil {
		out.networkAliases = append([]string(nil), info.networkAliases...)
	}
	if info.bound != nil {
		out.bound = append([]boundPort(nil), info.bound...)
	}
	if info.runOptions.capAdd != nil {
		out.runOptions.capAdd = append([]string(nil), info.runOptions.capAdd...)
	}
	if info.runOptions.capDrop != nil {
		out.runOptions.capDrop = append([]string(nil), info.runOptions.capDrop...)
	}
	if info.runOptions.ulimits != nil {
		out.runOptions.ulimits = append([]ulimitSpec(nil), info.runOptions.ulimits...)
	}
	if info.runOptions.extraHosts != nil {
		out.runOptions.extraHosts = append([]extraHostSpec(nil), info.runOptions.extraHosts...)
	}
	return &out
}

func (c *Container) rememberIdentity(ctx context.Context, info *engineInfo) error {
	identity := immutableInfo(info)
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return err
	}
	defer c.pack().inspectMu.Unlock()
	c.pack().mu.Lock()
	defer c.pack().mu.Unlock()
	if c.pack().info != nil && !sameImmutableInfo(c.pack().info, identity) {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	uid := c.immutableID()
	if uid != "" && uid != identity.uid {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if uid == "" && c.pack().info != nil && c.pack().info.uid != "" && c.pack().info.uid != identity.uid {
		return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
	}
	if c.creation != "" {
		actual := identity.labels[creationLabel]
		if actual != "" && actual != c.creation {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
		// Once a name-only handle has observed its generation, losing
		// the label is also evidence of a replacement.
		if actual == "" && uid == "" && c.pack().info != nil && c.pack().info.labels[creationLabel] != "" {
			return fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
		}
	}
	if c.pack().info == nil {
		c.pack().info = identity
	} else {
		c.pack().info = mergeImmutableInfo(c.pack().info, identity)
	}
	if c.image.reference == "" {
		c.image = imageFromInfo(info)
	}
	return nil
}

func (c *Container) cacheInfo(info *engineInfo) error {
	return c.rememberIdentity(context.Background(), info)
}

func immutableInfo(info *engineInfo) *engineInfo {
	if info == nil {
		return nil
	}
	labels := make(map[string]string, len(info.labels))
	for key, value := range info.labels {
		labels[key] = value
	}
	return &engineInfo{
		uid:                info.uid,
		image:              info.image,
		imageID:            info.imageID,
		imageDigest:        info.imageDigest,
		imageVariantDigest: info.imageVariantDigest,
		platform:           info.platform,
		platformMeta:       info.platformMeta,
		labels:             labels,
	}
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
	if a.imageID != "" && b.imageID != "" && a.imageID != b.imageID {
		return false
	}
	if a.imageDigest != "" && b.imageDigest != "" && !strings.EqualFold(a.imageDigest, b.imageDigest) {
		return false
	}
	if a.imageVariantDigest != "" && b.imageVariantDigest != "" && !strings.EqualFold(a.imageVariantDigest, b.imageVariantDigest) {
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
	if merged.imageID == "" {
		merged.imageID = current.imageID
	}
	if merged.imageDigest == "" {
		merged.imageDigest = current.imageDigest
	}
	if merged.imageVariantDigest == "" {
		merged.imageVariantDigest = current.imageVariantDigest
	}
	if merged.platform == "" {
		merged.platform = current.platform
	}
	if !merged.platformMeta.valid && current.platformMeta.valid {
		merged.platformMeta = current.platformMeta
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
	if uid := c.immutableID(); uid != "" {
		return uid
	}
	return c.id
}

// identitySnapshot reads the handle's backend identity and creation
// generation while holding the same lock that protects their publication.
func (c *Container) identitySnapshot(ctx context.Context) (uid, creation string, err error) {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return "", "", err
	}
	defer c.pack().inspectMu.Unlock()
	return c.immutableID(), c.creation, nil
}

// verifiedOperationTarget is the safe target for lifecycle, copy, exec,
// and log operations. In particular, a Docker handle with an empty or
// malformed UID fails before a CLI call instead of falling back to its
// name. Apple handles likewise require a valid generation before using a
// name-addressed operation. Lock acquisition is context-aware so a copy
// cannot wait behind another call's inspect or default-network lookup after
// its caller has been canceled.
func (c *Container) verifiedOperationTarget(ctx context.Context) (string, error) {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return "", err
	}
	defer c.pack().inspectMu.Unlock()
	uid := c.immutableID()
	creation := c.creation
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
	return c.inspectTargetLocked(), nil
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
	return c.ensureDefaultNetworkForConfig(ctx, nil)
}

// ensureDefaultNetworkForConfig resolves the daemon default for a caller that
// does not own the handle. The answer is stored on the handle, so concurrent
// reuse attaches sharing one ensure result perform one lookup; a failed
// lookup is not stored and the next caller retries. cfg supplies the caller's
// runner and optional Client cache for the miss path.
func (c *Container) ensureDefaultNetworkForConfig(ctx context.Context, cfg *config) (string, error) {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return "", err
	}
	defer c.pack().inspectMu.Unlock()
	if name := c.cachedDefaultNetwork(); name != "" {
		// The handle value is valid only for the handle's own target and
		// runner. A reuse waiter may carry a different runner (for example a
		// custom cli.Runner), and accepting the leader's answer would validate
		// its container against the wrong daemon.
		if cfg == nil || dockerDefaultNetworkKey(cfg.eng, cfg.runner) == dockerDefaultNetworkKey(c.eng, c.runner) {
			return name, nil
		}
	}
	runner, meta := c.runner, c.dockerMeta
	eng := c.eng
	timeouts := opTimeouts{query: c.effectiveQueryTimeout(), probe: c.effectiveProbeTimeout()}
	if cfg != nil {
		runner, meta, eng = cfg.runner, cfg.dockerMeta, cfg.eng
		timeouts = opTimeouts{query: cfg.effectiveQueryTimeout(), probe: cfg.effectiveProbeTimeout()}
	}
	name, err := dockerDefaultNetworkCached(ctx, meta, runner, eng, timeouts)
	if err != nil {
		return "", err
	}
	if cfg != nil && dockerDefaultNetworkKey(cfg.eng, cfg.runner) != dockerDefaultNetworkKey(c.eng, c.runner) {
		// Do not overwrite the handle's own answer with another runner's.
		return name, nil
	}
	return c.storeDefaultNetwork(name), nil
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

// attachDefaultNetworkForConfig resolves the daemon default for a reuse
// attach. Concurrent callers on one Client share the Client's cache, which is
// keyed by the resolved target, so they perform one lookup per target and a
// failed lookup is not retained.
func attachDefaultNetworkForConfig(ctx context.Context, cfg *config, info *engineInfo) error {
	if !configNeedsDefaultNetwork(cfg) {
		return nil
	}
	name, err := dockerDefaultNetworkCached(ctx, cfg.dockerMeta, cfg.runner, cfg.eng, opTimeouts{query: cfg.effectiveQueryTimeout(), probe: cfg.effectiveProbeTimeout()})
	if err != nil {
		return err
	}
	info.defaultNetwork = name
	return nil
}

func (c *Container) applyCachedDefaultNetwork(ctx context.Context, info *engineInfo) error {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return err
	}
	defer c.pack().inspectMu.Unlock()
	if name := c.cachedDefaultNetwork(); name != "" {
		info.defaultNetwork = name
	}
	return nil
}

// cachedDefaultNetwork and storeDefaultNetwork synchronize the lazily learned
// Docker daemon default with pack.mu. Endpoint's short-TTL cache reads the
// same field under that lock; writing it under inspectMu alone races.
func (c *Container) cachedDefaultNetwork() string {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.defaultNetwork
}

func (c *Container) storeDefaultNetwork(name string) string {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.defaultNetwork == "" && name != "" {
		p.defaultNetwork = name
	}
	return p.defaultNetwork
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer c.pack().inspectMu.Unlock()
	return c.inspectFreshLocked(ctx)
}

func (c *Container) inspectFreshLocked(ctx context.Context) (*engineInfo, error) {
	uid := c.immutableID()
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

	qCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target := c.inspectTargetLocked()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "inspect"), "inspect", target, c.id)
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		if errors.Is(err, errInspectTargetNotFound) {
			return nil, wrapInspectTargetNotFound(err)
		}
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
	// its caller as data only. It is never published into the handle UID here; the
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
