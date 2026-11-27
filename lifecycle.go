package container

import (
	"context"
	"errors"
	"fmt"
	"github.com/hirokazumiyaji/container-go/internal/domain"
	"io"
	"regexp"
	"strings"
	"time"
)

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
	if c.eng != nil {
		return c.eng.reaperSubcommand()
	}
	return "delete"
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
	stopCtx, cancel := withDefaultTimeout(ctx, addTimeout(c.effectiveQueryTimeout(), durationOrZero(timeout)))
	defer cancel()
	target, err := c.verifiedOperationTarget(stopCtx)
	if err != nil {
		return err
	}
	args, err := c.eng.stopArgs(target, timeout)
	if err != nil {
		return fmt.Errorf("stop %s: %w", c.id, err)
	}
	teardown := c.markAttachTeardown()
	_, _, err = c.runner.Run(stopCtx, args...)
	if err != nil {
		c.clearAttachTeardown(teardown)
		return c.classifyOperation(ctx, err, "stop")
	}
	c.stopAttachedTarget(teardown)
	c.clearDynamic()
	return nil
}

// ExitCode returns the reported exit code of a stopped container.
// ErrContainerNotStopped indicates that the process has not stopped. Both that
// error and an omitted backend exit code match ErrExitCodeUnavailable.
// Apple CLI versions that omit status.exitCode cannot provide a process result.
func (c *Container) ExitCode(ctx context.Context) (int, error) {
	info, err := c.inspectDynamicFresh(ctx)
	if err != nil {
		// A bare sentinel is returned as-is; a wrapped deadline takes the
		// redacted path below so its diagnostic values stay protected.
		if err == context.Canceled || err == context.DeadlineExceeded { //nolint:errorlint // exact identity selects the unredacted path
			return 0, err
		}
		redacted := c.redactError(err)
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, exitCodeDeadlineError{redacted}
		}
		return 0, redacted
	}
	return reportedExitCode(info)
}

// Keep the deadline visible to callers classifying redacted inspect failures.
type exitCodeDeadlineError struct{ error }

func (e exitCodeDeadlineError) Unwrap() []error {
	return []error{e.error, context.DeadlineExceeded}
}

func reportedExitCode(info *engineInfo) (int, error) {
	if info.state != StateStopped {
		return 0, fmt.Errorf("%w: %w: container state is %s", ErrContainerNotStopped, ErrExitCodeUnavailable, info.state)
	}
	if !info.exitCodeReported {
		return 0, ErrExitCodeUnavailable
	}
	return info.exitCode, nil
}

// Wait polls an inspect-backed state read. The interval starts small so a
// quick exit is noticed promptly, then backs off to a cap: a long batch
// container should cost a bounded number of CLI subprocesses.
var (
	waitExitPollInterval    = 100 * time.Millisecond
	waitExitPollMaxInterval = time.Second
)

func nextWaitExitPollInterval(current time.Duration) time.Duration {
	next := current * 2
	if next > waitExitPollMaxInterval {
		return waitExitPollMaxInterval
	}
	return next
}

// Wait blocks until the container reaches StateStopped and returns its exit code.
// If the container is already stopped, it returns immediately. A missing backend
// exit code returns ErrExitCodeUnavailable rather than an assumed zero result.
// Retrying individual query deadlines requires a deadline on ctx.
func (c *Container) Wait(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	interval := waitExitPollInterval
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
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			_, bounded := ctx.Deadline()
			if retryErr := inspectRetryError(ctx, err); retryErr != nil {
				return 0, retryErr
			}
			if !bounded && errors.Is(err, context.DeadlineExceeded) {
				return 0, err
			}
		} else if info.state == StateStopped {
			return reportedExitCode(info)
		}
		timer.Reset(interval)
		interval = nextWaitExitPollInterval(interval)
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
	restartCtx, cancel := withDefaultTimeout(ctx, addTimeout(c.effectiveQueryTimeout(), durationOrZero(timeout)))
	defer cancel()
	target, err := c.verifiedOperationTarget(restartCtx)
	if err != nil {
		return err
	}
	lifecycle, ok := c.eng.(engineLifecycleExtensions)
	if !ok {
		return c.restartAttached(ctx, timeout)
	}
	args, err := lifecycle.restartArgs(target, timeout)
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
// An empty signal defaults to SIGKILL. Only a signal that is guaranteed to
// end the main process also ends the WithAttachOutput stream; a stream left
// attached by a catchable signal ends when the process exits.
func (c *Container) Kill(ctx context.Context, signal string) error {
	if err := validateKillSignal(signal); err != nil {
		return err
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
	args := c.eng.killArgs(target, signal)
	terminating := terminatingKillSignal(signal)
	var teardown attachTeardownTarget
	if terminating {
		teardown = c.markAttachTeardown()
	}
	_, _, err = c.runner.Run(killCtx, args...)
	if err != nil {
		if terminating {
			c.clearAttachTeardown(teardown)
		}
		return wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "kill"), "kill", target, c.id)
	}
	if terminating {
		c.stopAttachedTarget(teardown)
	}
	c.clearDynamic()
	return nil
}

