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
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	defaultStartupTimeout      = 60 * time.Second
	defaultPollInterval        = 100 * time.Millisecond
	stateCheckInterval         = time.Second
	lifecycleProbeTimeout      = 5 * time.Second
	finalLifecycleProbeTimeout = 30 * time.Second
)

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

// Strategy waits until a started container is ready for use.
// Implementations must return when ctx is done so composite strategies can
// collect their final causes without extending the caller's budget.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

type options struct {
	startupTimeout time.Duration
	pollInterval   time.Duration
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

// waitError keeps a conventional single context cause for errors.Unwrap
// while exposing additional transient causes through errors.Is/As.
type waitError struct {
	message string
	primary error
	causes  []error
}

func (e *waitError) Error() string { return e.message }
func (e *waitError) Unwrap() error { return e.primary }
func (e *waitError) Is(target error) bool {
	for _, cause := range e.causes {
		if errors.Is(cause, target) {
			return true
		}
	}
	return false
}
func (e *waitError) As(target any) bool {
	for _, cause := range e.causes {
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
	for _, cause := range e.causes {
		if errors.As(cli.WithRedactor(cause, redactor), target) {
			return true
		}
	}
	return false
}

func newWaitError(message string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	var primary error
	if len(filtered) > 0 {
		primary = filtered[0]
	}
	return &waitError{message: message, primary: primary, causes: filtered}
}

func waitContextError(what string, contextErr, lastErr error) error {
	message := fmt.Sprintf("%s: %s", what, contextErr)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, contextErr, lastErr)
}

func waitTimeoutError(what string, timeout time.Duration, lastErr error) error {
	message := fmt.Sprintf("%s: timed out after %v", what, timeout)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, context.DeadlineExceeded, lastErr)
}

func waitStoppedStateError(what string, state State, lastErr error) error {
	message := fmt.Sprintf("%s: container %s while waiting", what, state)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, lastErr)
}

