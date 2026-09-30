package cli

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

// Redactor describes values that must be removed from a diagnostic in
// addition to the structural redaction applied to every CLIError.
type Redactor = diagnostic.Redactor

const MaxStreamOverlap = diagnostic.MaxStreamOverlap

// StreamValueFits reports whether a value and its common escaped forms can be
// protected by a bounded streaming redactor.
func StreamValueFits(value string) bool { return diagnostic.StreamValueFits(value) }

// NewRedactor creates a boundary-aware redactor for values supplied by a
// caller.
func NewRedactor(values ...string) *Redactor {
	return diagnostic.NewRedactor(values...)
}

// NewContextRedactor creates a redactor for operation context. Its explicit
// values are replaced even when adjacent to other text, preventing a short
// value from surviving a tail boundary.
func NewContextRedactor(values ...string) *Redactor {
	return diagnostic.NewContextRedactor(values...)
}

// NewHashedRedactor creates a redactor that retains hashes rather than
// caller-provided plaintext after construction.
func NewHashedRedactor(values ...string) *Redactor {
	return diagnostic.NewHashedRedactor(values...)
}

// NewHashedContextRedactor is the force-replacement variant used for
// operation context and handle lifetimes.
func NewHashedContextRedactor(values ...string) *Redactor {
	return diagnostic.NewHashedContextRedactor(values...)
}

// SanitizeDiagnostic escapes terminal control characters without redacting
// application-defined values.
func SanitizeDiagnostic(s string) string { return diagnostic.Sanitize(s) }

// ComposeRedactors combines redactors without mutating any input.
func ComposeRedactors(redactors ...*Redactor) *Redactor {
	return diagnostic.ComposeRedactors(redactors...)
}

// Compose is a short alias for ComposeRedactors.
func Compose(redactors ...*Redactor) *Redactor { return ComposeRedactors(redactors...) }

// WithRedactor returns err with a safe diagnostic rendering. The original
// error remains available through errors.Is/errors.As, while the returned
// error's Error method uses the supplied redactor. It deliberately has no
// ordinary Unwrap method: raw diagnostics are available only through the
// explicit UnwrapRaw escape hatch.
func WithRedactor(err error, r *Redactor) error {
	if err == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	r = r.WithForce()
	if existing, ok := err.(*safeError); ok {
		return existing.withRedactor(r)
	}
	combined := r
	for _, existing := range redactorsIn(err) {
		combined = existing.Compose(combined)
	}
	return &safeError{err: err, redactor: combined}
}

// safeError is the public-boundary error facade. It keeps the source error
// only for Is and explicit UnwrapRaw; ordinary Unwrap traversal would make a
// raw CLI diagnostic reachable again through errors.As.
type safeError struct {
	err      error
	redactor *Redactor
}

func (e *safeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if cliErr, ok := e.err.(*CLIError); ok {
		return cliErr.format(e.redactor)
	}
	return e.redactor.Text(e.err.Error())
}