// killSignalPattern accepts the signal spellings the backends understand: a
// positive signal number, a signal name such as SIGTERM or KILL, or a realtime
// signal spelling such as SIGRTMIN+3. A leading sign, whitespace, control
// character, or flag-shaped value must never reach the CLI argv.
var killSignalPattern = regexp.MustCompile(`^(?:[1-9][0-9]*|(?:SIG)?[A-Z][A-Z0-9]*(?:[+-][0-9]+)?)$`)

// validateKillSignal rejects a signal that is not a single signal spelling.
// The rejected value is never echoed because it may contain control
// characters.
func validateKillSignal(signal string) error {
	if signal == "" || killSignalPattern.MatchString(signal) {
		return nil
	}
	return newValidationErrorWithField("Kill", "signal", nil, errors.New("invalid signal: want a positive number or an uppercase signal name"))
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
	lifecycle, ok := c.eng.(engineLifecycleExtensions)
	if !ok {
		return unsupportedLifecycle(c.eng, "pause")
	}
	args, err := lifecycle.pauseArgs(target)
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
	lifecycle, ok := c.eng.(engineLifecycleExtensions)
	if !ok {
		return unsupportedLifecycle(c.eng, "unpause")
	}
	args, err := lifecycle.unpauseArgs(target)
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

// StopAttached terminates any active background log streaming started by WithAttachOutput.
func (c *Container) StopAttached() {
	c.stopAttached()
}

// attachStopWait bounds how long a cancelled attach stream may take to finish.
// Cancellation stops a stream at its next read, so only a sink wedged inside a
// write can exceed it.
const attachStopWait = 500 * time.Millisecond

// attachTeardownTarget identifies the attached stream a Stop or terminating
// Kill is ending. The target carries the stream's shared teardown state so
// overlapping commands resolve one outcome instead of replacing each other's.
type attachTeardownTarget struct {
	cancel     context.CancelFunc
	streamDone chan struct{}
	state      *attachTeardownState
	command    bool
}

// attachTeardownState is the outcome of ending one attached stream. Owners
// counts the operations still deciding whether the container stopped; the last
// owner discards the observed failure after any command succeeded, or publishes
// it when command owners failed. A stream ended only by StopAttached never
// publishes the failure.
type attachTeardownState struct {
	streamDone chan struct{}
	cancel     context.CancelFunc
	// heldDone is the public AttachedDone channel kept open while err is
	// unresolved.
	heldDone      chan struct{}
	err           error
	owners        int
	commandOwners int
	succeeded     bool
}

// attachTeardownForLocked joins or creates the teardown state for a stream
// and adds an owner. The caller must hold p.attachMu.
func attachTeardownForLocked(p *lockPack, streamDone chan struct{}, cancel context.CancelFunc) attachTeardownTarget {
	if p.attachTeardowns == nil {
		p.attachTeardowns = make(map[chan struct{}]*attachTeardownState)
	}
	state := p.attachTeardowns[streamDone]
	if state == nil {
		state = &attachTeardownState{streamDone: streamDone, cancel: cancel}
		p.attachTeardowns[streamDone] = state
	}
	state.owners++
	return attachTeardownTarget{cancel: state.cancel, streamDone: streamDone, state: state}
}

// markAttachTeardown records that a library operation is ending the container
// and therefore its attached stream. An error the backend reports while the
// container stops belongs to that teardown, not to the stream. Commands do not
// block each other here: a force-kill must reach the backend while a graceful
// Stop is still running.
func (c *Container) markAttachTeardown() attachTeardownTarget {
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	target := attachTeardownForLocked(p, p.attachStreamDone, p.attachCancel)
	target.command = true
	target.state.commandOwners++
	return target
}

// clearAttachTeardown records that the operation failed. The container may
// still be running, so any stream failure observed during it belongs to the
// stream and is published once the last command owning the stream resolves.
func (c *Container) clearAttachTeardown(target attachTeardownTarget) {
	c.resolveAttachTeardown(target, false)
}

// resolveAttachTeardown releases one operation's claim on a stream's outcome.
// The last owner publishes the deferred failure when a command failed and none
// succeeded, and closes the held AttachedDone after the follower's internal
// completion.
func (c *Container) resolveAttachTeardown(target attachTeardownTarget, succeeded bool) {
	p := c.pack()
	p.attachMu.Lock()
	state := target.state
	if target.command && succeeded {
		state.succeeded = true
	}
	if state.owners > 0 {
		state.owners--
	}
	if state.owners > 0 {
		p.attachMu.Unlock()
		return
	}
	if p.attachTeardowns[state.streamDone] == state {
		delete(p.attachTeardowns, state.streamDone)
	}
	if state.commandOwners > 0 && !state.succeeded {
		p.attachErr = joinAttachError(p.attachErr, state.err)
	}
	held := state.heldDone
	streamDone := state.streamDone
	p.attachMu.Unlock()
	closeAttachDoneAfterStream(held, streamDone)
}

// stopAttached stops the current attached stream. Callers that started a Stop
// or terminating Kill must use stopAttachedTarget instead so a Start that
// reattached in the meantime keeps its stream.
func (c *Container) stopAttached() {
	p := c.pack()
	p.attachMu.Lock()
	cancel := p.attachCancel
	streamDone := p.attachStreamDone
	p.attachCancel = nil
	// Mark the teardown before cancelling: a stream error that races the
	// stop belongs to the teardown, not to the stream.
	target := attachTeardownForLocked(p, streamDone, cancel)
	p.attachMu.Unlock()
	c.finishAttachStop(cancel, streamDone)
	c.resolveAttachTeardown(target, true)
}

// stopAttachedTarget stops the stream a lifecycle command marked for teardown
// and records the command's successful outcome.
func (c *Container) stopAttachedTarget(target attachTeardownTarget) {
	c.finishAttachStop(target.cancel, target.streamDone)
	c.resolveAttachTeardown(target, true)
}

func (c *Container) finishAttachStop(cancel context.CancelFunc, streamDone chan struct{}) {
	p := c.pack()
	p.attachMu.Lock()
	if streamDone == p.attachStreamDone {
		// Mark the follower inactive before the drain wait so a concurrent
		// Start replaces it instead of treating the cancelled stream as live.
		p.attachCancel = nil
	}
	p.attachMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if streamDone != nil {
		select {
		case <-streamDone:
		case <-time.After(attachStopWait):
		}
	}
}

// closeAttachDoneAfterStream releases a held AttachedDone only after the
// follower's internal completion. A caller that observes the public channel
// closed must not still see the finished stream as active.
func closeAttachDoneAfterStream(done, streamDone chan struct{}) {
	if done == nil {
		return
	}
	if streamDone != nil {
		select {
		case <-streamDone:
		case <-time.After(attachStopWait):
		}
	}
	close(done)
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
	streamDone := make(chan struct{})
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
	// Pending teardown states belong to their commands, which keep honouring
	// the stream they marked even after a replacement starts.
	p.attachCancel = cancel
	p.attachDone = done
	p.attachStreamDone = streamDone
	p.attachMu.Unlock()
	go func() {
		// A caller that sees AttachedDone close may immediately call Start, so
		// the internal completion must be visible first or that Start sees this
		// stream as still active and leaves the writer detached.
		deferred := false
		defer func() {
			cancel()
			close(streamDone)
			if !deferred {
				close(done)
			}
		}()
		// The gate keeps a replacement stream from writing to the same
		// caller-supplied io.Writer while its predecessor is still inside a
		// write. A wedged sink never releases it, so give up after the same
		// bound StopAttached uses and report the failure instead of blocking
		// forever.
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			err := c.followLogsTo(attachCtx, output, FollowLogsOptions{}, resumeSince)
			// A teardown cancellation is not a stream failure, but losing
			// output to a failing sink is always reportable.
			if attachCtx.Err() == nil || errors.Is(err, ErrOutputDelivery) {
				if c.recordAttachError(done, streamDone, err) {
					// The teardown outcome decides whether this failure is
					// published. Keep AttachedDone open until then.
					deferred = true
				}
			}
		case <-time.After(attachStopWait):
			// Failing to attach a replacement is an output-delivery failure,
			// not expected teardown noise: a successful teardown must not
			// hide that the writer could not be served.
			err := fmt.Errorf("%w: logs: previous attached stream is still writing to the output; not starting a replacement", ErrOutputDelivery)
			if c.recordAttachError(done, streamDone, err) {
				deferred = true
			}
		}
	}()
}

