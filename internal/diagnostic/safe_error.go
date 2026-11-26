package diagnostic

import (
	"errors"
	"fmt"
	"reflect"
)

// RedactedAsFunc fills target with a redacted clone of err and reports
// whether it matched. It lets a package register support for a public error
// type whose redaction method is intentionally unexported, because an
// unexported method cannot satisfy an interface in another package.
type RedactedAsFunc func(err error, target any, r *Redactor) bool

var redactedAsFuncs []RedactedAsFunc

// RegisterRedactedAs registers an additional redaction provider. It must be
// called during package initialization, before diagnostics are wrapped.
func RegisterRedactedAs(provider RedactedAsFunc) {
	if provider != nil {
		redactedAsFuncs = append(redactedAsFuncs, provider)
	}
}

// RedactedAs reports whether target was populated with a redacted clone. It
// consults registered providers first and then an AsRedacted method, which
// remains available to internal safe wrappers and unexported error types.
func RedactedAs(err error, target any, r *Redactor) bool {
	if err == nil {
		return false
	}
	for _, provider := range redactedAsFuncs {
		if provider(err, target, r) {
			return true
		}
	}
	if provider, ok := err.(interface {
		AsRedacted(any, *Redactor) bool
	}); ok {
		return provider.AsRedacted(target, r)
	}
	return false
}

// DiagnosticRedactorFunc returns the redactor attached to err, if any. It
// serves the same purpose as RedactedAsFunc for public error types whose
// diagnostic context accessor is intentionally unexported.
type DiagnosticRedactorFunc func(err error) *Redactor

var diagnosticRedactorFuncs []DiagnosticRedactorFunc

// RegisterDiagnosticRedactor registers an additional redactor provider. It
// must be called during package initialization.
func RegisterDiagnosticRedactor(provider DiagnosticRedactorFunc) {
	if provider != nil {
		diagnosticRedactorFuncs = append(diagnosticRedactorFuncs, provider)
	}
}

// DiagnosticRedactorOf returns the redactor carried by err, if any. Registered
// providers run before the DiagnosticRedactor method, which remains available
// to internal types because its return type names an internal Redactor.
func DiagnosticRedactorOf(err error) *Redactor {
	if err == nil {
		return nil
	}
	for _, provider := range diagnosticRedactorFuncs {
		if r := provider(err); r != nil {
			return r
		}
	}
	if provider, ok := err.(interface{ DiagnosticRedactor() *Redactor }); ok {
		return provider.DiagnosticRedactor()
	}
	return nil
}

// safeError is kept in this package for the small generic wrapper used by
// callers that only need structural redaction. CLI-specific wrappers live in
// internal/cli so they can intercept errors.As for *CLIError.
type safeError struct {
	err      error
	redactor *Redactor
}

// DiagnosticRedactor exposes the context to higher-level safe wrappers so
// they can compose instead of dropping an earlier wait or public boundary.
func (e *safeError) DiagnosticRedactor() *Redactor {
	if e == nil {
		return nil
	}
	return e.redactor
}

// UnwrapRaw is an explicit opt-in escape hatch. It is intentionally not named
// Unwrap, because a safe wrapper must not let errors.As/errors.Unwrap reach a
// raw diagnostic accidentally.
func (e *safeError) UnwrapRaw() error {
	if e == nil {
		return nil
	}
	return e.err
}

// WithRedactor returns err with a safe diagnostic rendering while retaining
// source identity for errors.Is and explicit UnwrapRaw.
func WithRedactor(err error, r *Redactor) error {
	if err == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	r = r.WithForce()
	if existing, ok := err.(*safeError); ok {
		return &safeError{err: existing.err, redactor: existing.redactor.Compose(r)}
	}
	combined := r
	for _, existing := range redactorsIn(err) {
		combined = existing.Compose(combined)
	}
	return &safeError{err: err, redactor: combined}
}

func redactorsIn(err error) []*Redactor {
	var redactors []*Redactor
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if safe, ok := current.(*safeError); ok {
			if safe.redactor != nil {
				redactors = append(redactors, safe.redactor)
			}
			return
		}
		if provider := DiagnosticRedactorOf(current); provider != nil {
			redactors = append(redactors, provider)
		}
		for _, child := range rawChildren(current) {
			walk(child)
		}
	}
	walk(err)
	return redactors
}

func rawChildren(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return []error{wrapped.Unwrap()}
	}
	if raw, ok := err.(interface{ UnwrapRaw() error }); ok {
		child := raw.UnwrapRaw()
		if child != nil && !sameError(child, err) {
			return []error{child}
		}
	}
	return nil
}

func (e *safeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.redactor.Text(e.err.Error())
}

func (e *safeError) Is(target error) bool {
	return e != nil && errors.Is(e.err, target)
}

func (e *safeError) As(target any) bool {
	if e == nil {
		return false
	}
	return e.as(target, e.redactor)
}

func (e *safeError) AsRedacted(target any, r *Redactor) bool {
	if e == nil {
		return false
	}
	if r == nil {
		r = NewRedactor()
	}
	combined := e.redactor
	if combined == nil {
		combined = r
	} else {
		combined = combined.Compose(r)
	}
	return e.as(target, combined)
}

func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() {
		return false
	}
	if av.Type().Comparable() {
		return av.Interface() == bv.Interface()
	}
	return false
}

func (e *safeError) as(target any, r *Redactor) bool {
	var walk func(error) bool
	walk = func(current error) bool {
		if current == nil {
			return false
		}
		if RedactedAs(current, target, r) {
			return true
		}
		if _, safe := current.(interface{ DiagnosticSafe() }); safe && errors.As(current, target) {
			return true
		}
		for _, child := range rawChildren(current) {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(e.err)
}

func (e *safeError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}