func stateFailure(what string, state State, checkErr, stateErr error) error {
	return waitStoppedStateError(what, state, joinNonNil(checkErr, stateErr))
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

func waitContextTerminationError(callerCtx, waitCtx context.Context, what string, timeout time.Duration, lastErr error) error {
	if err := callerCtx.Err(); err != nil {
		return waitContextError(what, err, lastErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return waitTimeoutError(what, timeout, lastErr)
		}
		return waitContextError(what, err, lastErr)
	}
	return nil
}

func finalLifecycleError(
	callerCtx, waitCtx context.Context,
	target Target,
	what string,
	timeout, interval time.Duration,
	lastErr error,
) error {
	// A successful endpoint/exec/log check can race a short-lived daemon
	// hiccup. The lifecycle check is still a fail-fast guard for a stopped
	// container, but transient probe failures get another attempt while the
	// original wait budget remains. Retries reuse the strategy's poll
	// interval so a transient daemon failure cannot turn into a faster
	// probe loop than the check it follows, and only the latest probe cause
	// is retained so retries cannot grow an error tree.
	var lastProbeErr error
	for {
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			return waitContextTerminationError(
				callerCtx,
				waitCtx,
				what,
				timeout,
				joinNonNil(lastErr, lastProbeErr, err),
			)
		}
		state, probeErr := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			probeErr = joinNonNil(probeErr, probeCtxErr)
		}
		if terminalErr := waitContextTerminationError(
			callerCtx,
			waitCtx,
			what,
			timeout,
			joinNonNil(lastErr, lastProbeErr, probeErr),
		); terminalErr != nil {
			return terminalErr
		}
		if probeErr != nil {
			lastProbeErr = probeErr
			if isPermanentCheckError(probeErr) {
				return wrapWaitCause(what, permanentCause(probeErr), joinNonNil(lastErr, lastProbeErr))
			}
			if err := waitForReconnect(waitCtx, interval); err != nil {
				return waitContextTerminationError(
					callerCtx,
					waitCtx,
					what,
					timeout,
					joinNonNil(lastErr, lastProbeErr),
				)
			}
			continue
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
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

// nonContextCause drops cancellation and deadline errors so a diagnostic
// settle that races the wait budget cannot hide an already-observed
// success. Joined non-context causes, such as a terminal CLIError, remain.
func nonContextCause(err error) error {
	if err == nil {
		return nil
	}
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		inners := unwrapper.Unwrap()
		kept := make([]error, 0, len(inners))
		changed := false
		for _, inner := range inners {
			cause := nonContextCause(inner)
			if cause == nil {
				changed = true
				continue
			}
			if !sameError(cause, inner) {
				changed = true
			}
			kept = append(kept, cause)
		}
		if len(kept) == 0 {
			return preserveEmptyContextWrapper(err)
		}
		joined := joinNonNil(kept...)
		if !changed {
			// Children are unchanged, but the wrapper itself may still
			// contribute a context match via its own Is method.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return replaceUnwrappedCause(err, joined)
			}
			return err
		}
		// Custom wrappers and fmt multi-%w errors keep framing via
		// contextStrippedCause. Plain errors.Join trees are rebuilt from the
		// filtered children so Error() no longer mentions stripped sentinels.
		if keepsClassificationWithoutChildren(err) || errorPkgPath(err) == "fmt" {
			return replaceUnwrappedCause(err, joined)
		}
		return joined
	}
	// Only unwrap a single-error wrapper when the tree contains a context
	// branch that must be stripped. Otherwise keep wrappers such as
	// *net.OpError so callers retain errors.As and operation diagnostics.
	if unwrapper, ok := err.(interface{ Unwrap() error }); ok &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		inner := unwrapper.Unwrap()
		if inner == nil {
			return preserveEmptyContextWrapper(err)
		}
		filtered := nonContextCause(inner)
		if filtered == nil {
			return preserveEmptyContextWrapper(err)
		}
		// Even when the inner value is unchanged, the wrapper may still
		// contribute a context match via its own Is method.
		return replaceUnwrappedCause(err, filtered)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Pure context sentinels have nothing else to retain.
		if err == context.Canceled || err == context.DeadlineExceeded {
			return nil
		}
		// Opaque facades (Is/As without Unwrap), such as cli.WithRedactor,
		// expose the source tree only via UnwrapRaw. Filter that tree so a
		// context-only facade is dropped while a mixed join keeps its
		// non-context surface under the original facade diagnostics.
		if raw, ok := err.(interface{ UnwrapRaw() error }); ok {
			filtered := nonContextCause(raw.UnwrapRaw())
			if filtered == nil {
				return nil
			}
			filtered = retainDiagnosticFacade(err, filtered)
			return &contextStrippedCause{original: err, cause: filtered}
		}
		return nil
	}
	return err
}

// retainDiagnosticFacade reapplies an opaque facade's redactor onto the
// filtered cause so Error/As cannot expose secrets that only that redactor
// knew how to protect.
func retainDiagnosticFacade(facade, filtered error) error {
	type redactorProvider interface {
		DiagnosticRedactor() *cli.Redactor
	}
	if provider, ok := facade.(redactorProvider); ok {
		if r := provider.DiagnosticRedactor(); r != nil {
			return cli.WithRedactor(filtered, r)
		}
	}
	return filtered
}

// preserveEmptyContextWrapper keeps a custom wrapper's type and Is surface
// after every child was a context sentinel. Stdlib join/wrap errors and
// transport wrappers exist only to carry children, so they are dropped once
// nothing remains.
func preserveEmptyContextWrapper(err error) error {
	if !keepsClassificationWithoutChildren(err) {
		return nil
	}
	if isStdlibTransportWrapper(err) {
		return nil
	}
	return &contextStrippedCause{original: err, cause: nil}
}

func keepsClassificationWithoutChildren(err error) bool {
	switch errorPkgPath(err) {
	case "", "errors", "fmt":
		return false
	default:
		return !isStdlibTransportWrapper(err)
	}
}

func isStdlibTransportWrapper(err error) bool {
	switch errorPkgPath(err) {
	case "net", "os", "net/url", "io/fs":
		return true
	default:
		return false
	}
}

func errorPkgPath(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return ""
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath()
}

// sameError reports whether a and b are the same error value without
// panicking on non-comparable dynamic types.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() || va.Type() != vb.Type() {
		return false
	}
	if va.Comparable() {
		return va.Interface() == vb.Interface()
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return va.Pointer() == vb.Pointer()
	default:
		return false
	}
}

