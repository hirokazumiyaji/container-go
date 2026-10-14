package container

import (
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
	var validation *ValidationError
	if errors.As(err, &validation) {
		if validation.Value != nil {
			switch value := validation.Value.(type) {
			case Mount:
				extra = append(extra, value.Source, value.Target, value.arg())
			case string:
				extra = append(extra, value)
			}
		} else if validation.Option == "Run" && validation.Err != nil {
			extra = append(extra, validation.Err.Error())
		}
	}
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
	c.secretsMu.RUnlock()

	var matchers []*cli.Redactor
	if base != nil {
		matchers = append(matchers, base)
	}
	// The handle identity and declared bindings are safe structural context
	// too, but are kept out of the long-lived hashed matcher when possible.
	values := make([]string, 0, len(c.exposed)+len(c.published)+1+len(extra))
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
	var validation *ValidationError
	if errors.As(err, &validation) && validation.Value != nil {
		switch value := validation.Value.(type) {
		case string:
			extra = append(extra, value)
		case Mount:
			extra = append(extra, value.Source, value.Target, value.arg())
		}
	}
	return cli.WithRedactor(err, c.diagnosticRedactor(extra...))
}

func (c *Container) clearDiagnosticSecrets() {
	if c == nil {
		return
	}
	c.secretsMu.Lock()
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
	return newValidationError(field, nil, errors.New(problem))
}

func optionBoundaryError(err error) error {
	var validation *ValidationError
	if errors.As(err, &validation) {
		return err
	}
	return newValidationError("Run", nil, err)
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
