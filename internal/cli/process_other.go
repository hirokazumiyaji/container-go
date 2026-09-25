//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cli

import (
	"context"
	"os/exec"
)

// Windows and other non-Unix targets do not get the POSIX parent-death
// supervisor. The reaper is already disabled there, and the normal direct
// command/cancellation path remains unchanged.
func commandWithParentDeath(ctx context.Context, binary string, args []string) (*exec.Cmd, func(), error) {
	return exec.CommandContext(ctx, binary, args...), func() {}, nil
}

func configureProcessGroup(*exec.Cmd) {}

type directPlatformProcessTree struct{}

func newProcessTree(*exec.Cmd) (processTree, error) { return directPlatformProcessTree{}, nil }
func (directPlatformProcessTree) terminate(cmd *exec.Cmd) error {
	return killProcessGroup(cmd)
}
func (directPlatformProcessTree) close() {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
