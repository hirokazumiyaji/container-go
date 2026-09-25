package cli

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

// Redactor describes values that must be removed from a diagnostic in
// addition to the structural redaction applied to every CLIError.
type Redactor = diagnostic.Redactor

// NewRedactor creates a redactor for values supplied by a caller.
func NewRedactor(values ...string) *Redactor {
	return diagnostic.NewRedactor(values...)
}

// SanitizeDiagnostic escapes terminal control characters without
// redacting application-defined values.
func SanitizeDiagnostic(s string) string {
	return diagnostic.Sanitize(s)
}

// WithRedactor returns err with a safe diagnostic rendering. The
// original error remains available through errors.Is/errors.As and
// Unwrap, while the returned error's Error method uses the supplied
// redactor.
func WithRedactor(err error, r *Redactor) error {
	if err == nil || r == nil {
		return err
	}
	if cliErr, ok := err.(*CLIError); ok {
		return cliErr.withRedactor(r)
	}
	var already *redactedError
	if errors.As(err, &already) {
		return err
	}
	return &redactedError{err: err, redactor: r}
}

type redactedError struct {
	err      error
	redactor *Redactor
}

func (e *redactedError) Error() string {
	if cliErr, ok := e.err.(*CLIError); ok {
		return cliErr.format(e.redactor)
	}
	return e.redactor.Text(e.err.Error())
}

func (e *redactedError) Unwrap() error { return e.err }

func (e *redactedError) As(target any) bool {
	ptr, ok := target.(**CLIError)
	if !ok {
		return false
	}
	var raw *CLIError
	if !errors.As(e.err, &raw) {
		return false
	}
	*ptr = raw.withRedactor(e.redactor)
	return true
}

func (e *redactedError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.Error())
}