// replaceUnwrappedCause keeps wrapper diagnostics after a nested context
// branch was stripped. Typed net.OpError values are cloned; other wrappers
// keep their Error() text and As/classification surface while suppressing
// context sentinel matches.
func replaceUnwrappedCause(err, filtered error) error {
	// Match only the current node. Nested transport wrappers are rebuilt by
	// recursion so outer wrappers keep their diagnostics.
	switch typed := err.(type) {
	case *net.OpError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *os.PathError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *os.SyscallError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *os.LinkError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *url.Error:
		clone := *typed
		clone.Err = filtered
		return &clone
	}
	return &contextStrippedCause{original: err, cause: filtered}
}

// contextStrippedCause retains an original wrapper's diagnostics and As
// surface after nested context branches were removed from the cause chain.
type contextStrippedCause struct {
	original error
	cause    error
}

func (e *contextStrippedCause) Error() string {
	if e == nil || e.original == nil {
		return "<nil>"
	}
	orig := e.original.Error()
	if e.cause == nil {
		// Context-only wrappers still need their discarded child text removed
		// so retained diagnostics do not contradict the filtered Is surface.
		if inners := multiUnwrap(e.original); len(inners) > 0 {
			if cleaned := replaceDiscardedErrorText(orig, inners, nil); cleaned != "" {
				return cleaned
			}
		}
		if innerText := unfilteredInnerErrorText(e.original); innerText != "" {
			if idx := strings.Index(orig, innerText); idx >= 0 {
				cleaned := strings.TrimSpace(strings.Trim(orig[:idx]+orig[idx+len(innerText):], ":;"))
				if cleaned != "" {
					return cleaned
				}
			}
		}
		return orig
	}
	causeText := e.cause.Error()
	// Preserve wrapper framing (for example "follow logs: " or "follow %w failed")
	// while replacing the unfiltered inner rendering so stripped context
	// sentinels do not remain in user-facing diagnostics.
	if innerText := unfilteredInnerErrorText(e.original); innerText != "" {
		if strings.HasSuffix(orig, innerText) {
			return strings.TrimSuffix(orig, innerText) + causeText
		}
		if idx := strings.Index(orig, innerText); idx >= 0 {
			return orig[:idx] + causeText + orig[idx+len(innerText):]
		}
	}
	if inners := multiUnwrap(e.original); len(inners) > 0 {
		if cleaned := replaceDiscardedErrorText(orig, inners, e.cause); cleaned != "" {
			return cleaned
		}
	}
	return causeText
}

func multiUnwrap(err error) []error {
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		return unwrapper.Unwrap()
	}
	return nil
}

