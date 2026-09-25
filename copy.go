package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxCopyToContainerSize is the maximum aggregate number of regular-file
	// bytes copied into the private staging tree.
	MaxCopyToContainerSize int64 = 64 << 20
	// MaxCopyToContainerEntries is the maximum number of filesystem entries,
	// including the source root, visited while building a snapshot.
	MaxCopyToContainerEntries int64 = 10_000
	// MaxCopyToContainerDepth is the maximum directory depth. The source root
	// has depth zero.
	MaxCopyToContainerDepth = 128
	// MaxCopyToContainerMetadataSize bounds the aggregate UTF-8 byte length of
	// the source paths visited while building a snapshot.
	MaxCopyToContainerMetadataSize int64 = 8 << 20
)

// copyStagingTimeout bounds only the private source snapshot. It is a variable
// so focused tests can use a shorter, separate staging budget.
var copyStagingTimeout = 2 * time.Minute

// File is a host regular file copied into the container right after start.
type File struct {
	HostPath      string
	ContainerPath string
}

// WithFiles copies regular files into the container after it starts. Directory
// sources are rejected. Copy failures fail Run and roll the container back.
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

// CopyToContainer copies a host regular file or directory into the running
// container. The source is opened once without following a final link, then
// copied from that same handle tree into private staging before the backend
// CLI runs. Symlinks, reparse points, and special files are rejected.
//
// The snapshot is intentionally metadata-lossy: it preserves names, bytes, and
// Unix permission bits, but not timestamps, ownership, setuid/setgid bits,
// ACLs, extended attributes, alternate data streams, sparse layout, or hardlink
// identity. A directory snapshot also does not preserve a concurrent writer's
// atomicity; the handles bound what each entry can observe.
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error {
	return c.copyToContainer(ctx, hostPath, containerPath, true)
}

func (c *Container) copyToContainer(ctx context.Context, hostPath, containerPath string, allowDirectory bool) (retErr error) {
	if err := validateContainerPath(containerPath); err != nil {
		return err
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}

	stagingCtx, stagingCancel := withDefaultTimeout(ctx, copyStagingTimeout)
	staged, cleanup, err := snapshotCopySource(stagingCtx, abs, allowDirectory)
	stagingCancel()
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	cliCtx, cliCancel := withDefaultTimeout(ctx, queryTimeout)
	_, _, err = c.runner.Run(cliCtx, c.eng.copyToArgs(c.id, staged, containerPath)...)
	cliCancel()
	return c.classify(ctx, err)
}

type copySnapshotLimits struct {
	maxBytes         int64
	maxEntries       int64
	maxDepth         int
	maxMetadataBytes int64
}

var defaultCopySnapshotLimits = copySnapshotLimits{
	maxBytes:         MaxCopyToContainerSize,
	maxEntries:       MaxCopyToContainerEntries,
	maxDepth:         MaxCopyToContainerDepth,
	maxMetadataBytes: MaxCopyToContainerMetadataSize,
}

type copySnapshotState struct {
	limits        copySnapshotLimits
	bytes         int64
	entries       int64
	metadataBytes int64
}

type copySourceOpenFunc func(string) (*os.File, bool, error)
type copySourceOpenAtFunc func(*os.File, string) (*os.File, bool, error)

type copySourceOpeners struct {
	open   copySourceOpenFunc
	openAt copySourceOpenAtFunc
}

var defaultCopySourceOpeners = copySourceOpeners{
	open:   openCopySource,
	openAt: openCopySourceAt,
}

type openedCopySource struct {
	file    *os.File
	info    os.FileInfo
	reparse bool
}

// snapshotCopySource makes a bounded private copy from one opened source
// handle. The path returned to the CLI is never the caller's mutable path.
func snapshotCopySource(ctx context.Context, source string, allowDirectory bool) (string, func() error, error) {
	return snapshotCopySourceWith(ctx, source, allowDirectory, defaultCopySnapshotLimits, defaultCopySourceOpeners)
}

