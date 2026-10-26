package wait

import (
	"errors"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ConfigError keeps its redaction method unexported so the public wait API
// never exposes internal/cli.Redactor. The diagnostic boundary asks this
// provider to build a redacted clone for errors.As.
func init() {
	cli.RegisterRedactedAsProvider(redactedWaitError)
}

func redactedWaitError(err error, target any, r *cli.Redactor) bool {
	if _, ok := target.(**ConfigError); !ok {
		return false
	}
	var value *ConfigError
	if errors.As(err, &value) {
		return value.asRedacted(target, r)
	}
	return false
}
