//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import "os/exec"

func prepareReaperCommand(*exec.Cmd) {}
