package container

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package.
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts.
var ErrPortNotExposed = errors.New("port not declared via WithExposedPorts")

// ErrImageNotFound reports that an image is not in the backend's local
// store. Run returns it when the pull policy is PullNever and the image
// is absent.
var ErrImageNotFound = errors.New("image not found in local store")

// ErrImageIdentityUnavailable reports that an image inspect completed
// successfully but did not provide a usable immutable identity (a
// repository-bearing digest or a verified local image ID). It is not
// used for transport, permission, or cancellation failures. Run fails
// closed by default; WithAllowMutableImageTag opts into passing the
// original mutable tag to the backend. For Apple, that option also
// explicitly accepts a caller-supplied name@digest alias as mutable;
// it is never treated as an atomic immutable run address. PullNever also
// rejects an identity-less Apple inspect even when the caller supplied a
// digest, because the backend has not confirmed the local identity.
var ErrImageIdentityUnavailable = errors.New("backend did not report an immutable image identity")

// ErrImageIdentityMismatch reports that image inspect returned an
// identity that does not belong to the requested image. It is never
// downgraded to the mutable-tag fallback.
var ErrImageIdentityMismatch = errors.New("backend reported a different image identity")

// ErrImageIdentityNotLocal reports that a resolved immutable identity
// was successfully checked but is not available from the backend's local
// image store under the selected policy. Apple Container has no run-time
// --pull=never switch; running an unaddressable pinned reference could
// otherwise fetch it, so Run fails before create unless the caller opted
// into the mutable-tag compatibility fallback.
var ErrImageIdentityNotLocal = errors.New("immutable image identity is not available locally under the selected pull policy")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, and Logs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that Terminate refused to delete because
// the live container's creation label no longer matches this handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// isNotFound reports whether a CLI failure means the container does not
// exist. Matching substrings live on each engine (see engine_*.go).
func isNotFound(err error) bool {
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	return appleEngine{}.containerMissing(err) || dockerEngine{}.containerMissing(err)
}

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
