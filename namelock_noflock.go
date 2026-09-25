//go:build aix || solaris

package container

import (
	"errors"
	"os"
)

var errNameLockUnsupported = errors.New("name locks are unsupported on this platform")

func tryLockNameFile(*os.File) error { return errNameLockUnsupported }

func unlockNameFile(*os.File) {}
