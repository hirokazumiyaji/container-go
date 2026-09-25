//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import (
	"context"
	"os/exec"
)

func prepareReaperCommand(*exec.Cmd) {}

func killReaperCommand(_ context.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
