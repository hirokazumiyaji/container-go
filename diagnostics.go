package container

import (
	"context"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func (c *config) diagnosticValues() []string {
	values := make([]string, 0, len(c.env)+len(c.cmd)+len(c.labels)+len(c.mounts)+len(c.files)+len(c.published)+1)
	for _, value := range c.env {
		values = append(values, value)
	}
	values = append(values, c.cmd...)
	values = append(values, c.entrypoint, c.user, c.workdir, c.network)
	for _, value := range c.labels {
		values = append(values, value)
	}
	for _, mount := range c.mounts {
		values = append(values, mount.Source, mount.Target)
	}
	for _, file := range c.files {
		values = append(values, file.HostPath, file.ContainerPath)
	}
	for _, published := range c.published {
		values = append(values, published.raw)
	}
	values = append(values, c.reuseGroup)
	return values
}

func (c *config) diagnosticRedactor(extra ...string) *cli.Redactor {
	values := append([]string(nil), c.diagnosticSecrets...)
	values = append(values, c.diagnosticValues()...)
	values = append(values, extra...)
	return cli.NewRedactor(values...)
}

func (c *Container) diagnosticRedactor(extra ...string) *cli.Redactor {
	values := append([]string(nil), c.diagnosticSecrets...)
	values = append(values, extra...)
	return cli.NewRedactor(values...)
}

func (c *Container) redactError(err error, extra ...string) error {
	return cli.WithRedactor(err, c.diagnosticRedactor(extra...))
}

func (c *Container) classifyWithSecrets(ctx context.Context, err error, secrets ...string) error {
	return c.redactError(cli.Classify(ctx, c.runner, err, c.eng.probe()), secrets...)
}

func execDiagnosticValues(cfg *execConfig, cmd []string) []string {
	values := make([]string, 0, len(cfg.env)+len(cmd)+2)
	for _, value := range cfg.env {
		values = append(values, value)
	}
	values = append(values, cmd...)
	values = append(values, cfg.user, cfg.workdir)
	return values
}
