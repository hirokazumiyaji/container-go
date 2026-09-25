//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import (
	"context"
	"errors"
	"os/exec"
)

func prepareReaperCommand(*exec.Cmd) {}

func killReaperCommand(_ context.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return errors.New("reaper: process cleanup is unsupported on this platform")
}