// UnwrapRaw is the explicit opt-in path to the source error. It is not named
// Unwrap, so errors.Is/errors.As and errors.Unwrap cannot cross this boundary
// accidentally.
func (e *safeError) UnwrapRaw() error {
	if e == nil {
		return nil
	}
	return e.err
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

func (e *safeError) AsRedacted(target any, r *diagnostic.Redactor) bool {
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
	for _, existing := range redactorsIn(e.err) {
		combined = existing.Compose(combined)
	}
	return e.as(target, combined)
}

func (e *safeError) as(target any, r *Redactor) bool {
	if r == nil {
		r = NewRedactor()
	}
	switch ptr := target.(type) {
	case **CLIError:
		raw, ok := findRawCLIError(e.err)
		if !ok {
			return false
		}
		*ptr = raw.withRedactor(r)
		return true
	case **SystemNotRunningError:
		raw, ok := findRawSystemNotRunning(e.err)
		if !ok {
			return false
		}
		*ptr = raw.withRedactor(r)
		return true
	default:
		return asSafeDiagnostic(e.err, target)
	}
}

func (e *safeError) withRedactor(r *Redactor) *safeError {
	if e == nil {
		return nil
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
	return &safeError{err: e.err, redactor: combined}
}

func (e *safeError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}

// DiagnosticRedactor exposes the context to higher-level safe wrappers so
// they can compose instead of dropping an earlier wait or public boundary.
func (e *safeError) DiagnosticRedactor() *Redactor {
	if e == nil {
		return nil
	}
	return e.redactor
}

// withRedactor makes a safe SystemNotRunningError clone. Its children are
// passed through redactErrorValue so Unwrap, OriginalError, and ProbeError
// never expose a raw child after the clone is returned.
func (e *SystemNotRunningError) withRedactor(r *Redactor) *SystemNotRunningError {
	if e == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	raw := e
	if e.rawSystem != nil {
		raw = e.rawSystem
	}
	combined := r
	if e.redactor != nil {
		combined = e.redactor.Compose(r)
	}
	return &SystemNotRunningError{
		hint:      combined.Text(e.hint),
		original:  redactErrorValue(e.original, combined),
		probe:     redactErrorValue(e.probe, combined),
		redactor:  combined,
		rawSystem: raw,
	}
}

func (e *SystemNotRunningError) DiagnosticRedactor() *Redactor {
	if e == nil {
		return nil
	}
	return e.redactor
}

func (e *SystemNotRunningError) AsRedacted(target any, r *diagnostic.Redactor) bool {
	ptr, ok := target.(**SystemNotRunningError)
	if !ok || e == nil {
		return false
	}
	*ptr = e.withRedactor(r)
	return true
}

func (e *CLIError) DiagnosticRedactor() *Redactor {
	if e == nil {
		return nil
	}
	return e.redactor
}

func (e *CLIError) AsRedacted(target any, r *diagnostic.Redactor) bool {
	ptr, ok := target.(**CLIError)
	if !ok || e == nil {
		return false
	}
	*ptr = e.withRedactor(r)
	return true
}

func redactErrorValue(err error, r *Redactor) error {
	if err == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	switch value := err.(type) {
	case *CLIError:
		return value.withRedactor(r)
	case *SystemNotRunningError:
		return value.withRedactor(r)
	case *safeError:
		return value.withRedactor(r)
	default:
		return &safeError{err: err, redactor: r}
	}
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
		if provider, ok := current.(interface{ DiagnosticRedactor() *diagnostic.Redactor }); ok {
			if redactor := provider.DiagnosticRedactor(); redactor != nil {
				redactors = append(redactors, redactor)
			}
		}
		for _, child := range rawChildren(current) {
			walk(child)
		}
	}
	walk(err)
	return redactors
}

func findRawCLIError(err error) (*CLIError, bool) {
	if err == nil {
		return nil, false
	}
	if cliErr, ok := err.(*CLIError); ok {
		return cliErr, true
	}
	for _, child := range rawChildren(err) {
		if raw, ok := findRawCLIError(child); ok {
			return raw, true
		}
	}
	return nil, false
}

func findRawSystemNotRunning(err error) (*SystemNotRunningError, bool) {
	if err == nil {
		return nil, false
	}
	if system, ok := err.(*SystemNotRunningError); ok {
		return system, true
	}
	for _, child := range rawChildren(err) {
		if raw, ok := findRawSystemNotRunning(child); ok {
			return raw, true
		}
	}
	return nil, false
}

// rawChildren is used only while constructing a safe clone. Standard
// errors.As/errors.Is never call it, and safe facades expose it solely via
// UnwrapRaw for explicit callers.
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

// asSafeDiagnostic exposes only explicitly marked value-free concrete types.
// This keeps errors.As useful for ValidationError and OptionError without
// returning arbitrary raw application errors from a safe wrapper.
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

func asSafeDiagnostic(err error, target any) bool {
	if err == nil {
		return false
	}
	if _, safe := err.(interface{ DiagnosticSafe() }); safe {
		return errors.As(err, target)
	}
	for _, child := range rawChildren(err) {
		if asSafeDiagnostic(child, target) {
			return true
		}
	}
	return false
}
