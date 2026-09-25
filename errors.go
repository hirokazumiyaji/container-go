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

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, and Logs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that Terminate refused to delete because
// the live container's creation label no longer matches this handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// ErrCopySourceUnsupported reports a host source that is not a regular
// file or directory. CopyToContainer rejects source symlinks and symlink
// entries in source directories rather than copying devices, sockets, or
// FIFOs.
var ErrCopySourceUnsupported = errors.New("copy source must be a regular file or directory")

// ErrCopySourceTooLarge reports a source whose regular-file bytes exceed
// MaxCopyToContainerSize. The limit applies to a directory's total
// snapshot size as well as to a single file.
var ErrCopySourceTooLarge = errors.New("copy source exceeds size limit")

// ErrCopySourceTooManyEntries reports a source tree that exceeds
// MaxCopyToContainerEntries, including entries whose files are empty.
var ErrCopySourceTooManyEntries = errors.New("copy source has too many entries")

// ErrCopySourceTooDeep reports a source tree deeper than
// MaxCopyToContainerDepth.
var ErrCopySourceTooDeep = errors.New("copy source is too deep")

// ErrCopySourceMetadataTooLarge reports source path metadata that exceeds
// MaxCopyToContainerMetadataSize while the snapshot tree is traversed.
var ErrCopySourceMetadataTooLarge = errors.New("copy source metadata exceeds limit")

// ErrCopySourceChanged reports that a source disappeared or its opened
// handle changed while the private snapshot was being built. The triggering
// filesystem or context error remains available through errors.Is/errors.As.
var ErrCopySourceChanged = errors.New("copy source changed while staging")

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
