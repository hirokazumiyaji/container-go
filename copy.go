package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxCopyToContainerSize is the maximum number of regular-file bytes
// CopyToContainer snapshots into its private staging directory. For a
// directory, the limit is the sum of all regular files below it.
const MaxCopyToContainerSize int64 = 64 << 20

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
// container. The host source is first copied into a private staging
// directory, so a later change to the source path cannot change what the
// backend CLI reads. Only regular files and directories are accepted;
// symlinks and special files are rejected. For a directory, the staged
// tree keeps the source basename and recursive layout expected by the
// backend CLI.
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error {
	if err := validateContainerPath(containerPath); err != nil {
		return err
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	staged, cleanup, err := snapshotCopySource(qCtx, abs)
	if err != nil {
		return err
	}
	defer cleanup()
	_, _, err = c.runner.Run(qCtx, c.eng.copyToArgs(c.id, staged, containerPath)...)
	return c.classify(ctx, err)
}

type copySnapshotState struct {
	bytes int64
}

// snapshotCopySource makes a bounded, private copy of source. The path
// returned to the CLI is never the caller's path, and cleanup is safe to
// call after the CLI returns (including on errors).
func snapshotCopySource(ctx context.Context, source string) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, fmt.Errorf("copy to container: %w", err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", nil, fmt.Errorf("copy to container: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, unsupportedCopySource(source)
	}
	if !info.Mode().IsRegular() && !info.Mode().IsDir() {
		return "", nil, unsupportedCopySource(source)
	}

	dir, err := newCopyStagingDir(source)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	staged := filepath.Join(dir, copySourceBase(source))
	state := copySnapshotState{}
	if err := snapshotCopyEntry(ctx, source, staged, info, &state); err != nil {
		cleanup()
		return "", nil, err
	}
	return staged, cleanup, nil
}

func copySourceBase(source string) string {
	base := filepath.Base(source)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "source"
	}
	return base
}

// newCopyStagingDir keeps the snapshot outside the source tree so a
// directory walk cannot observe and recursively copy its own staging path.
func newCopyStagingDir(source string) (string, error) {
	if filepath.Dir(source) == source {
		return "", unsupportedCopySource(source)
	}
	candidates := []string{os.TempDir(), filepath.Dir(source)}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	if cache, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, cache)
	}

	canonicalSource := canonicalCopyPath(source)
	seen := make(map[string]struct{}, len(candidates))
	var lastErr error
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		base, err := filepath.Abs(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		canonicalBase := canonicalCopyPath(base)
		if _, ok := seen[canonicalBase]; ok {
			continue
		}
		seen[canonicalBase] = struct{}{}
		if copyPathWithin(canonicalBase, canonicalSource) {
			continue
		}
		dir, err := os.MkdirTemp(base, "containergo-cp-to-")
		if err != nil {
			lastErr = err
			continue
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			lastErr = err
			continue
		}
		canonicalDir := canonicalCopyPath(absDir)
		if copyPathWithin(canonicalDir, canonicalSource) {
			_ = os.RemoveAll(dir)
			continue
		}
		return canonicalDir, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("copy to container: create private staging directory: %w", lastErr)
	}
	return "", fmt.Errorf("copy to container: no private staging directory outside source")
}

func canonicalCopyPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func copyPathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func unsupportedCopySource(path string) error {
	return fmt.Errorf("copy to container: %w: %s", ErrCopySourceUnsupported, path)
}

func changedCopySource(path string) error {
	return fmt.Errorf("copy to container: %w: %s", ErrCopySourceChanged, path)
}

func tooLargeCopySource(path string) error {
	return fmt.Errorf("copy to container: %w: %s", ErrCopySourceTooLarge, path)
}

func snapshotCopyEntry(ctx context.Context, source, staged string, expected os.FileInfo, state *copySnapshotState) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	if expected.Mode()&os.ModeSymlink != 0 {
		return unsupportedCopySource(source)
	}
	switch {
	case expected.Mode().IsRegular():
		return snapshotCopyFile(ctx, source, staged, expected, state)
	case expected.Mode().IsDir():
		return snapshotCopyDirectory(ctx, source, staged, expected, state)
	default:
		return unsupportedCopySource(source)
	}
}