// recordAttachError accumulates a terminal stream failure for AttachedError.
// A later failure is joined with the earlier ones so a caller holding an older
// AttachedDone keeps matching its stream's failure; a failure that already
// matches the accumulated value is ignored instead of growing the join.
// Failures observed while a lifecycle command tears down the stream wait until
// the command's outcome is known; output-delivery failures are recorded
// immediately because stopping the container must not hide lost output. The
// return value reports whether the failure was deferred, in which case the
// caller must leave AttachedDone open for the teardown outcome to close.
func (c *Container) recordAttachError(done, streamDone chan struct{}, err error) bool {
	if err == nil {
		return false
	}
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	// Losing output is always reportable, even while a command tears the
	// stream down.
	if errors.Is(err, ErrOutputDelivery) {
		p.attachErr = joinAttachError(p.attachErr, err)
		return false
	}
	if state := p.attachTeardowns[streamDone]; state != nil {
		state.err = joinAttachError(state.err, err)
		state.heldDone = done
		return true
	}
	p.attachErr = joinAttachError(p.attachErr, err)
	return false
}

// joinAttachError adds err to the accumulated failures without duplicating a
// failure already present in existing.
func joinAttachError(existing, err error) error {
	if err == nil {
		return existing
	}
	if existing == nil {
		return err
	}
	if errors.Is(existing, err) {
		return existing
	}
	return errors.Join(existing, err)
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
	done := p.attachStreamDone
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
// log stream finishes and any failure observed while a teardown was in flight
// has been resolved. If WithAttachOutput was not used, it returns nil.
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
// observes its failure after the channel closes. A failure observed while a
// teardown command is running is resolved before that stream's AttachedDone
// closes. Failures are accumulated with errors.Join, which keeps the latest
// stream's failure included, so errors.Is matches every failure the streams
// recorded.
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
	if c.eng.capabilities().nameAddressed {
		uid, creation, err := c.identitySnapshot(ctx)
		if err != nil {
			return err
		}
		if uid != "" {
			return identityError(fmt.Sprintf("%s inspect unexpectedly returned a container ID", c.eng.name()))
		}
		if !domain.IsCreationID(creation) {
			return identityError(fmt.Sprintf("%s handle has no valid creation generation", c.eng.name()))
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
	if err != nil {
		if contextErr := commandContextError(delCtx, err); contextErr != nil {
			return contextErr
		}
	}
	if err == nil || isNotFoundForOperation(c.eng, err, c.deleteOperation(), target, c.id) {
		unregisterContainerReaper(&config{runner: c.runner, eng: c.eng, name: c.id, creation: c.creation}, c.id, c.creation, target)
		c.clearDynamic()
		c.clearDiagnosticSecrets()
		return nil
	}
	return c.classifyOperation(ctx, err, c.deleteOperation())
}

func durationOrZero(d *time.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return *d
}
