package wait

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
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
	// Deliberately node-level: a wrapped context error is classified by the
	// caller's cause set, not treated as a bare sentinel here.
	return err == context.Canceled || err == context.DeadlineExceeded //nolint:errorlint // identity check on the sentinel itself
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
	var dns *net.DNSError
	if errors.As(err, &dns) {
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
