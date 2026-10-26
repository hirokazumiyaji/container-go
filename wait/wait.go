// Package wait provides startup readiness strategies for containers.
// Apple Container has no healthcheck or wait primitive, so every
// strategy here probes from the client side.
package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	defaultStartupTimeout = 60 * time.Second
	defaultPollInterval   = 100 * time.Millisecond
	stateCheckInterval    = time.Second
	lifecycleProbeTimeout = 5 * time.Second
)

// finalLifecycleProbeTimeout bounds composite final lifecycle retries even
// when the wait context is otherwise unbounded. Overridable in tests.
var finalLifecycleProbeTimeout = 30 * time.Second

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port asks the built-in adapter for
	// its first exposed TCP declaration, falling back to a published TCP
	// declaration.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream. A
	// terminal CLI failure is returned by Read after the stream starts.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// State is a container lifecycle state reported by Target.
type State string

const (
	StateUnknown    State = "unknown"
	StateCreated    State = "created"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateRestarting State = "restarting"
	StatePaused     State = "paused"
)

// StateTarget is an optional extension to Target for targets that can
// distinguish transitional states (Created, Restarting, Unknown) from
// terminal ones (Stopped, Stopping, Paused). Built-in strategies use it
// when available and fall back to Running.
type StateTarget interface {
	State(ctx context.Context) (State, error)
}

// Health is a Docker HEALTHCHECK status reported by Target. Its zero value
// means the image defines no HEALTHCHECK.
type Health string

const (
	HealthNone      Health = ""
	HealthStarting  Health = "starting"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

// HealthTarget is an optional extension to Target for targets that can
// report Docker HEALTHCHECK status. ForHealthcheck returns a ConfigError for
// targets that do not implement it.
type HealthTarget interface {
	Health(ctx context.Context) (Health, error)
}

// Strategy waits until a started container is ready for use.
// Implementations must return when ctx is done so composite strategies can
// collect their final causes without extending the caller's budget.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

type options struct {
	startupTimeout time.Duration
	pollInterval   time.Duration
	// stateFirst runs the lifecycle probe before the readiness check when
	// it is due. Healthcheck waits use it so the 1 Hz inspect that refreshes
	// the health snapshot happens before the check reads that snapshot.
	stateFirst bool
}

func (o options) effective() (timeout, interval time.Duration) {
	timeout, interval = o.startupTimeout, o.pollInterval
	if timeout == 0 {
		timeout = defaultStartupTimeout
	}
	if interval == 0 {
		interval = defaultPollInterval
	}
	return timeout, interval
}

// waitReason identifies how a wait ended. The reason selects the diagnostic
// wording; the causes themselves are never rewritten.
type waitReason uint8

const (
	waitReasonMessage waitReason = iota
	waitReasonCause
	waitReasonContext
	waitReasonTimeout
	waitReasonState
)

// waitError reports a wait failure from structured parts: the operation
// name, the termination reason, the last check error, the last state probe
// error, and any terminal cause. Error() renders those parts once; Is/As
// walk the retained causes directly. A context sentinel is kept only as the
// termination reason, so a backend cause can never make the wait appear
// canceled.
type waitError struct {
	reason     waitReason
	what       string
	message    string
	timeout    time.Duration
	state      State
	contextErr error
	checkErr   error
	probeErr   error
	cause      error
	causes     []error
}

func (e *waitError) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch e.reason {
	case waitReasonContext:
		return appendLastError(fmt.Sprintf("%s: %v", e.what, e.contextErr), e.lastDiagnostic())
	case waitReasonTimeout:
		return appendLastError(fmt.Sprintf("%s: timed out after %v", e.what, e.timeout), e.lastDiagnostic())
	case waitReasonState:
		return appendLastError(fmt.Sprintf("%s: container %s while waiting", e.what, e.state), e.lastDiagnostic())
	case waitReasonCause:
		if e.cause == nil {
			return e.message
		}
		if e.what == "" {
			return e.cause.Error()
		}
		return fmt.Sprintf("%s: %v", e.what, e.cause)
	default:
		return e.message
	}
}

func appendLastError(message string, last error) string {
	if last == nil || last.Error() == "" {
		return message
	}
	return message + fmt.Sprintf(" (last error: %v)", last)
}

func (e *waitError) lastDiagnostic() error {
	return joinNonNil(e.checkErr, e.probeErr)
}

