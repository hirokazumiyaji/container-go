package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func (appleEngine) probe() cli.Probe {
	return cli.Probe{
		Args:          []string{"system", "status"},
		Hint:          "Ensure container system service has been started with `container system start`.",
		Binary:        "container",
		Operation:     "system",
		IsUnavailable: appleProbeUnavailable,
	}
}

func appleProbeUnavailable(err error) bool {
	return probeUnavailable(err, "container", "system", appleProbeLivenessText)
}

func appleProbeLivenessText(line string) bool {
	for _, fragment := range []string{
		"xpc connection",
		"container-apiserver",
		"plugins are unavailable",
		"start the container system services",
		"system is not running",
		"system service is not running",
		"apiserver is not running",
		"not registered with launchd",
		"connection refused",
	} {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

// Apple Container's public cp command has no mode that preserves source
// types or prevents it from dereferencing special files, so fail closed.
func (appleEngine) checkCopyFileFromContainer() error {
	return fmt.Errorf(
		"%w: Apple Container cp has no type-preserving/no-follow copy-out mode",
		ErrCopyFileFromContainerUnsupported,
	)
}

func (appleEngine) checkCopyFileFromContainerVersion(context.Context, cli.Runner, opTimeouts) error {
	return appleEngine{}.checkCopyFileFromContainer()
}
