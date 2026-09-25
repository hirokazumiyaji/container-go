package container

import (
	"context"
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

func (c *config) diagnosticValues() []string {
	values := make([]string, 0, len(c.env)+len(c.cmd)+len(c.labels)+len(c.mounts)+len(c.files)+len(c.published)+5)
	values = append(values, c.name, c.entrypoint, c.user, c.workdir, c.network, c.reuseGroup)
	for _, value := range c.env {
		values = append(values, value)
	}
	values = append(values, c.cmd...)
	for _, value := range c.labels {
		values = append(values, value)
	}
	for _, mount := range c.mounts {
		values = append(values, mount.Source, mount.Target, mount.arg())
	}
	for _, file := range c.files {
		values = append(values, file.HostPath, file.ContainerPath)
	}
	for _, exposed := range c.exposed {
		values = append(values, exposed.String())
	}
	for _, published := range c.published {
		values = append(values, published.raw)
	}
	values = append(values, c.waitDiagnosticValues()...)
	return uniqueStrings(values)
}

func (c *config) waitDiagnosticValues() []string {
	values := append([]string(nil), c.waitDiagnosticSecrets...)
	return append(values, wait.DiagnosticValues(c.waitStrategy)...)
}

func (c *config) rememberDiagnosticValues() {
	c.diagnosticSecrets = uniqueStrings(append(c.diagnosticSecrets, c.diagnosticValues()...))
}

func (c *config) diagnosticRedactor(extra ...string) *cli.Redactor {
	values := append([]string(nil), c.diagnosticSecrets...)
	values = append(values, c.diagnosticValues()...)
	values = append(values, extra...)
	return cli.NewHashedContextRedactor(values...)
}

func safePublicError(err error, values ...string) error {
	return cli.WithRedactor(err, cli.NewHashedRedactor(values...))
}

func (c *config) publicError(err error, extra ...string) error {
	return cli.WithRedactor(err, c.diagnosticRedactor(extra...))
}

func newDiagnosticMatcher(values []string) *cli.Redactor {
	return cli.NewHashedContextRedactor(values...)
}

func composeDiagnosticMatchers(matchers ...*cli.Redactor) *cli.Redactor {
	return cli.ComposeRedactors(matchers...)
}

func (c *Container) diagnosticRedactor(extra ...string) *cli.Redactor {
	c.secretsMu.RLock()
	base := c.diagnosticRedactorValue
	var values []string
	if len(c.diagnosticSecrets) > 0 {
		values = append(values, c.diagnosticSecrets...)
	}
	c.secretsMu.RUnlock()

	var matchers []*cli.Redactor
	if base != nil {
		matchers = append(matchers, base)
	}
	if len(values) > 0 {
		matchers = append(matchers, cli.NewHashedContextRedactor(values...))
	}
	// The handle identity and declared bindings are safe structural context
	// too, but are kept out of the long-lived hashed matcher when possible.
	values = append(values, c.id)
	for _, exposed := range c.exposed {
		values = append(values, exposed.String())
	}
	for _, published := range c.published {
		values = append(values, published.raw)
	}
	values = append(values, extra...)
	if len(values) > 0 {
		matchers = append(matchers, cli.NewHashedContextRedactor(values...))
	}
	return composeDiagnosticMatchers(matchers...)
}

func (c *Container) publicError(err error, extra ...string) error {
	return cli.WithRedactor(err, c.diagnosticRedactor(extra...))
}

func (c *Container) clearDiagnosticSecrets() {
	if c == nil {
		return
	}
	c.secretsMu.Lock()
	for i := range c.diagnosticSecrets {
		c.diagnosticSecrets[i] = ""
	}
	c.diagnosticSecrets = nil
	// The matcher is immutable and may be shared with an in-flight error.
	// Dropping the handle reference is sufficient; do not mutate it here.
	c.diagnosticRedactorValue = nil
	c.secretsMu.Unlock()
}

// redactError is retained as the short internal spelling used throughout
// the container lifecycle.
func (c *Container) redactError(err error, extra ...string) error {
	return c.publicError(err, extra...)
}

func (c *Container) classifyWithSecrets(ctx context.Context, err error, secrets ...string) error {
	return c.redactError(cli.Classify(ctx, c.runner, err, c.eng.probe()), secrets...)
}

func execDiagnosticValues(cfg *execConfig, cmd []string) []string {
	values := make([]string, 0, len(cfg.env)+len(cmd)+2)
	for key, value := range cfg.env {
		values = append(values, key, value, key+"="+value)
	}
	values = append(values, cmd...)
	values = append(values, fmt.Sprint(cmd), cfg.user, cfg.workdir)
	return uniqueStrings(values)
}

func invalidOption(field, problem string) error {
	return &ValidationError{Field: field, Problem: problem}
}

func optionBoundaryError(err error) error {
	var validation *ValidationError
	if errors.As(err, &validation) {
		return err
	}
	return &OptionError{cause: err}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// rollbackError preserves both the readiness cause and a cleanup failure.
// It is intentionally a typed multi-error rather than a %v interpolation so
// errors.Is/As continue to work after a failed rollback.
type rollbackError struct {
	cause   error
	cleanup error
	id      string
}

func (e *rollbackError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v (container %s left behind: %v)", e.cause, e.id, e.cleanup)
}

func (e *rollbackError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.cause, e.cleanup}
}

func (e *rollbackError) Is(target error) bool {
	return errors.Is(e.cause, target) || errors.Is(e.cleanup, target)
}
