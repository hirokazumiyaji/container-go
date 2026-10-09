package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
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
type State string

const (
	StateRunning    State = "running"
	StateStopped    State = "stopped"
	StateStopping   State = "stopping"
	StateCreated    State = "created"
	StateRestarting State = "restarting"
	StatePaused     State = "paused"
	StateUnknown    State = "unknown"
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
	//
	// It is promoted from the first inspect, so it is written long after the
	// handle is published. Guarded by uidMu rather than mu, because readers
	// on the Terminate path must not hold the inspect lock.
	uid   string
	uidMu sync.RWMutex

	mu        sync.Mutex
	info      *engineInfo // immutable identity snapshot; dynamic data is never cached
	inspectMu contextLock // serializes inspect target selection and UID publication
	// nameInspect is limited to short-lived name lookups used by reuse
	// discovery and failed-create cleanup. It never publishes an inspected
	// Docker UID to a caller-visible handle.
	nameInspect bool
}

// immutableID returns the backend's immutable container ID, or "" when the
// backend has none. The ID is only ever promoted from empty to a real value,
// so a caller that observes "" may re-read after a failed operation.
func (c *Container) immutableID() string {
	c.uidMu.RLock()
	defer c.uidMu.RUnlock()
	return c.uid
}

func (c *Container) infoSnapshot() *engineInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
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
	// Validate the public image before applying options. Option functions
	// are user-provided code and may have side effects or return errors;
	// an invalid image must have a deterministic, side-effect-free result.
	if err := validateImageReference(image); err != nil {
		return nil, err
	}
	cfg := newConfig()
	for i, opt := range opts {
		if opt == nil {
			return nil, validationErrorf("Run", i, "option %d is nil", i)
		}
		if err := opt(cfg); err != nil {
			return nil, err
		}
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

	// Register the exact logical name and generation before the backend
	// create starts. If the process is killed while `run` is in flight,
	// the reaper can wait for the pending generation instead of missing
	// the container entirely.
	preRegisterRunWithGlobalReaper(cfg)
	stdout, _, attempted, runErr := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	envCleanupErr := cleanupEnvFileAfterUseContext(runCtx, envDir)
	if envCleanupErr == nil {
		envDir = ""
	}
	if runErr != nil {
		if !attempted {
			if target, ok := runReaperTarget(cfg); ok {
				_ = unregisterWithGlobalReaper(target.binary, cfg.name, cfg.creation)
			}
			return nil, joinEnvFileCleanupError(runErr, envCleanupErr)
		}
		classified := cli.Classify(ctx, cfg.runner, runErr, cfg.eng.probe())
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
		exposed:         cfg.exposed,
		published:       cfg.published,
		network:         cfg.network,
		networkExplicit: cfg.networkExplicit,
		creation:        cfg.creation,
		uid:             uid,
	}
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
		_ = completePreRegistrationWithGlobalReaper(target.binary, cfg.name, cfg.creation)
		if uid := c.immutableID(); uid != "" {
			_ = registerWithGlobalReaper(target.binary, target.subcommand, target.deleteFlags, uid, "")
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
		binary = cfg.eng.binary()
	}
	return runReaperTargetInfo{
		binary:      binary,
		subcommand:  cfg.eng.reaperSubcommand(),
		deleteFlags: append([]string(nil), cfg.eng.reaperDeleteFlags()...),
	}, true
}

func preRegisterRunWithGlobalReaper(cfg *config) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	_ = preRegisterWithGlobalReaper(target.binary, target.subcommand, target.deleteFlags, cfg.name, cfg.creation)
}