// lookupCauses returns the retained causes in diagnostic order. The context
// termination reason is deliberately absent: it terminated the wait, it is
// not a failure cause.
func (e *waitError) lookupCauses() []error {
	causes := make([]error, 0, 3+len(e.causes))
	if e.checkErr != nil {
		causes = append(causes, e.checkErr)
	}
	if e.probeErr != nil {
		causes = append(causes, e.probeErr)
	}
	if e.cause != nil {
		causes = append(causes, e.cause)
	}
	causes = append(causes, e.causes...)
	return causes
}

func (e *waitError) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	if e.contextErr != nil && errors.Is(e.contextErr, target) {
		return true
	}
	for _, cause := range e.lookupCauses() {
		if errors.Is(cause, target) {
			return true
		}
	}
	return false
}

func (e *waitError) As(target any) bool {
	if e == nil || target == nil {
		return false
	}
	for _, cause := range e.lookupCauses() {
		if errors.As(cause, target) {
			return true
		}
	}
	return false
}

func (e *waitError) AsRedacted(target any, redactor *cli.Redactor) bool {
	if e == nil {
		return false
	}
	for _, cause := range e.lookupCauses() {
		if errors.As(cli.WithRedactor(cause, redactor), target) {
			return true
		}
	}
	return false
}

func (e *waitError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.contextErr != nil {
		return e.contextErr
	}
	if e.reason == waitReasonCause && e.cause != nil {
		return e.cause
	}
	if e.checkErr != nil {
		return e.checkErr
	}
	if e.probeErr != nil {
		return e.probeErr
	}
	if len(e.causes) > 0 {
		return e.causes[0]
	}
	return nil
}

// newWaitError builds a message-first error for composite strategies, whose
// diagnostics combine several already-classified termination reasons. It is
// not used for per-probe causes; those use the structured constructors below.
func newWaitError(message string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	return &waitError{reason: waitReasonMessage, message: message, causes: filtered}
}

func waitContextError(what string, contextErr, checkErr, probeErr error) error {
	return &waitError{
		reason:     waitReasonContext,
		what:       what,
		contextErr: contextErr,
		checkErr:   checkErr,
		probeErr:   probeErr,
	}
}

func waitTimeoutError(what string, timeout time.Duration, checkErr, probeErr error) error {
	return &waitError{
		reason:     waitReasonTimeout,
		what:       what,
		timeout:    timeout,
		contextErr: context.DeadlineExceeded,
		checkErr:   checkErr,
		probeErr:   probeErr,
	}
}

func waitStoppedStateError(what string, state State, checkErr, probeErr error) error {
	return &waitError{
		reason:   waitReasonState,
		what:     what,
		state:    state,
		checkErr: checkErr,
		probeErr: probeErr,
	}
}

func stateFailure(what string, state State, checkErr, stateErr error) error {
	return waitStoppedStateError(what, state, checkErr, stateErr)
}

