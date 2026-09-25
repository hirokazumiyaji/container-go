package wait

import "github.com/hirokazumiyaji/container-go/internal/diagnostic"

func safeDiagnosticError(err error, values ...string) error {
	return diagnostic.WithRedactor(err, diagnostic.NewRedactor(values...))
}