func unregisterReuseFromGlobalReaper(cfg *config) {
	target, ok := runReaperTarget(cfg)
	if !ok {
		return
	}
	_ = unregisterWithGlobalReaper(target.binary, cfg.name, cfg.creation)
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
		_ = unregisterWithGlobalReaper(target.binary, uid, "")
	}
	if name != "" {
		_ = unregisterWithGlobalReaper(target.binary, name, creation)
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
	_ = markSharedWithGlobalReaper(target.binary, cfg.name, cfg.creation)
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

func waitForReusePoll(ctx context.Context) error {
	timer := time.NewTimer(reusePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// verifyCreatedOwnership validates the labels and backend identity before
// an external reaper is allowed to retain a newly created container. A
// successful `run` command alone is not proof that the library's ownership
// labels were attached.
func verifyCreatedOwnership(ctx context.Context, c *Container, cfg *config) error {
	if c == nil || cfg == nil || !validCreationID(c.creation) {
		return fmt.Errorf("verify created container: invalid creation generation")
	}
	verifyCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
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
			inspectContainer = &Container{id: uid, uid: uid, runner: c.runner, eng: c.eng}
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
		if err := waitForReusePoll(verifyCtx); err != nil {
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
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	c.setImmutableID(info.uid)
	return nil
}

// rollback removes a container Run created but cannot return. A failed removal
// is not hidden: without an immutable ID, Terminate refuses to delete when it
// cannot verify the generation, and the caller must know the container was left
// behind.
func (c *Container) rollback(ctx context.Context, cause error) error {
	if keepContainers() {
		return cause
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if c.reused {
		cfg := &config{runner: c.runner, eng: c.eng, name: c.id, reuse: true, creation: c.creation}
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
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
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
		if isNotFoundFor(cfg.eng, err) {
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
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
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
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
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
	return c.classify(ctx, err)
}

// Terminate force-removes the container. A Docker handle must carry a
// validated immutable UID; a name-only handle is never an acceptable
// fallback. Apple Container has no immutable UID, so its name path is
// allowed only with a valid generation and a fresh locked inspect.
func (c *Container) Terminate(ctx context.Context) error {
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
	delCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err := c.runner.Run(delCtx, c.eng.deleteArgs(target)...)
	if err == nil || isNotFoundFor(c.eng, err) {
		unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, target)
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
	_, p, err := c.resolve(ctx, port, "MappedPort")
	return p, err
}

// Endpoint returns "host:port" for a port declared via WithExposedPorts
// or WithPublishedPort. It does not infer a host-network service port.
func (c *Container) Endpoint(ctx context.Context, port string) (string, error) {
	host, p, err := c.resolve(ctx, port, "Endpoint")
	if err != nil {
		return "", err
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if err := c.rememberIdentity(ctx, info); err != nil {
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
	if err := c.applyCachedDefaultNetwork(ctx, info); err != nil {
		return nil, err
	}
	if c.nameInspect {
		return info, nil
	}
	if err := c.rememberIdentity(ctx, info); err != nil {
		return nil, err
	}
	return info, nil
}

func (c *Container) rememberIdentity(ctx context.Context, info *engineInfo) error {
	identity := immutableInfo(info)
	if err := c.inspectMu.Lock(ctx); err != nil {
		return err
	}
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

// identitySnapshot reads the handle's backend identity and creation
// generation while holding the same lock that protects their publication.
func (c *Container) identitySnapshot(ctx context.Context) (uid, creation string, err error) {
	if err := c.inspectMu.Lock(ctx); err != nil {
		return "", "", err
	}
	defer c.inspectMu.Unlock()
	return c.uid, c.creation, nil
}

// verifiedOperationTarget is the safe target for lifecycle, copy, exec,
// and log operations. In particular, a Docker handle with an empty or
// malformed UID fails before a CLI call instead of falling back to its
// name. Apple handles likewise require a valid generation before using a
// name-addressed operation. Lock acquisition is context-aware so a copy
// cannot wait behind another call's inspect or default-network lookup after
// its caller has been canceled.
func (c *Container) verifiedOperationTarget(ctx context.Context) (string, error) {
	if err := c.inspectMu.Lock(ctx); err != nil {
		return "", err
	}
	defer c.inspectMu.Unlock()
	uid := c.uid
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
	if err := c.inspectMu.Lock(ctx); err != nil {
		return "", err
	}
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

func (c *Container) applyCachedDefaultNetwork(ctx context.Context, info *engineInfo) error {
	if err := c.inspectMu.Lock(ctx); err != nil {
		return err
	}
	defer c.inspectMu.Unlock()
	if c.defaultNetwork != "" {
		info.defaultNetwork = c.defaultNetwork
	}
	return nil
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	if err := c.inspectMu.Lock(ctx); err != nil {
		return nil, err
	}
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
