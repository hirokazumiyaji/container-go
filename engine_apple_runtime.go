package container

import (
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
