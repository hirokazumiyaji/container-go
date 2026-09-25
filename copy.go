package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// File is a host file copied into the container right after start.
type File struct {
	HostPath      string
	ContainerPath string
}

// WithFiles copies files into the container after it starts. Copy
// failures fail Run and roll the container back.
func WithFiles(files ...File) Option {
	return func(c *config) error {
		for _, f := range files {
			if err := validateContainerPath(f.ContainerPath); err != nil {
				return err
			}
		}
		c.files = append(c.files, files...)
		return nil
	}
}

// CopyToContainer copies a host file or directory into the running
// container.
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error {
	if err := validateContainerPath(containerPath); err != nil {
		return err
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err = c.runner.Run(qCtx, c.eng.copyToArgs(c.id, abs, containerPath)...)
	return c.classify(ctx, err)
}

// CopyFileFromContainer copies one regular file out of the running
// container and returns its content. It is supported by the Docker
// backend when the host can open copied files without following links or
// blocking on special files. Apple Container, unsupported hosts, and
// Windows Go 1.23 through 1.25 return ErrCopyFileFromContainerUnsupported
// before invoking the copy-out CLI. Close releases the temporary copy.
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error) {
	if err := validateContainerPath(containerPath); err != nil {
		return nil, err
	}
	requestedPath := containerPath
	containerPath = path.Clean(containerPath)
	base := path.Base(containerPath)
	if base == "/" || base == "." || strings.HasSuffix(requestedPath, "/") {
		return nil, fmt.Errorf("copy file from container %q: cannot copy directory or root as a single file", requestedPath)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.eng.checkCopyFileFromContainer(); err != nil {
		return nil, err
	}
	if err := checkCopyFileOpenCapability(); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "containergo-cp-")
	if err != nil {
		return nil, err
	}
	keepDir := false
	defer func() {
		if !keepDir {
			_ = os.RemoveAll(dir)
		}
	}()
	// The destination name is fixed so container path components can
	// never escape the private directory or become a host path.
	dst := filepath.Join(dir, "payload")
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	if _, _, err := c.runner.Run(qCtx, c.eng.copyFromArgs(c.id, containerPath, dst)...); err != nil {
		return nil, c.classify(ctx, err)
	}
	if err := qCtx.Err(); err != nil {
		return nil, err
	}

	info, err := os.Lstat(dst)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, copyFileNotRegularError(requestedPath, info.Mode())
	}
	if err := qCtx.Err(); err != nil {
		return nil, err
	}

	f, err := openCopyFile(dst)
	if err != nil {
		// A no-follow open can fail after the path changes to a link
		// or special file. Prefer the same typed error in that case.
		if changed, statErr := os.Lstat(dst); statErr == nil && !changed.Mode().IsRegular() {
			return nil, copyFileNotRegularError(requestedPath, changed.Mode())
		}
		return nil, err
	}
	openedInfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, statErr
	}
	if !openedInfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, copyFileNotRegularError(requestedPath, openedInfo.Mode())
	}
	if err := qCtx.Err(); err != nil {
		_ = f.Close()
		return nil, err
	}

	keepDir = true
	return &tempFileReader{File: f, dir: dir}, nil
}

func copyFileNotRegularError(containerPath string, mode os.FileMode) error {
	if mode.IsDir() {
		return fmt.Errorf("copy file from container %q: target is a directory: %w", containerPath, ErrCopyFileNotRegular)
	}
	return fmt.Errorf("copy file from container %q: target is not a regular file (mode %s): %w", containerPath, mode.Type(), ErrCopyFileNotRegular)
}

type tempFileReader struct {
	*os.File
	dir string
}

func (r *tempFileReader) Close() error {
	err := r.File.Close()
	_ = os.RemoveAll(r.dir)
	return err
}

// validateContainerPath enforces the invariants the copy protocol
// relies on: absolute, valid UTF-8, and free of NUL bytes.
func validateContainerPath(p string) error {
	if !path.IsAbs(p) {
		return fmt.Errorf("container path %q must be absolute", p)
	}
	if !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return fmt.Errorf("container path %q must be valid UTF-8 without NUL bytes", p)
	}
	return nil
}