func snapshotCopyFile(ctx context.Context, source, staged string, expected os.FileInfo, state *copySnapshotState) error {
	remaining := MaxCopyToContainerSize - state.bytes
	if expected.Size() > remaining {
		return tooLargeCopySource(source)
	}

	src, actual, err := openVerifiedCopySource(source, expected)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("copy to container: create staged file %q: %w", staged, err)
	}
	n, copyErr := copySnapshotBytes(ctx, dst, src, remaining)
	if copyErr != nil {
		_ = dst.Close()
		if errors.Is(copyErr, ErrCopySourceTooLarge) {
			return tooLargeCopySource(source)
		}
		return fmt.Errorf("copy to container: snapshot %q: %w", source, copyErr)
	}
	if n > remaining {
		_ = dst.Close()
		return tooLargeCopySource(source)
	}
	after, err := src.Stat()
	if err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy to container: recheck source %q: %w", source, err)
	}
	if !sameCopyInfo(actual, after) {
		_ = dst.Close()
		return changedCopySource(source)
	}
	if err := dst.Chmod(actual.Mode().Perm()); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy to container: set staged file mode %q: %w", staged, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("copy to container: close staged file %q: %w", staged, err)
	}
	state.bytes += n
	return nil
}

func snapshotCopyDirectory(ctx context.Context, source, staged string, expected os.FileInfo, state *copySnapshotState) error {
	src, actual, err := openVerifiedCopySource(source, expected)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if err := os.Mkdir(staged, 0o700); err != nil {
		return fmt.Errorf("copy to container: create staged directory %q: %w", staged, err)
	}
	for {
		entries, readErr := src.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("copy to container: %w", err)
			}
			name := entry.Name()
			if name == "" || name == "." || name == ".." {
				return unsupportedCopySource(filepath.Join(source, name))
			}
			childSource := filepath.Join(source, name)
			if entry.Type()&os.ModeSymlink != 0 {
				return unsupportedCopySource(childSource)
			}
			childInfo, err := entry.Info()
			if err != nil {
				return fmt.Errorf("copy to container: inspect %q: %w", childSource, err)
			}
			if childInfo.Mode()&os.ModeSymlink != 0 {
				return unsupportedCopySource(childSource)
			}
			if err := snapshotCopyEntry(ctx, childSource, filepath.Join(staged, name), childInfo, state); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return fmt.Errorf("copy to container: read source directory %q: %w", source, readErr)
		}
	}
	after, err := src.Stat()
	if err != nil {
		return fmt.Errorf("copy to container: recheck source directory %q: %w", source, err)
	}
	if !sameCopyInfo(actual, after) {
		return changedCopySource(source)
	}
	if err := os.Chmod(staged, expected.Mode().Perm()); err != nil {
		return fmt.Errorf("copy to container: set staged directory mode %q: %w", staged, err)
	}
	return nil
}

func openVerifiedCopySource(path string, expected os.FileInfo) (*os.File, os.FileInfo, error) {
	src, err := openCopySource(path)
	if err != nil {
		if current, statErr := os.Lstat(path); statErr == nil {
			if current.Mode()&os.ModeSymlink != 0 ||
				(current.Mode().IsRegular() != expected.Mode().IsRegular() || current.Mode().IsDir() != expected.Mode().IsDir()) {
				return nil, nil, unsupportedCopySource(path)
			}
			if !sameCopyInfo(expected, current) {
				return nil, nil, changedCopySource(path)
			}
		}
		return nil, nil, fmt.Errorf("copy to container: open source %q: %w", path, err)
	}
	actual, err := src.Stat()
	if err != nil {
		_ = src.Close()
		return nil, nil, fmt.Errorf("copy to container: inspect opened source %q: %w", path, err)
	}
	if !sameCopyInfo(expected, actual) {
		_ = src.Close()
		return nil, nil, changedCopySource(path)
	}
	if actual.Mode()&os.ModeSymlink != 0 ||
		(actual.Mode().IsRegular() != expected.Mode().IsRegular() || actual.Mode().IsDir() != expected.Mode().IsDir()) {
		_ = src.Close()
		return nil, nil, changedCopySource(path)
	}
	return src, actual, nil
}

func sameCopyInfo(expected, actual os.FileInfo) bool {
	return os.SameFile(expected, actual) &&
		expected.Size() == actual.Size() &&
		expected.Mode() == actual.Mode() &&
		expected.ModTime().Equal(actual.ModTime())
}

func copySnapshotBytes(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(copyContextReader{ctx: ctx, reader: src}, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, ErrCopySourceTooLarge
	}
	return n, nil
}

type copyContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r copyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
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
