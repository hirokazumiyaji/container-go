//go:build darwin || dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package cli

import "errors"

var errStableProcessIdentityUnavailable = errors.New("stable Unix process identity is unavailable")

type processIdentity struct{}

func openProcessIdentity(int) (processIdentity, error) {
	return processIdentity{}, errStableProcessIdentityUnavailable
}

func (processIdentity) active() (bool, error) {
	return false, errStableProcessIdentityUnavailable
}
func (processIdentity) stop() error { return errStableProcessIdentityUnavailable }
func (processIdentity) stopped() (bool, error) {
	return false, errStableProcessIdentityUnavailable
}
func (processIdentity) kill() error          { return errStableProcessIdentityUnavailable }
func (processIdentity) groupID() (int, bool) { return 0, false }
func (processIdentity) close()               {}
func processGroupTerminationSupported() bool { return false }
