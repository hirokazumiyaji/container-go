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
	// (Docker). Deletes target it directly, which makes the generation
	// check unnecessary: a replacement never shares it.
	//
	// It is promoted from the first inspect, so it is written long after the
	// handle is published. Guarded by uidMu rather than mu, because readers
	// on the Terminate path must not hold the inspect lock.
	uid   string
	uidMu sync.RWMutex
	// nameInspect marks short-lived name-addressed lookups. They may
	// inspect by name, but must never publish that name's UID into a
	// caller-visible handle or use it for a destructive operation.
	nameInspect bool

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
	// Register the exact logical name and generation before the backend
	// create starts. If the process is killed while `run` is in flight,
	// the reaper can wait for the pending generation instead of missing
	// the container entirely.
	preRegisterRunWithGlobalReaper(cfg)
	stdout, _, attempted, err := runCreateLocked(runCtx, cfg, cfg.eng.runArgs(cfg, image, envFile)...)
	if err != nil {
		if !attempted {
			if target, ok := runReaperTarget(cfg); ok {
				_ = unregisterWithGlobalReaper(target.binary, cfg.name, cfg.creation)
			}
			return nil, err
		}
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cleanupErr := cleanupFailedCreate(ctx, cfg, err, classified); cleanupErr != nil {
			return nil, withCleanupError(classified, &CleanupError{Container: cfg.name, Err: cleanupErr})
		}
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
				return nil, withCleanupError(err, &CleanupError{Container: cfg.name, Err: cleanupErr})
			}
			return nil, err
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
	if c == nil || cfg == nil || !creationRE.MatchString(c.creation) {
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
		if !isNotFoundFor(cfg.eng, err) {
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

// rollback removes a container Run created but cannot return. A failed
// removal is not hidden: without an immutable ID, Terminate refuses to
// delete when it cannot verify the generation, and the caller must know
// the container was left behind.
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
	if !creationRE.MatchString(cfg.creation) {
		return fmt.Errorf("cleanup container %s: creation generation is missing or invalid", cfg.name)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	var unlock func()
	if cfg.eng.name() == "apple" {
		var err error
		unlock, err = lockName(cleanupCtx, cfg.name)
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
		if !ok || !creationRE.MatchString(actual) || actual != cfg.creation {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
		// A running or transitional generation may already have been
		// adopted by another caller. Only a freshly stopped/created
		// generation is eligible for automatic failed-create cleanup.
		if info.state != StateStopped && info.state != StateCreated {
			protectReuseReaper(cfg)
			return fmt.Errorf("cleanup container %s: %s reuse generation may already be adopted; refusing automatic deletion", cfg.name, info.state)
		}
	} else {
		actual, ok := info.labels[creationLabel]
		if !ok || !creationRE.MatchString(actual) || actual != cfg.creation {
			unregisterContainerReaper(cfg, cfg.name, cfg.creation, "")
			return nil
		}
	}
	if info.state != StateStopped && info.state != StateCreated {
		if cfg.reuse {
			protectReuseReaper(cfg)
		}
		return fmt.Errorf("cleanup container %s: %s generation may already be adopted; refusing automatic deletion", cfg.name, info.state)
	}
	target := cfg.name
	if cfg.eng.name() == "docker" {
		if !dockerIDRE.MatchString(info.uid) {
			if cfg.reuse {
				protectReuseReaper(cfg)
			}
			return fmt.Errorf("cleanup container %s: inspect returned no verified immutable ID", cfg.name)
		}
		target = info.uid
	} else if info.uid != "" {
		if cfg.reuse {
			protectReuseReaper(cfg)
		}
		return fmt.Errorf("cleanup container %s: unexpected immutable ID", cfg.name)
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	if err := ctr.delete(delCtx, target); err != nil {
		return fmt.Errorf("cleanup container %s: %w", cfg.name, err)
	}
	unregisterContainerReaper(cfg, cfg.name, cfg.creation, target)
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

// ID returns the logical container ID (the configured name). Backend
// operations use the immutable UID when one has been verified.
func (c *Container) ID() string { return c.id }

func (c *Container) infoSnapshot() *engineInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// operationTarget returns the verified immutable identity when the
// backend has one, and the logical name otherwise. The UID is never
// replaced by a name-addressed inspect result.
func (c *Container) operationTarget() string {
	uid := c.immutableID()
	if c.eng.name() == "docker" {
		if dockerIDRE.MatchString(uid) {
			return uid
		}
		if uid != "" {
			return ""
		}
		if c.nameInspect {
			return c.id
		}
		return ""
	}
	return c.id
}

func (c *Container) verifiedOperationTarget() (string, error) {
	if c.eng.name() == "docker" {
		uid := c.immutableID()
		if !dockerIDRE.MatchString(uid) {
			return "", fmt.Errorf("%w: container %s has no verified immutable backend ID", ErrGenerationReplaced, c.id)
		}
		return uid, nil
	}
	if c.eng.name() == "apple" {
		if c.nameInspect || !creationRE.MatchString(c.creation) || c.immutableID() != "" {
			return "", fmt.Errorf("%w: container %s has no verified creation generation", ErrGenerationReplaced, c.id)
		}
	}
	target := c.operationTarget()
	if target == "" {
		return "", fmt.Errorf("container %s has no backend target", c.id)
	}
	return target, nil
}

func (c *Container) checkedOperationTarget() (string, error) {
	return c.verifiedOperationTarget()
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
	target, err := c.checkedOperationTarget()
	if err != nil {
		return err
	}
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
	uid := c.immutableID()
	if c.eng.name() == "docker" {
		if dockerIDRE.MatchString(uid) {
			return c.delete(ctx, uid)
		}
		if !c.nameInspect {
			return fmt.Errorf("%w: container %s has no verified immutable backend ID", ErrGenerationReplaced, c.id)
		}
	}
	if c.eng.name() == "apple" && !creationRE.MatchString(c.creation) {
		return fmt.Errorf("%w: container %s has no verified creation generation", ErrGenerationReplaced, c.id)
	}
	if c.creation == "" {
		return c.delete(ctx, c.id)
	}
	unlock, err := lockName(ctx, c.id)
	if err != nil {
		return fmt.Errorf("terminate %s: lock name: %w", c.id, err)
	}
	defer unlock()
	info, err := c.inspectFreshLocked(ctx)
	if isNotFoundFor(c.eng, err) {
		unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, "")
		return nil
	}
	if err != nil {
		return fmt.Errorf("terminate %s: verify generation: %w", c.id, err)
	}
	// An absent generation cannot prove ownership of this handle, so
	// it counts as a replacement too.
	if info.labels[creationLabel] != c.creation {
		unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, "")
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
	info, err := c.cachedInfoFor(ctx, true, false)
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
	info, err := c.cachedInfoFor(ctx, false, true)
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

// cachedInfo returns a fresh-enough inspect result. Labels and state are
// stable for a handle, but network addresses and published bindings can
// appear after the first inspect. A partial result is retained only as a
// cache candidate; the next operation that needs the missing metadata
// refreshes it.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
	if c.nameInspect {
		return c.inspectFresh(ctx)
	}
	return c.cachedInfoFor(ctx, false, false)
}

func (c *Container) cachedInfoFor(ctx context.Context, requireIP, requirePorts bool) (*engineInfo, error) {
	if c.nameInspect {
		return c.inspectFresh(ctx)
	}
	c.mu.Lock()
	if c.info != nil && c.infoCompleteLocked(c.info, requireIP, requirePorts) {
		info := c.info
		c.mu.Unlock()
		return info, nil
	}
	c.mu.Unlock()

	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	// Delegate, so the "never downgrade a promoted value" invariant lives in
	// one place rather than being reimplemented here.
	c.setImmutableID(info.uid)
	return info, nil
}

func (c *Container) infoCompleteLocked(info *engineInfo, requireIP, requirePorts bool) bool {
	if info == nil || info.state == "" || info.state == StateUnknown {
		return false
	}
	if requireIP && info.ip == "" {
		return false
	}
	if requirePorts {
		if !c.eng.directIP() {
			for _, spec := range c.exposed {
				if !hasBoundPort(info.bound, spec.port, spec.proto) {
					return false
				}
			}
			for _, spec := range c.published {
				if !hasPublishedBinding(info.bound, spec) {
					return false
				}
			}
		}
	}
	return true
}

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	if c.eng.name() == "apple" {
		lockCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
		defer cancel()
		unlock, err := lockName(lockCtx, c.id)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: lock name: %w", c.id, err)
		}
		info, inspectErr := c.inspectFreshLocked(lockCtx)
		unlock()
		return info, inspectErr
	}
	return c.inspectFreshLocked(ctx)
}

// inspectFreshLocked is the implementation for callers that already hold
// the Apple per-name lock. Keeping the lock scope explicit prevents an
// inspect/delete pair from accidentally releasing the lock between them.
func (c *Container) inspectFreshLocked(ctx context.Context) (*engineInfo, error) {
	if c.eng.name() == "apple" && !c.nameInspect && !creationRE.MatchString(c.creation) {
		return nil, fmt.Errorf("%w: container %s has no verified creation generation", ErrGenerationReplaced, c.id)
	}
	target := c.operationTarget()
	if target == "" {
		return nil, fmt.Errorf("%w: container %s has no verified immutable backend ID", ErrGenerationReplaced, c.id)
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(target)...)
	if err != nil {
		return nil, wrapNotFoundFor(c.eng, c.classify(ctx, err))
	}
	info, err := c.eng.parseInspect(stdout, target)
	if err != nil {
		return nil, err
	}
	if c.eng.name() == "docker" && dockerIDRE.MatchString(target) {
		if !dockerIDRE.MatchString(info.uid) || info.uid != target {
			return nil, fmt.Errorf("container %s inspect returned immutable ID %q", target, info.uid)
		}
		c.setImmutableID(info.uid)
	}
	if c.eng.name() == "apple" && !c.nameInspect && c.creation != "" &&
		info.labels[creationLabel] != c.creation {
		return nil, fmt.Errorf("%w: %s", ErrGenerationReplaced, c.id)
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