// targetState uses the richer optional interface when available. Running is a
// compatibility fallback whose false result necessarily means stopped.
func targetState(ctx context.Context, target Target) (State, error) {
	if stateTarget, ok := target.(StateTarget); ok {
		state, err := stateTarget.State(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if permanentProbeError(err) || isPermanentCheckError(err) {
				return StateUnknown, ctxErr
			}
			return StateUnknown, joinNonNil(err, ctxErr)
		}
		if err != nil {
			return StateUnknown, err
		}
		return canonicalState(state), nil
	}
	running, err := target.Running(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			return StateUnknown, ctxErr
		}
		return StateUnknown, joinNonNil(err, ctxErr)
	}
	if err != nil {
		return StateUnknown, err
	}
	if running {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func canonicalState(state State) State {
	switch state {
	case StateCreated, StateRunning, StateStopping, StateStopped, StateRestarting, StatePaused:
		return state
	default:
		return StateUnknown
	}
}

// terminalWaitState reports lifecycle states from which a container cannot
// become ready during startup without external intervention. Created,
// restarting, unknown, and transient inspect errors are retried under the
// startup timeout. Stopping includes backend removal transitions.
func terminalWaitState(state State) bool {
	return state == StateStopping || state == StateStopped || state == StatePaused
}

func waitContextTerminationError(callerCtx, waitCtx context.Context, what string, timeout time.Duration, checkErr, probeErr error) error {
	if err := callerCtx.Err(); err != nil {
		return waitContextError(what, err, checkErr, probeErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return waitTimeoutError(what, timeout, checkErr, probeErr)
		}
		return waitContextError(what, err, checkErr, probeErr)
	}
	return nil
}

func finalLifecycleError(
	callerCtx, waitCtx context.Context,
	target Target,
	what string,
	timeout, interval time.Duration,
	checkErr, probeErr error,
) error {
	// A successful endpoint/exec/log check can race a short-lived daemon
	// hiccup. The lifecycle check is still a fail-fast guard for a stopped
	// container, but transient probe failures get another attempt while the
	// original wait budget remains. Retries reuse the strategy's poll
	// interval so a transient daemon failure cannot turn into a faster
	// probe loop than the check it follows, and only the latest probe cause
	// is retained so retries cannot grow an error tree.
	lastProbeErr := probeErr
	for {
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			// The probe context is derived from waitCtx, so a done probe
			// context means the caller or startup budget ended. probeCtx.Err()
			// itself is not a cause and is deliberately not retained.
			return waitContextTerminationError(callerCtx, waitCtx, what, timeout, checkErr, lastProbeErr)
		}
		state, probeErr := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			probeErr = joinNonNil(probeErr, probeCtxErr)
		}
		// Retain only non-context branches for diagnostics/timeout wrapping,
		// but keep the raw probeErr for control flow so a pure probe-context
		// deadline still retries instead of falling through as StateUnknown.
		if filtered := nonContextCause(probeErr); filtered != nil {
			lastProbeErr = filtered
		}
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, checkErr, lastProbeErr); terminalErr != nil {
			return terminalErr
		}
		if probeErr != nil {
			if isPermanentCheckError(probeErr) {
				cause := permanentCause(probeErr)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, checkErr, lastProbeErr)
				}
				// Context-only permanent wrapper while the wait is still
				// active: always retry. A prior transient lastProbeErr is
				// retained only for a later timeout diagnostic, not as an
				// immediate failure of an otherwise ready target.
			}
			if err := waitForReconnect(waitCtx, interval); err != nil {
				return waitContextTerminationError(callerCtx, waitCtx, what, timeout, checkErr, lastProbeErr)
			}
			continue
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, checkErr, lastProbeErr)
		}
		if state != StateRunning {
			return fmt.Errorf("%s: final lifecycle state %s; want running", what, state)
		}
		return nil
	}
}

func joinNonNil(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return errors.Join(filtered...)
}

// nonContextCause returns a diagnostic view of err that retains every
// non-context cause and suppresses context cancellation. It returns err
// unchanged when no context cause is present, so ordinary backend failures
// keep their concrete wrapper types and As surface.
//
// The view renders only causes that are already non-context values. It never
// edits a rendered message: context wording is dropped by not retaining the
// branch, not by rewriting text.
func nonContextCause(err error) error {
	if err == nil {
		return nil
	}
	if !carriesContextCause(err) {
		return err
	}
	var set causeSet
	collectNonContext(err, &set)
	if len(set.diagnostics) == 0 && len(set.classifiers) == 0 {
		return nil
	}
	if len(set.classifiers) == 0 && len(set.diagnostics) == 1 {
		return set.diagnostics[0]
	}
	if len(set.diagnostics) == 0 {
		set.diagnostics = append(set.diagnostics, staticError(safeOpaqueText(err)))
	}
	return &retainedCause{diagnostics: set.diagnostics, classifiers: set.classifiers}
}

