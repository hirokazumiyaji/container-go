package container

import (
	"errors"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// Public error types keep their redaction method unexported so godoc and API
// compatibility tooling never expose internal/cli.Redactor. The internal
// diagnostic boundary cannot name an unexported method from this package, so
// it asks this provider to build a redacted clone for errors.As.
func init() {
	cli.RegisterRedactedAsProvider(redactedPublicError)
}

func redactedPublicError(err error, target any, r *cli.Redactor) bool {
	switch target.(type) {
	case **ConfigError:
		var value *ConfigError
		if errors.As(err, &value) {
			return value.asRedacted(target, r)
		}
	case **ValidationError:
		var value *ValidationError
		if errors.As(err, &value) {
			return value.asRedacted(target, r)
		}
	case **ExecTerminationError:
		var value *ExecTerminationError
		if errors.As(err, &value) {
			return value.asRedacted(target, r)
		}
	case **CleanupError:
		var value *CleanupError
		if errors.As(err, &value) {
			return value.asRedacted(target, r)
		}
	}
	return false
}
