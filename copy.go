package container

import (
	"context"
	"fmt"
	"io"
	"os"
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
	target, unlock, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err = c.runner.Run(qCtx, c.eng.copyToArgs(target, abs, containerPath)...)
	return c.classify(ctx, err)
}

// CopyFileFromContainer copies one file out of the running container
// and returns its content. Close releases the temporary copy.
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error) {
	if err := validateContainerPath(containerPath); err != nil {
		return nil, err
	}
	if filepath.Clean(containerPath) == "/" || strings.HasSuffix(containerPath, "/") {
		return nil, fmt.Errorf("copy file from container %q: cannot copy directory or root as a single file", containerPath)
	}
	dir, err := os.MkdirTemp("", "containergo-cp-")
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, filepath.Base(containerPath))
	target, unlock, err := c.verifiedOperationTarget(ctx)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	defer unlock()
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	if _, _, err := c.runner.Run(qCtx, c.eng.copyFromArgs(target, containerPath, dst)...); err != nil {
		_ = os.RemoveAll(dir)
		return nil, c.classify(ctx, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if info.IsDir() {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("copy file from container %q: target is a directory", containerPath)
	}
	f, err := os.Open(dst)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &tempFileReader{File: f, dir: dir}, nil
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
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("container path %q must be absolute", p)
	}
	if !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return fmt.Errorf("container path %q must be valid UTF-8 without NUL bytes", p)
	}
	return nil
}
