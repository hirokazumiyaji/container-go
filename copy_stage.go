package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// copyStagingTimeout bounds only the private source snapshot. It is a variable
// so focused tests can use a shorter, separate staging budget.
var copyStagingTimeout = 2 * time.Minute

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

type copySourceIdentity struct {
	primary   uint64
	secondary uint64
}

type openedCopySource struct {
	file     *os.File
	path     string
	info     os.FileInfo
	identity copySourceIdentity
	reparse  bool
}

// copySourceOptions selects how a source snapshot is built. A zero mode
// preserves a regular file's Unix permission bits; a non-zero mode replaces
// them for the root file only.
type copySourceOptions struct {
	allowDirectory bool
	mode           fs.FileMode
}

// snapshotCopySource makes a bounded private copy from one opened source
// handle. The path returned to the CLI is never the caller's mutable path.
func snapshotCopySource(ctx context.Context, source string, opts copySourceOptions) (string, func() error, error) {
	return snapshotCopySourceWith(ctx, source, opts, defaultCopySnapshotLimits, defaultCopySourceOpeners)
}

func snapshotCopySourceWith(ctx context.Context, source string, opts copySourceOptions, limits copySnapshotLimits, openers copySourceOpeners) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, fmt.Errorf("copy to container: %w", err)
	}
	opened, err := openVerifiedCopySource(source, openers.open)
	if err != nil {
		return "", nil, err
	}
	if err := copySourcePathPresent(source); err != nil {
		_ = opened.file.Close()
		return "", nil, err
	}

	dir, cleanup, err := newCopyStagingDir(opened)
	if err != nil {
		_ = opened.file.Close()
		return "", nil, err
	}
	staged := filepath.Join(dir, copySourceBase(source))
	state := copySnapshotState{limits: limits}
	if err := snapshotCopyEntry(ctx, source, staged, opened, opts.allowDirectory, opts.mode, 0, &state, openers); err != nil {
		return "", nil, errors.Join(err, cleanup())
	}
	return staged, cleanup, nil
}