func carriesContextCause(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isContextSentinel(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

// causeSet separates what a retained cause renders from what it classifies.
// A facade can contribute an Is/As classification without contributing
// context or ambiguous text to the diagnostic.
type causeSet struct {
	diagnostics []error
	classifiers []error
}

// collectNonContext walks err and keeps only non-context information.
//
// Wrappers whose Error() renders their children are transparent: their
// retained children are safe to render. A wrapper that hides its children is
// kept as an opaque classifier, and only its own message is used when that
// message does not itself carry a context phrase. This is a containment
// check, not a rewrite: a hidden child is never exposed and a context-bearing
// message is dropped whole rather than edited.
func collectNonContext(err error, set *causeSet) {
	if err == nil || isContextSentinel(err) {
		return
	}
	// A subtree without a context branch is already safe: keep the whole
	// wrapper so concrete types such as *exec.Error stay available through
	// errors.As, and so sanitizing wrappers keep their rendering.
	if !carriesContextCause(err) {
		set.diagnostics = append(set.diagnostics, err)
		return
	}
	if raw, ok := err.(interface{ UnwrapRaw() error }); ok {
		if redactor := cli.DiagnosticRedactor(err); redactor != nil {
			collectRedactedFacade(err, raw, redactor, set)
			return
		}
	}
	if dns, ok := err.(*net.DNSError); ok {
		if retained := retainDNSErrorCause(dns); retained != nil {
			set.diagnostics = append(set.diagnostics, retained)
			return
		}
	}
	children := unwrapChildren(err)
	if len(children) == 0 {
		if carriesContextCause(err) {
			if hasNonContextClassification(err) {
				set.classifiers = append(set.classifiers, &classifiedCause{original: err})
			}
			return
		}
		set.diagnostics = append(set.diagnostics, err)
		return
	}
	childSet := causeSet{}
	for _, child := range children {
		collectNonContext(child, &childSet)
	}
	if hasCustomClassification(err) {
		set.classifiers = append(set.classifiers, &classifiedCause{original: err})
	}
	if len(childSet.diagnostics) > 0 {
		// A wrapper that does not render its children's text is treated as a
		// sanitizing boundary: its own message is the safe diagnostic.
		if rendersChildren(err, children) {
			set.diagnostics = append(set.diagnostics, childSet.diagnostics...)
		} else {
			set.diagnostics = append(set.diagnostics, staticError(safeOpaqueText(err)))
			set.classifiers = append(set.classifiers, childSet.diagnostics...)
		}
		set.classifiers = append(set.classifiers, childSet.classifiers...)
		return
	}
	// Only context was found below this wrapper. Retain it only when its own
	// classification still describes a non-context failure (for example an
	// ErrTargetNotFound wrapper over a canceled child).
	if carriesContextCause(err) {
		if hasNonContextClassification(err) {
			set.classifiers = append(set.classifiers, &classifiedCause{original: err})
		}
		return
	}
	if text := safeOpaqueText(err); text != "" {
		set.diagnostics = append(set.diagnostics, staticError(text))
	}
	set.classifiers = append(set.classifiers, err)
}

// collectRedactedFacade rebuilds a redacted diagnostic from the facade's raw
// child while keeping the redactor on every cause that is rendered or
// classified. A facade whose raw tree is context-only is not retained: its
// context match must not turn a still-running wait into a cancellation.
func collectRedactedFacade(
	facade error,
	raw interface{ UnwrapRaw() error },
	redactor *cli.Redactor,
	set *causeSet,
) {
	child := raw.UnwrapRaw()
	if child == nil {
		return
	}
	childSet := causeSet{}
	collectNonContext(child, &childSet)
	if len(childSet.diagnostics) == 0 && len(childSet.classifiers) == 0 {
		return
	}
	for _, diagnostic := range childSet.diagnostics {
		set.diagnostics = append(set.diagnostics, cli.WithRedactor(diagnostic, redactor))
	}
	for _, classifier := range childSet.classifiers {
		set.classifiers = append(set.classifiers, cli.WithRedactor(classifier, redactor))
	}
	// The facade itself carries the redacted As surface (for example a
	// *cli.CLIError clone); its Is surface is filtered by classifiedCause.
	set.classifiers = append(set.classifiers, &classifiedCause{original: facade})
}

// retainDNSErrorCause keeps a *net.DNSError that has an independent lookup
// failure after its context-only child is removed. The clone nils UnwrapErr
// structurally; it never rewrites the cached DNS reason. An Err field that
// repeats a context phrase is cleared rather than edited so the retained
// diagnostic cannot include cancellation text.
func retainDNSErrorCause(dns *net.DNSError) error {
	if dns == nil || dns.UnwrapErr == nil || !carriesContextCause(dns.UnwrapErr) {
		return nil
	}
	clone := *dns
	clone.UnwrapErr = nonContextCause(dns.UnwrapErr)
	if containsContextText(clone.Err) {
		clone.Err = ""
	}
	if clone.Err == "" && !clone.IsNotFound && !clone.IsTimeout && !clone.IsTemporary {
		return nil
	}
	return &clone
}

func unwrapChildren(err error) []error {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		return multi.Unwrap()
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		if child := single.Unwrap(); child != nil {
			return []error{child}
		}
	}
	return nil
}

func hasCustomClassification(err error) bool {
	if _, ok := err.(interface{ Is(error) bool }); ok {
		return true
	}
	_, ok := err.(interface{ As(any) bool })
	return ok
}

func hasNonContextClassification(err error) bool {
	for _, target := range []error{
		ErrTargetNotFound,
		ErrContainerNotFound,
		ErrPortNotExposed,
		ErrLogStreamSetup,
		ErrInvalidConfiguration,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func rendersChildren(err error, children []error) bool {
	text := err.Error()
	for _, child := range children {
		if child == nil {
			continue
		}
		childText := child.Error()
		if childText != "" && !strings.Contains(text, childText) {
			return false
		}
	}
	return true
}

func safeOpaqueText(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if text == "" {
		return fmt.Sprintf("%T", err)
	}
	if containsContextText(text) {
		// Drop the whole rendering rather than editing the message. The type
		// name still identifies the failing wrapper.
		return fmt.Sprintf("%T", err)
	}
	return text
}

func containsContextText(text string) bool {
	return strings.Contains(text, context.Canceled.Error()) ||
		strings.Contains(text, context.DeadlineExceeded.Error())
}

// classifiedCause keeps a wrapper's Is/As classification while suppressing
// any context match. It has no Unwrap, so errors.Is cannot reach a discarded
// context child through it.
type classifiedCause struct {
	original error
}

func (e *classifiedCause) Error() string {
	if e == nil {
		return "<nil>"
	}
	return safeOpaqueText(e.original)
}

func (e *classifiedCause) Is(target error) bool {
	if e == nil || e.original == nil || target == nil || carriesContextCause(target) {
		return false
	}
	return errors.Is(e.original, target)
}

func (e *classifiedCause) As(target any) bool {
	if e == nil || e.original == nil {
		return false
	}
	return asWithoutContext(e.original, target)
}

// asWithoutContext runs errors.As on a facade and rejects a matched value
// that would reintroduce a context cause through a discarded child.
func asWithoutContext(err error, target any) bool {
	if err == nil || target == nil {
		return false
	}
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return false
	}
	temporary := reflect.New(value.Elem().Type())
	if !errors.As(err, temporary.Interface()) {
		return false
	}
	matched := temporary.Elem().Interface()
	if matchedErr, ok := matched.(error); ok && carriesContextCause(matchedErr) {
		return false
	}
	value.Elem().Set(temporary.Elem())
	return true
}

// retainedCause is the structured non-context view of a mixed error.
type retainedCause struct {
	diagnostics []error
	classifiers []error
}

func (e *retainedCause) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch len(e.diagnostics) {
	case 0:
		return ""
	case 1:
		return e.diagnostics[0].Error()
	default:
		return errors.Join(e.diagnostics...).Error()
	}
}

func (e *retainedCause) Is(target error) bool {
	if e == nil || target == nil || carriesContextCause(target) {
		return false
	}
	for _, classifier := range e.classifiers {
		if errors.Is(classifier, target) {
			return true
		}
	}
	for _, diagnostic := range e.diagnostics {
		if errors.Is(diagnostic, target) {
			return true
		}
	}
	return false
}

func (e *retainedCause) As(target any) bool {
	if e == nil || target == nil {
		return false
	}
	for _, classifier := range e.classifiers {
		if errors.As(classifier, target) {
			return true
		}
	}
	for _, diagnostic := range e.diagnostics {
		if errors.As(diagnostic, target) {
			return true
		}
	}
	return false
}

// Unwrap exposes only rendered non-context diagnostics. Classifications that
// need the original wrapper are reached through Is/As above.
func (e *retainedCause) Unwrap() []error {
	return e.diagnostics
}

type staticError string

func (e staticError) Error() string { return string(e) }

func wrapWaitCause(what string, cause error, previous ...error) error {
	retained := nonContextCause(cause)
	if retained == nil {
		return nil
	}
	causes := make([]error, 0, len(previous))
	for _, prior := range previous {
		if filtered := nonContextCause(prior); filtered != nil {
			causes = append(causes, filtered)
		}
	}
	return &waitError{
		reason: waitReasonCause,
		what:   what,
		cause:  retained,
		causes: causes,
	}
}

// poll runs check until it succeeds, the container enters a state from
// which startup cannot proceed, or the timeout elapses. Every strategy gets
// the same lifecycle policy: inspect once up front, then at a bounded cadence;
// retain transient check and state errors for final diagnostics.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	if err := o.validate(); err != nil {
		return err
	}
	timeout, interval := o.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// lastErr and lastProbeErr hold structured non-context views. A newer
	// context-only observation never erases an already retained backend
	// cause, and a backend cause never contributes a context match.
	var lastErr, lastProbeErr error
	var lastStateCheck time.Time
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr, lastProbeErr)
	}

	if err := terminationErr(); err != nil {
		return err
	}

	state, err := targetState(waitCtx, target)
	if terminalErr := terminationErr(); terminalErr != nil {
		return terminalErr
	}
	if err != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			cause := permanentCause(err)
			if filtered := nonContextCause(cause); filtered != nil {
				return wrapWaitCause(what, filtered, lastProbeErr)
			}
			// Context-only permanent wrapper: continue polling rather than
			// returning an error that still matches context.Canceled.
		} else if filtered := nonContextCause(err); filtered != nil {
			lastProbeErr = filtered
		}
	} else if terminalWaitState(state) {
		return waitStoppedStateError(what, state, lastErr, lastProbeErr)
	}
	lastStateCheck = time.Now()

	probeState := func() error {
		if time.Since(lastStateCheck) < stateCheckInterval || waitCtx.Err() != nil {
			return nil
		}
		lastStateCheck = time.Now()
		state, err := targetState(waitCtx, target)
		if err != nil {
			if terminalErr := terminationErr(); terminalErr != nil {
				if filtered := nonContextCause(err); filtered != nil {
					lastProbeErr = filtered
				}
				return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr, lastProbeErr)
			}
			if isPermanentCheckError(err) || permanentProbeError(err) {
				cause := permanentCause(err)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, lastErr, lastProbeErr)
				}
				// Context-only permanent wrapper: keep polling.
			} else if filtered := nonContextCause(err); filtered != nil {
				lastProbeErr = filtered
			}
			return nil
		}
		if terminalErr := terminationErr(); terminalErr != nil {
			return terminalErr
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, lastErr, lastProbeErr)
		}
		return nil
	}

	for {
		if err := terminationErr(); err != nil {
			return err
		}
		if o.stateFirst {
			if err := probeState(); err != nil {
				return err
			}
		}

		if err := check(waitCtx); err != nil {
			// Keep the latest non-context check cause even when it races with
			// the context becoming done; the terminal classification must not
			// erase a typed backend error, and a mixed context branch must
			// not survive in a later timeout diagnostic.
			if filtered := nonContextCause(err); filtered != nil {
				lastErr = filtered
			}
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(err) {
				cause := permanentCause(err)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, lastErr, lastProbeErr)
				}
				// Context-only permanent wrapper while the wait is still
				// active: treat as transient and keep polling.
			}
		} else {
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if err := finalLifecycleError(callerCtx, waitCtx, target, what, timeout, interval, lastErr, lastProbeErr); err != nil {
				return err
			}
			return terminationErr()
		}

		if err := terminationErr(); err != nil {
			return err
		}
		if !o.stateFirst {
			if err := probeState(); err != nil {
				return err
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			// No post-deadline probe is started. The caller and startup
			// deadlines are the hard budget for every target call.
			return terminationErrOrTimeout(terminationErr(), what, timeout, lastErr, lastProbeErr)
		case <-timer.C:
		}
	}
}

func terminationErrOrTimeout(err error, what string, timeout time.Duration, checkErr, probeErr error) error {
	if err != nil {
		return err
	}
	return waitTimeoutError(what, timeout, checkErr, probeErr)
}

func permanentCause(err error) error {
	var fatal fatalCheckError
	if errors.As(err, &fatal) && fatal.err != nil {
		return fatal.err
	}
	var fatalPointer *fatalCheckError
	if errors.As(err, &fatalPointer) && fatalPointer != nil && fatalPointer.err != nil {
		return fatalPointer.err
	}
	return err
}

// fatalCheckError wraps a check error that must end the poll
// immediately instead of being retried: the check cannot succeed as
// configured (for example, a CLI launch failure, unknown container, or
// invalid request), so retrying cannot help.
type fatalCheckError struct{ err error }

func (e fatalCheckError) Error() string {
	if e.err == nil {
		return "permanent wait error"
	}
	return e.err.Error()
}
func (e fatalCheckError) Unwrap() error { return e.err }

func permanentProbeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTargetNotFound) {
		return true
	}
	return cli.PermanentStartError(err)
}
