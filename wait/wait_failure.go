package wait

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

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

// Unwrap exposes every retained child so a diagnostic boundary can render
// each node with the redactor that belongs to it. Returning only the
// termination reason would hide a backend CLIError from the leaf-by-leaf
// redaction and trimming path whenever the wait failed on a context deadline.
func (e *waitError) Unwrap() []error {
	if e == nil {
		return nil
	}
	children := make([]error, 0, 4+len(e.causes))
	children = append(children, e.contextErr, e.checkErr, e.probeErr, e.cause)
	children = append(children, e.causes...)
	filtered := children[:0]
	for _, child := range children {
		if child != nil {
			filtered = append(filtered, child)
		}
	}
	return filtered
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