func snapshotCopySourceWith(ctx context.Context, source string, allowDirectory bool, limits copySnapshotLimits, openers copySourceOpeners) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, fmt.Errorf("copy to container: %w", err)
	}
	opened, err := openVerifiedCopySource(source, openers.open)
	if err != nil {
		return "", nil, err
	}

	dir, err := newCopyStagingDir(source)
	if err != nil {
		_ = opened.file.Close()
		return "", nil, err
	}
	cleanup := func() error { return cleanupCopyStagingDir(dir) }
	staged := filepath.Join(dir, copySourceBase(source))
	state := copySnapshotState{limits: limits}
	if err := snapshotCopyEntry(ctx, source, staged, opened, allowDirectory, 0, &state, openers); err != nil {
		return "", nil, errors.Join(err, cleanup())
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

// newCopyStagingDir uses one verified per-user cache root and never falls
// back to the source, working directory, home, or system temp path.
func newCopyStagingDir(source string) (string, error) {
	if filepath.Dir(source) == source {
		return "", unsupportedCopySource(source)
	}
	root, err := prepareCopyStagingRoot()
	if err != nil {
		return "", err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("copy to container: resolve private staging root: %w", err)
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("copy to container: resolve source for staging isolation: %w", err)
	}
	if copyPathWithin(canonicalRoot, canonicalSource) {
		return "", fmt.Errorf("copy to container: private staging root is inside source %q", source)
	}

	dir, err := createCopyStagingDir(canonicalRoot)
	if err != nil {
		return "", fmt.Errorf("copy to container: create private staging directory: %w", err)
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.Join(fmt.Errorf("copy to container: resolve created staging directory: %w", err), cleanupCopyStagingDir(dir))
	}
	if copyPathWithin(canonicalDir, canonicalSource) {
		return "", errors.Join(fmt.Errorf("copy to container: created staging directory is inside source %q", source), cleanupCopyStagingDir(dir))
	}
	return canonicalDir, nil
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

func changedCopySource(path string, cause error) error {
	if cause == nil {
		return fmt.Errorf("copy to container: %w: %s", ErrCopySourceChanged, path)
	}
	return fmt.Errorf("copy to container: %w: %s: %w", ErrCopySourceChanged, path, cause)
}

func tooLargeCopySource(path string) error {
	return fmt.Errorf("copy to container: %w: %s", ErrCopySourceTooLarge, path)
}

func tooManyCopySourceEntries(path string, limit int64) error {
	return fmt.Errorf("copy to container: %w: %s: limit %d", ErrCopySourceTooManyEntries, path, limit)
}

func copySourceTooDeep(path string, limit int) error {
	return fmt.Errorf("copy to container: %w: %s: limit %d", ErrCopySourceTooDeep, path, limit)
}

func copySourceMetadataTooLarge(path string, limit int64) error {
	return fmt.Errorf("copy to container: %w: %s: limit %d", ErrCopySourceMetadataTooLarge, path, limit)
}

func (s *copySnapshotState) enter(source string, depth int) error {
	if depth > s.limits.maxDepth {
		return copySourceTooDeep(source, s.limits.maxDepth)
	}
	if s.entries >= s.limits.maxEntries {
		return tooManyCopySourceEntries(source, s.limits.maxEntries)
	}
	metadata := int64(len(source))
	if metadata < 0 || metadata > s.limits.maxMetadataBytes-s.metadataBytes {
		return copySourceMetadataTooLarge(source, s.limits.maxMetadataBytes)
	}
	s.entries++
	s.metadataBytes += metadata
	return nil
}

func snapshotCopyEntry(ctx context.Context, source, staged string, src *openedCopySource, allowDirectory bool, depth int, state *copySnapshotState, openers copySourceOpeners) error {
	defer func() { _ = src.file.Close() }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	if err := state.enter(source, depth); err != nil {
		return err
	}
	if src.reparse || src.info.Mode()&os.ModeSymlink != 0 {
		return unsupportedCopySource(source)
	}
	switch {
	case src.info.Mode().IsRegular():
		return snapshotCopyFile(ctx, source, staged, src, state)
	case src.info.Mode().IsDir():
		if depth == 0 && !allowDirectory {
			return unsupportedCopySource(source)
		}
		return snapshotCopyDirectory(ctx, source, staged, src, depth, state, openers)
	default:
		return unsupportedCopySource(source)
	}
}

func snapshotCopyFile(ctx context.Context, source, staged string, src *openedCopySource, state *copySnapshotState) error {
	remaining := state.limits.maxBytes - state.bytes
	if src.info.Size() > remaining {
		return tooLargeCopySource(source)
	}

	dst, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("copy to container: create staged file %q: %w", staged, err)
	}
	n, copyErr := copySnapshotBytes(ctx, dst, src.file, remaining)
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
	after, err := src.file.Stat()
	if err != nil {
		_ = dst.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return changedCopySource(source, err)
		}
		return fmt.Errorf("copy to container: recheck source %q: %w", source, err)
	}
	if !sameOpenCopyInfo(src.info, after) {
		_ = dst.Close()
		return changedCopySource(source, nil)
	}
	if err := dst.Chmod(src.info.Mode().Perm()); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy to container: set staged file mode %q: %w", staged, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("copy to container: close staged file %q: %w", staged, err)
	}
	state.bytes += n
	return nil
}