// replaceDiscardedErrorText removes or rewrites child renderings independently
// of Unwrap order so indexed %w verbs that reorder children still strip the
// discarded sentinel occurrence and emit each retained child's filtered text.
// When multiple children share identical Error text, positions cannot be
// associated safely without the format string; return "" so callers fall back
// to the filtered cause text.
func replaceDiscardedErrorText(orig string, inners []error, _ error) string {
	phraseCount := make(map[string]int)
	for _, inner := range inners {
		if inner == nil {
			continue
		}
		if phrase := inner.Error(); phrase != "" {
			phraseCount[phrase]++
		}
	}
	for _, n := range phraseCount {
		if n > 1 {
			return ""
		}
	}

	type edit struct {
		start, end int
		repl       string
	}
	occupied := make([]bool, len(orig))
	claim := func(phrase string) (int, bool) {
		for i := 0; i+len(phrase) <= len(orig); i++ {
			if orig[i:i+len(phrase)] != phrase {
				continue
			}
			overlap := false
			for j := i; j < i+len(phrase); j++ {
				if occupied[j] {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			for j := i; j < i+len(phrase); j++ {
				occupied[j] = true
			}
			return i, true
		}
		return 0, false
	}

	var edits []edit
	changed := false
	for _, inner := range inners {
		phrase := ""
		if inner != nil {
			phrase = inner.Error()
		}
		if phrase == "" {
			continue
		}
		start, ok := claim(phrase)
		if !ok {
			continue
		}
		if filtered := nonContextCause(inner); filtered != nil {
			repl := filtered.Error()
			edits = append(edits, edit{start: start, end: start + len(phrase), repl: repl})
			if repl != phrase {
				changed = true
			}
		} else {
			edits = append(edits, edit{start: start, end: start + len(phrase), repl: ""})
			changed = true
		}
	}
	if !changed {
		return ""
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	masked := orig
	for _, e := range edits {
		masked = masked[:e.start] + e.repl + masked[e.end:]
	}
	// Only tidy delimiters left by removals; do not collapse intentional
	// whitespace inside retained backend diagnostics.
	masked = strings.ReplaceAll(masked, ": ;", ":")
	masked = strings.ReplaceAll(masked, "; ;", ";")
	masked = strings.TrimSpace(masked)
	masked = strings.Trim(masked, ":;")
	return strings.TrimSpace(masked)
}
func (e *contextStrippedCause) Unwrap() error { return e.cause }

func (e *contextStrippedCause) Is(target error) bool {
	if errors.Is(e.cause, target) {
		return true
	}
	if errors.Is(target, context.Canceled) || errors.Is(target, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(e.original, target)
}

func (e *contextStrippedCause) As(target any) bool {
	if errors.As(e.cause, target) {
		return true
	}
	// Match the original wrapper's concrete type without recursively searching
	// discarded children that may still carry context sentinels.
	return asExactType(e.original, target)
}

func asExactType(err error, target any) bool {
	if err == nil || target == nil {
		return false
	}
	val := reflect.ValueOf(target)
	if val.Kind() != reflect.Pointer || val.IsNil() {
		return false
	}
	dest := val.Elem()
	errVal := reflect.ValueOf(err)
	if !errVal.IsValid() || !errVal.Type().AssignableTo(dest.Type()) {
		return false
	}
	dest.Set(errVal)
	return true
}

func unfilteredInnerErrorText(err error) string {
	if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
		if inner := unwrapper.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	if unwrapper, ok := err.(interface{ UnwrapRaw() error }); ok {
		if inner := unwrapper.UnwrapRaw(); inner != nil {
			return inner.Error()
		}
	}
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		if inners := unwrapper.Unwrap(); len(inners) > 0 {
			return errors.Join(inners...).Error()
		}
	}
	return ""
}

func wrapWaitCause(what string, cause error, previous ...error) error {
	if cause == nil {
		return nil
	}
	causes := make([]error, 0, len(previous)+1)
	causes = append(causes, cause)
	causes = append(causes, previous...)
	return newWaitError(fmt.Sprintf("%s: %v", what, cause), causes...)
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

	var lastErr, lastProbeErr error
	var lastStateCheck time.Time
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
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
			return wrapWaitCause(what, permanentCause(err), lastProbeErr)
		}
		lastProbeErr = err
	} else if terminalWaitState(state) {
		return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
	}
	lastStateCheck = time.Now()

	for {
		if err := terminationErr(); err != nil {
			return err
		}

		if err := check(waitCtx); err != nil {
			// Keep the latest check cause even when it races with the
			// context becoming done; the terminal classification must not
			// erase a typed backend error.
			lastErr = err
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(err) {
				return wrapWaitCause(what, permanentCause(err), joinNonNil(lastErr, lastProbeErr))
			}
		} else {
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			causes := joinNonNil(lastErr, lastProbeErr)
			if err := finalLifecycleError(callerCtx, waitCtx, target, what, timeout, interval, causes); err != nil {
				return err
			}
			return terminationErr()
		}

		if err := terminationErr(); err != nil {
			return err
		}
		if time.Since(lastStateCheck) >= stateCheckInterval && waitCtx.Err() == nil {
			lastStateCheck = time.Now()
			state, err := targetState(waitCtx, target)
			if err != nil {
				if terminalErr := terminationErr(); terminalErr != nil {
					lastProbeErr = err
					return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
				}
				if isPermanentCheckError(err) || permanentProbeError(err) {
					return wrapWaitCause(what, permanentCause(err), joinNonNil(lastErr, lastProbeErr))
				}
				lastProbeErr = err
			} else if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			} else if terminalWaitState(state) {
				return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			// No post-deadline probe is started. The caller and startup
			// deadlines are the hard budget for every target call.
			return terminationErrOrTimeout(terminationErr(), what, timeout, joinNonNil(lastErr, lastProbeErr))
		case <-timer.C:
		}
	}
}

func terminationErrOrTimeout(err error, what string, timeout time.Duration, lastErr error) error {
	if err != nil {
		return err
	}
	return waitTimeoutError(what, timeout, lastErr)
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
