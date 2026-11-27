package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/wait"
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
	// attachStreamDone closes when the follow goroutine exits, even if the
	// public AttachedDone is held open until a teardown outcome resolves the
	// stream's terminal error.
	attachStreamDone chan struct{}
	// attachOutput is the WithAttachOutput destination of the current
	// stream. It is retained so a restart can re-attach the same writer.
	attachOutput io.Writer
	// attachErr accumulates the terminal failures across the attach streams
	// of this handle, kept for AttachedError. Reattaching does not clear it so
	// a caller that retained the previous stream's AttachedDone can still
	// observe why that stream ended; later failures are joined to it.
	attachErr error
	// attachTeardowns tracks the in-flight Stop and terminating Kill teardowns
	// of this handle, keyed by the stream's internal completion channel.
	// Commands run concurrently; entries let an overlapping command for the
	// same stream share its outcome while a command for a replacement stream
	// keeps its own. A stream without an entry is not being torn down, so its
	// failure is never suppressed by another stream's teardown.
	attachTeardowns map[chan struct{}]*attachTeardownState
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
	// process exit. Stop/CopyToContainer/CopyContentToContainer/
	// CopyFileFromContainer also refuse with ErrSharedContainer unless
	// allowDestructive is set via Shared. Explicit Terminate still removes
	// them.
	reused bool
	// created reports whether the Run call that returned this handle
	// created the container. It is true for an ordinary Run and for the
	// WithReuse caller whose flight created the shared generation; an
	// attach to a generation another caller created reports false.
	created bool
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
	diagnosticBaseRedactor  *diagnosticRedactorCache
	secretsMu               sync.RWMutex
	targetHost              string
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

// applyConfigDiagnostics installs the per-handle redactor and freezes the
// target host. Every construction site must use it so a new handle cannot
// silently omit either field.
func (c *Container) applyConfigDiagnostics(cfg *config) {
	c.diagnosticRedactorValue = newDiagnosticMatcher(cfg.diagnosticValues()).WithBoundaryValues(cfg.diagnosticBoundaryValues()...)
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
	if err := cfg.validateDockerTargetBackend(); err != nil {
		return nil, err
	}
	cfg.eng = configuredTarget(cfg.eng, engineTargetOverride{
		host:      cfg.dockerHost,
		context:   cfg.dockerContext,
		configDir: cfg.dockerConfigDir,
		platform:  targetOverrideString(cfg.platform),
	})
	applyEngineBinary(cfg)
	applyEngineRunnerEnv(cfg)
	// Fail closed before the backend is asked anything if this platform
	// cannot store the secret environment file the run will need.
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
	materializeRunPlatform(cfg)
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
		identityErr := identityError(fmt.Sprintf("%s run returned no valid immutable container ID", cfg.eng.name()))
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
		created:         true,
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
	var createdInfo *engineInfo
	if target, ok := runReaperTarget(cfg); ok {
		info, err := verifyCreatedOwnership(ctx, c, cfg)
		if err != nil {
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
		createdInfo = info
		// completePending updates the in-memory state before any pipe
		// write. Never register a second active record as a fallback:
		// that could turn a failed state transition into a duplicate
		// destructive target.
		_ = completePreRegistrationWithGlobalReaperKey(target.reaperKey, cfg.name, cfg.creation)
		if uid := c.immutableID(); uid != "" {
			_ = registerWithGlobalReaperTarget(target.reaperKey, target.binary, target.subcommand, target.deleteFlags, target.env, uid, "")
		}
	}
	if err := verifyStrictCreatedImage(ctx, c, resolvedImage, createdInfo); err != nil {
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
		return nil, c.rollback(ctx, err)
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
			return nil, c.rollback(ctx, err)
		}
	}
	startTestLogger(cfg, c)
	if err := runPostReadyHooks(ctx, c, cfg.postReadyHooks); err != nil {
		return nil, c.rollback(ctx, err)
	}
	return c, envCleanupErr
}

func runWaitPorts(cfg *config) []string {
	ports := make([]string, 0, len(cfg.exposed)+len(cfg.published))
	for _, exposed := range cfg.exposed {
		ports = append(ports, exposed.String())
	}
	for _, published := range cfg.published {
		ports = append(ports, strconv.Itoa(published.ContainerPort)+"/"+published.Proto)
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
		env = targetPinnedEnv(cfg.eng)
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
// the identity. It is intentionally best effort: an absent global reaper is
// already safe, and the entry is dropped from the replay set before the
// removal is written, so a failed write cannot resurrect it.
func unregisterContainerReaper(cfg *config, name, creation, uid string) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	if uid != "" && cfg.eng.capabilities().immutableID {
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
//
//nolint:unparam // stderr is part of the runner result shape even when callers ignore it
func runCreateLocked(ctx context.Context, cfg *config, args ...string) (stdout []byte, stderr []byte, attempted bool, err error) {
	if !cfg.eng.capabilities().nameAddressed {
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
	return sleepWithContext(ctx, d)
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
	expected := domain.OwnershipExpected{
		RequireManaged: true,
		Session:        sessionID(),
		Creation:       cfg.creation,
		RequireReuse:   cfg.reuse,
		ReuseGroup:     cfg.reuseGroup,
	}
	if failure := domain.VerifyOwnership(expected, ownershipObserved(info)); failure != domain.OwnershipFailureNone {
		// The object is not the generation this create published. It may
		// belong to a peer, so leave it untouched.
		unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
		return nil
	}
	if cfg.reuse && info.state != StateStopped && info.state != StateCreated {
		// A running or transitional reuse generation may already have been
		// adopted by another caller. Only a freshly stopped/created
		// generation is eligible for automatic failed-create cleanup.
		protectReuseReaper(cfg)
		return fmt.Errorf("cleanup container %s: %s reuse generation may already be adopted; refusing automatic deletion", cfg.name, info.state)
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

// Image returns the image reference the container was created with.
func (c *Container) Image() string {
	c.pack().mu.Lock()
	defer c.pack().mu.Unlock()
	return c.image.reference
}

// Created reports whether the Run call that returned this handle created the
// container. It is always true for a plain Run. WithReuse reports true only
// for the caller whose shared flight created the generation; callers that
// attached to a generation created earlier or concurrently report false.
//
// Created is not a cross-caller synchronization barrier. Another WithReuse
// caller can attach and return before the creating caller's post-ready hook
// has finished, so initialization gated on Created must be idempotent or
// detected through a wait strategy or application-level signal.
func (c *Container) Created() bool {
	return c.created
}

// Reused reports whether this handle attached to a WithReuse generation
// instead of creating it. It is the inverse of Created and is independent of
// the shared-handle cleanup behavior: a WithReuse caller whose flight created
// the generation reports false here even though Cleanup still leaves it in
// place.
func (c *Container) Reused() bool {
	return !c.created
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
	// rememberIdentity may promote the image identity of a name-addressed
	// handle after construction, so read it under the same lock.
	c.pack().mu.Lock()
	image := c.image
	c.pack().mu.Unlock()
	return &Container{
		id: c.id, runner: c.runner, eng: c.eng,
		queryTimeout: c.queryTimeout, probeTimeout: c.probeTimeout,
		exposed: c.exposed, published: c.published,
		network: c.network, networkExplicit: c.networkExplicit,
		reused: c.reused, created: c.created, allowDestructive: true, creation: c.creation,
		image: image, nameInspect: c.nameInspect,
		sync: c.sync, dockerMeta: c.dockerMeta,
		diagnosticRedactorValue: diagnosticRedactor,
		targetHost:              c.targetHost,
	}
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
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