func snapshotCopyDirectory(ctx context.Context, source, staged string, src *openedCopySource, depth int, state *copySnapshotState, openers copySourceOpeners) error {
	if err := os.Mkdir(staged, 0o700); err != nil {
		return fmt.Errorf("copy to container: create staged directory %q: %w", staged, err)
	}
	for {
		entries, readErr := src.file.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("copy to container: %w", err)
			}
			name := entry.Name()
			if name == "" || name == "." || name == ".." {
				return unsupportedCopySource(filepath.Join(source, name))
			}
			childSource := filepath.Join(source, name)
			child, err := openVerifiedCopySourceAt(src.file, childSource, name, openers.openAt)
			if err != nil {
				return err
			}
			if err := snapshotCopyEntry(ctx, childSource, filepath.Join(staged, name), child, true, depth+1, state, openers); err != nil {
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
	after, err := src.file.Stat()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return changedCopySource(source, err)
		}
		return fmt.Errorf("copy to container: recheck source directory %q: %w", source, err)
	}
	if !sameOpenCopyInfo(src.info, after) {
		return changedCopySource(source, nil)
	}
	if err := os.Chmod(staged, src.info.Mode().Perm()); err != nil {
		return fmt.Errorf("copy to container: set staged directory mode %q: %w", staged, err)
	}
	return nil
}

func openVerifiedCopySource(path string, open copySourceOpenFunc) (*openedCopySource, error) {
	file, reparse, err := open(path)
	if err != nil {
		return nil, classifyCopySourceOpenError(path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, changedCopySource(path, err)
		}
		return nil, fmt.Errorf("copy to container: inspect opened source %q: %w", path, err)
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.Mode().IsDir()) {
		_ = file.Close()
		return nil, unsupportedCopySource(path)
	}
	return &openedCopySource{file: file, info: info, reparse: reparse}, nil
}

func openVerifiedCopySourceAt(parent *os.File, path, name string, open copySourceOpenAtFunc) (*openedCopySource, error) {
	file, reparse, err := open(parent, name)
	if err != nil {
		return nil, classifyCopySourceOpenError(path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, changedCopySource(path, err)
		}
		return nil, fmt.Errorf("copy to container: inspect opened source %q: %w", path, err)
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.Mode().IsDir()) {
		_ = file.Close()
		return nil, unsupportedCopySource(path)
	}
	return &openedCopySource{file: file, info: info, reparse: reparse}, nil
}

func classifyCopySourceOpenError(path string, err error) error {
	if isCopySourceLinkError(err) {
		return unsupportedCopySource(path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return changedCopySource(path, err)
	}
	return fmt.Errorf("copy to container: open source %q: %w", path, err)
}

func sameOpenCopyInfo(before, after os.FileInfo) bool {
	return before.Size() == after.Size() &&
		before.Mode() == after.Mode() &&
		before.ModTime().Equal(after.ModTime())
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

func cleanupCopyStagingDir(dir string) error {
	var directories []string
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	})
	if errors.Is(walkErr, fs.ErrNotExist) {
		walkErr = nil
	}

	var cleanupErr error
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Chmod(directories[i], 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("copy to container: make staging directory owner-writable %q: %w", directories[i], err))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("copy to container: remove staging directory %q: %w", dir, err))
	}
	if walkErr != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("copy to container: inspect staging directory %q for cleanup: %w", dir, walkErr))
	}
	return cleanupErr
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
