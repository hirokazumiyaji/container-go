package cli

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

// Redactor describes values that must be removed from a diagnostic in
// addition to the structural redaction applied to every CLIError.
type Redactor = diagnostic.Redactor

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
// error remains available through errors.Is/errors.As and Unwrap, while the
// returned error's Error method uses the supplied redactor. Applying another
// redactor composes it with an existing safe wrapper instead of discarding the
// first context.
func WithRedactor(err error, r *Redactor) error {
	if err == nil {
		return nil
	}
	if r == nil {
		r = NewRedactor()
	}
	r = r.WithForce()
	if existing, ok := err.(*safeError); ok {
		return &safeError{
			err:      existing.err,
			redactor: existing.redactor.Compose(r),
		}
	}
	combined := r
	for _, existing := range redactorsIn(err) {
		combined = existing.Compose(combined)
	}
	return &safeError{err: err, redactor: combined}
}

// safeError deliberately keeps the original error as its Unwrap target. Its
// As method is important: returning the raw *CLIError through the ordinary
// unwrap path would make errors.As hand callers an object whose fields and
// Error method do not carry the public-boundary redaction context.
type safeError struct {
	err      error
	redactor *Redactor
}

func (e *safeError) Error() string {
	if cliErr, ok := e.err.(*CLIError); ok {
		return cliErr.format(e.redactor)
	}
	// A classified or otherwise wrapped error must retain its surrounding
	// context and all probe details. Its nested CLIError.Error already
	// applies structural rules; the outer redactor adds caller context.
	return e.redactor.Text(e.err.Error())
}

func (e *safeError) Unwrap() error { return e.err }

func (e *safeError) Is(target error) bool { return errors.Is(e.err, target) }

func (e *safeError) As(target any) bool {
	return e.as(target, e.redactor)
}

func (e *safeError) AsRedacted(target any, r *diagnostic.Redactor) bool {
	combined := e.redactor.Compose(r)
	for _, existing := range redactorsIn(e.err) {
		combined = existing.Compose(combined)
	}
	return e.as(target, combined)
}

func (e *safeError) as(target any, r *Redactor) bool {
	ptr, ok := target.(**CLIError)
	if !ok {
		return false
	}
	raw, ok := findRawCLIError(e.err)
	if !ok {
		return false
	}
	*ptr = raw.withRedactor(r)
	return true
}

func (e *safeError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}

func redactorsIn(err error) []*Redactor {
	var redactors []*Redactor
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if safe, ok := current.(*safeError); ok {
			redactors = append(redactors, safe.redactor)
		}
		if provider, ok := current.(interface{ DiagnosticRedactor() *diagnostic.Redactor }); ok {
			if redactor := provider.DiagnosticRedactor(); redactor != nil {
				redactors = append(redactors, redactor)
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return redactors
}

// findRawCLIError walks only Unwrap links. Calling errors.As here would
// re-enter safeError.As and can return a redacted clone rather than the raw
// source needed to compose multiple redactors.
func findRawCLIError(err error) (*CLIError, bool) {
	if err == nil {
		return nil, false
	}
	if cliErr, ok := err.(*CLIError); ok {
		return cliErr, true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if raw, ok := findRawCLIError(child); ok {
				return raw, true
			}
		}
		return nil, false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return findRawCLIError(wrapped.Unwrap())
	}
	return nil, false
}