// stageCopyContent writes one in-memory regular file into the same private
// per-user staging tree used for host-path snapshots. The staged file starts at
// 0600 and is changed to the requested permission bits before the CLI runs.
func stageCopyContent(ctx context.Context, content []byte, mode fs.FileMode) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, fmt.Errorf("copy to container: %w", err)
	}
	if int64(len(content)) > MaxCopyToContainerSize {
		return "", nil, contentTooLargeError(len(content))
	}
	root, rootCleanup, err := prepareCopyStagingRoot()
	if err != nil {
		return "", nil, err
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		return "", nil, errors.Join(
			fmt.Errorf("copy to container: create private staging directory: %w", err),
			rootCleanup(),
		)
	}
	cleanup := func() error { return errors.Join(cleanupCopyStagingDir(dir), rootCleanup()) }

	staged := filepath.Join(dir, "content")
	dst, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, errors.Join(fmt.Errorf("copy to container: create staged content file: %w", err), cleanup())
	}
	if _, err := dst.Write(content); err != nil {
		_ = dst.Close()
		return "", nil, errors.Join(fmt.Errorf("copy to container: write staged content file: %w", err), cleanup())
	}
	if err := ctx.Err(); err != nil {
		_ = dst.Close()
		return "", nil, errors.Join(fmt.Errorf("copy to container: %w", err), cleanup())
	}
	if mode == 0 {
		mode = 0o600
	}
	if err := dst.Chmod(mode); err != nil {
		_ = dst.Close()
		return "", nil, errors.Join(fmt.Errorf("copy to container: set staged content file mode: %w", err), cleanup())
	}
	if err := dst.Close(); err != nil {
		return "", nil, errors.Join(fmt.Errorf("copy to container: close staged content file: %w", err), cleanup())
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
// back to the source, working directory, home, or system temp path. Ancestry
// is checked through opened directory identities, not the caller's path.
func newCopyStagingDir(source *openedCopySource) (string, func() error, error) {
	if source.info.Mode().IsDir() && filepath.Dir(source.path) == source.path {
		return "", nil, unsupportedCopySource(source.path)
	}
	root, rootCleanup, err := prepareCopyStagingRootForSource(source)
	if err != nil {
		return "", nil, err
	}
	if err := ensureCopyStagingOutsideSource(root, source); err != nil {
		return "", nil, errors.Join(err, rootCleanup())
	}

	dir, err := createCopyStagingDir(root)
	if err != nil {
		return "", nil, errors.Join(
			fmt.Errorf("copy to container: create private staging directory: %w", err),
			rootCleanup(),
		)
	}
	if err := ensureCopyStagingOutsideSource(dir, source); err != nil {
		return "", nil, errors.Join(err, cleanupCopyStagingDir(dir), rootCleanup())
	}
	cleanup := func() error { return errors.Join(cleanupCopyStagingDir(dir), rootCleanup()) }
	return dir, cleanup, nil
}

// ensureCopyStagingOutsideSource walks directory handles from the staging
// path toward the filesystem root. It therefore handles case-insensitive
// aliases and a source path that was replaced after it was opened.
func ensureCopyStagingOutsideSource(root string, source *openedCopySource) error {
	if source == nil || !source.info.Mode().IsDir() {
		return nil
	}
	current, reparse, err := openCopySource(root)
	if err != nil {
		return fmt.Errorf("copy to container: open staging root for ancestry check: %w", err)
	}
	defer func() { _ = current.Close() }()
	return ensureOpenCopyDirectoryOutsideSource(current, reparse, source)
}

func ensureOpenCopyDirectoryOutsideSource(current *os.File, reparse bool, source *openedCopySource) error {
	if source == nil || !source.info.Mode().IsDir() {
		return nil
	}
	info, err := current.Stat()
	if err != nil {
		return fmt.Errorf("copy to container: inspect staging directory: %w", err)
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("copy to container: staging path is not a real directory")
	}
	currentIdentity, err := copySourceFileIdentity(current)
	if err != nil {
		return fmt.Errorf("copy to container: identify staging directory: %w", err)
	}
	opened := make([]*os.File, 0, 8)
	defer func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}()
	for {
		if sameCopySourceIdentity(currentIdentity, source.identity) {
			return fmt.Errorf("copy to container: private staging path is inside opened source %q", source.path)
		}
		parent, parentReparse, err := openCopySourceAt(current, "..")
		if err != nil {
			// NtCreateFile("..") from a Windows volume root returns
			// ERROR_INVALID_NAME instead of the same directory handle.
			if isCopyStagingRootParentError(err) {
				return nil
			}
			return fmt.Errorf("copy to container: inspect staging ancestry: %w", err)
		}
		opened = append(opened, parent)
		parentInfo, err := parent.Stat()
		if err != nil {
			return fmt.Errorf("copy to container: inspect staging ancestry: %w", err)
		}
		if parentReparse || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("copy to container: staging ancestry contains a non-directory")
		}
		parentIdentity, err := copySourceFileIdentity(parent)
		if err != nil {
			return fmt.Errorf("copy to container: identify staging ancestry: %w", err)
		}
		if sameCopySourceIdentity(currentIdentity, parentIdentity) {
			return nil
		}
		current = parent
		currentIdentity = parentIdentity
	}
}

// copySourcePathPresent is an existence check only. It never supplies source
// metadata or identity; the opened handle remains authoritative for copying.
func copySourcePathPresent(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return changedCopySource(path, err)
		}
		return fmt.Errorf("copy to container: check source path %q: %w", path, err)
	}
	return nil
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
	if metadata > s.limits.maxMetadataBytes-s.metadataBytes {
		return copySourceMetadataTooLarge(source, s.limits.maxMetadataBytes)
	}
	s.entries++
	s.metadataBytes += metadata
	return nil
}

func snapshotCopyEntry(ctx context.Context, source, staged string, src *openedCopySource, allowDirectory bool, override fs.FileMode, depth int, state *copySnapshotState, openers copySourceOpeners) error {
	defer func() { _ = src.file.Close() }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	if err := state.enter(source, depth); err != nil {
		return err
	}
	var snapshotErr error
	switch {
	case src.info.Mode().IsRegular():
		snapshotErr = snapshotCopyFile(ctx, source, staged, src, state, override)
	case src.info.Mode().IsDir():
		if depth == 0 && !allowDirectory {
			return unsupportedCopySource(source)
		}
		snapshotErr = snapshotCopyDirectory(ctx, source, staged, src, depth, state, openers)
	default:
		return unsupportedCopySource(source)
	}
	if snapshotErr != nil {
		return snapshotErr
	}
	return copySourcePathPresent(src.path)
}

