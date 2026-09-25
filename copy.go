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
		validated := make([]File, len(files))
		for i, f := range files {
			if err := validateContainerPath(f.ContainerPath); err != nil {
				return newValidationErrorWithField("WithFiles", "containerPath", f.ContainerPath, err)
			}
			abs, err := validateHostPath(f.HostPath)
			if err != nil {
				return newValidationErrorWithField("WithFiles", "hostPath", f.HostPath, err)
			}
			f.HostPath = abs
			validated[i] = f
		}
		c.files = append(c.files, validated...)
		return nil
	}
}

// validateHostPath resolves a host path before any backend work and checks
// that the resulting absolute path can be statted. Resolving here also
// prevents a later working-directory change from changing the source.
func validateHostPath(hostPath string) (string, error) {
	if hostPath == "" {
		return "", fmt.Errorf("host path must not be empty")
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return "", fmt.Errorf("resolve host path %q: %w", hostPath, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("host path %q: %w", hostPath, err)
	}
	return abs, nil
}

// CopyToContainer copies a host file or directory into the running
// container.
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error {
	if err := validateContainerPath(containerPath); err != nil {
		return newValidationErrorWithField("CopyToContainer", "containerPath", containerPath, err)
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

// CopyFileFromContainer copies one file out of the running container
// and returns its content. Close releases the temporary copy.
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error) {
	if err := validateContainerPath(containerPath); err != nil {
		return nil, newValidationErrorWithField("CopyFileFromContainer", "containerPath", containerPath, err)
	}
	// Container paths are POSIX paths even when the client runs on Windows.
	// Normalize with path (not filepath) so equivalent root spellings such as
	// // and /tmp/.. are classified consistently. Keep the raw trailing slash
	// check because path.Clean intentionally removes it.
	cleaned := path.Clean(containerPath)
	if cleaned == "/" || strings.HasSuffix(containerPath, "/") {
		return nil, newValidationErrorWithField(
			"CopyFileFromContainer",
			"containerPath",
			containerPath,
			fmt.Errorf("copy file from container %q: cannot copy directory or root as a single file", containerPath),
		)
	}
	dir, err := os.MkdirTemp("", "containergo-cp-")
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, path.Base(containerPath))
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	if _, _, err := c.runner.Run(qCtx, c.eng.copyFromArgs(c.id, containerPath, dst)...); err != nil {
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
