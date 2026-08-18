package container

import (
	"errors"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts.
var ErrPortNotExposed = errors.New("port not declared via WithExposedPorts")

// isNotFound reports whether a CLI failure means the container does not
// exist.
func isNotFound(err error) bool {
	var cliErr *cli.CLIError
	return errors.As(err, &cliErr) && strings.Contains(strings.ToLower(cliErr.Stderr), "not found")
}