func snapshotCopyFile(ctx context.Context, source, staged string, src *openedCopySource, state *copySnapshotState, override fs.FileMode) error {
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
	stagedMode := src.info.Mode().Perm()
	if override != 0 {
		stagedMode = override.Perm()
	}
	if err := dst.Chmod(stagedMode); err != nil {
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
			if err := snapshotCopyEntry(ctx, childSource, filepath.Join(staged, name), child, true, 0, depth+1, state, openers); err != nil {
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
	return verifiedCopySource(path, file, reparse, err)
}

func openVerifiedCopySourceAt(parent *os.File, path, name string, open copySourceOpenAtFunc) (*openedCopySource, error) {
	file, reparse, err := open(parent, name)
	return verifiedCopySource(path, file, reparse, err)
}

func verifiedCopySource(path string, file *os.File, reparse bool, err error) (*openedCopySource, error) {
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
	identity, err := copySourceFileIdentity(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("copy to container: identify opened source %q: %w", path, err)
	}
	return &openedCopySource{file: file, path: path, info: info, identity: identity, reparse: reparse}, nil
}

func classifyCopySourceOpenError(path string, err error) error {
	if isCopySourceLinkError(err) || isCopySourceUnsupportedOpenError(err) {
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

// copySnapshotBytes copies at most limit+1 bytes and reports
// ErrCopySourceTooLarge when the source grew past the caller's budget. The
// extra byte is what distinguishes an oversized source from one that exactly
// fills the budget.
func copySnapshotBytes(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	n, err := copySnapshotLimited(ctx, dst, src, limit+1)
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, ErrCopySourceTooLarge
	}
	return n, nil
}

// copySnapshotFileBytes keeps the source as an *os.File so ReadFrom can use
// the kernel's copy_file_range path instead of a userspace read/write loop. It
// transfers bounded chunks to keep a cancellation observable and to stop a
// growing file before more than limit+1 bytes reach the staging file.
func copySnapshotFileBytes(ctx context.Context, dst, src *os.File, max int64) (int64, error) {
	var written int64
	for max > 0 {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		chunk := max
		if chunk > copySnapshotReadFromChunk {
			chunk = copySnapshotReadFromChunk
		}
		n, err := dst.ReadFrom(io.LimitReader(src, chunk))
		written += n
		max -= n
		if err != nil {
			return written, err
		}
		// A regular-file transfer shorter than the requested chunk only ends
		// at EOF. Stop here instead of asking ReadFrom again, which would run
		// the generic fallback and allocate a buffer just to confirm EOF.
		if n < chunk {
			break
		}
	}
	return written, nil
}

func copySnapshotLimited(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	if fileDst, ok := dst.(*os.File); ok && copyReadFromSupported {
		if fileSrc, ok := src.(*os.File); ok {
			return copySnapshotFileBytes(ctx, fileDst, fileSrc, max)
		}
	}
	buffer := copySnapshotBufferPool.Get().(*[]byte)
	defer copySnapshotBufferPool.Put(buffer)
	reader := io.LimitReader(copyContextReader{ctx: ctx, reader: src}, max)
	// Hide ReadFrom so the copy uses the pooled buffer instead of allocating
	// a fresh one inside the standard library's generic fallback.
	return io.CopyBuffer(copyWriteOnly{Writer: dst}, reader, *buffer)
}

const (
	copySnapshotBufferSize    = 32 * 1024
	copySnapshotReadFromChunk = 1 << 20
)

var copySnapshotBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, copySnapshotBufferSize)
		return &buffer
	},
}

type copyWriteOnly struct {
	io.Writer
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
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("copy staging: make staging directory owner-writable %q: %w", directories[i], err))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("copy staging: remove staging directory %q: %w", dir, err))
	}
	if walkErr != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("copy staging: inspect staging directory %q for cleanup: %w", dir, walkErr))
	}
	return cleanupErr
}

// noCopyStagingRootCleanup reports success for the shared per-user staging
// root, which must survive an individual copy operation.
func noCopyStagingRootCleanup() error { return nil }
