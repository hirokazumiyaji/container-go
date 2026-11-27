package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

const defaultCopyFilesParallelism = 4

// fileCopyError intentionally retains only a source label rather than the
// whole File, so Content cannot leak through an error rendering.
type fileCopyError struct {
	Source        string
	ContainerPath string
	Err           error
}

// sourceLabel names the source in diagnostics without ever rendering Content.
func (f File) sourceLabel() string {
	if f.Content != nil {
		return "content"
	}
	return f.HostPath
}

func newFileCopyError(f File, err error) *fileCopyError {
	return &fileCopyError{Source: f.sourceLabel(), ContainerPath: f.ContainerPath, Err: err}
}

func (e *fileCopyError) Error() string {
	return fmt.Sprintf("copy %q to %q: %v", e.Source, e.ContainerPath, e.Err)
}

func (e *fileCopyError) Unwrap() error {
	return e.Err
}

func (c *Container) copyFiles(ctx context.Context, files []File) error {
	if len(files) == 0 {
		return nil
	}
	cliCtx, cliCancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	target, err := c.verifiedOperationTarget(cliCtx)
	cliCancel()
	if err != nil {
		return err
	}

	copyOne := func(runCtx context.Context, f File) error {
		if err := c.copyFileWithTarget(runCtx, target, f, true, true); err != nil {
			return newFileCopyError(f, err)
		}
		return nil
	}
	if len(files) == 1 {
		return copyOne(ctx, files[0])
	}
	return copyFilesConcurrently(ctx, files, copyOne)
}

// copyFilesConcurrently applies copyFile to every file with bounded
// parallelism, stopping at the first failure and waiting for the rest before
// returning. A worker that finds the context already cancelled stops without
// starting another copy, so when no copy recorded an error the caller gets the
// context error: a cancellation landing between copies would otherwise look
// like a fully applied copy set.
func copyFilesConcurrently(ctx context.Context, files []File, copyFile func(context.Context, File) error) error {
	concurrency := defaultCopyFilesParallelism
	if len(files) < concurrency {
		concurrency = len(files)
	}

	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)

	jobs := make(chan File, len(files))
	for _, f := range files {
		jobs <- f
	}
	close(jobs)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if copyCtx.Err() != nil {
					return
				}
				if err := copyFile(copyCtx, f); err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// copyOutHooks carries test seams for CopyFileFromContainer. Production
// passes the zero value.
type copyOutHooks struct {
	openFile func(string) (*os.File, error)
}

func (c *Container) copyFileFromContainer(ctx context.Context, containerPath string, hooks copyOutHooks) (rc io.ReadCloser, retErr error) {
	if c.refusesSharedOps() {
		return nil, fmt.Errorf("copy file from container %s: %w", c.id, ErrSharedContainer)
	}
	if err := validateContainerPath(containerPath); err != nil {
		return nil, newValidationErrorWithField("CopyFileFromContainer", "containerPath", containerPath, err)
	}
	// Container paths are POSIX paths even when the client runs on Windows.
	// Normalize with path (not filepath) so equivalent root spellings such as
	// // and /tmp/.. are classified consistently. Keep the raw trailing slash
	// check because path.Clean intentionally removes it.
	requestedPath := containerPath
	containerPath = path.Clean(containerPath)
	base := path.Base(containerPath)
	if containerPath == "/" || base == "/" || base == "." || strings.HasSuffix(requestedPath, "/") {
		return nil, newValidationErrorWithField(
			"CopyFileFromContainer",
			"containerPath",
			requestedPath,
			fmt.Errorf("copy file from container %q: cannot copy directory or root as a single file", requestedPath),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, c.publicError(err)
	}
	if err := c.eng.checkCopyFileFromContainer(); err != nil {
		return nil, c.publicError(err)
	}
	if err := checkCopyFileOpenCapability(); err != nil {
		return nil, c.publicError(err)
	}

	qCtx, cancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	target, err := c.verifiedOperationTarget(qCtx)
	if err != nil {
		return nil, c.publicError(err)
	}
	if err := c.checkCopyFileFromContainerVersion(qCtx); err != nil {
		return nil, c.publicError(err)
	}

	// Use the same verified private staging root as copy-in rather than an
	// untrusted TMPDIR or %TEMP% location.
	root, rootCleanup, err := prepareCopyStagingRoot()
	if err != nil {
		return nil, c.publicError(fmt.Errorf("copy file from container: prepare private staging root: %w", err))
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		return nil, c.publicError(errors.Join(
			fmt.Errorf("copy file from container: create private staging directory: %w", err),
			rootCleanup(),
		))
	}
	cleanup := func() error { return errors.Join(cleanupCopyStagingDir(dir), rootCleanup()) }
	keepDir := false
	defer func() {
		if keepDir {
			return
		}
		if cleanupErr := cleanup(); cleanupErr != nil {
			retErr = errors.Join(retErr, c.publicError(cleanupErr))
		}
	}()
	// The destination name is fixed so container path components can
	// never escape the private directory or become a host path.
	dst := filepath.Join(dir, "payload")
	if err := ctx.Err(); err != nil {
		return nil, c.publicError(err)
	}
	if _, _, err := c.runner.Run(qCtx, c.eng.copyFromArgs(target, containerPath, dst)...); err != nil {
		classified := wrapNotFoundForOperation(c.eng, c.classifyOperation(ctx, err, "cp"), "cp", target, c.id)
		return nil, c.redactError(classified, target, containerPath)
	}
	if err := qCtx.Err(); err != nil {
		return nil, c.publicError(err)
	}

	info, err := lstatCopyFile(dst)
	if err != nil {
		return nil, c.publicError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, c.publicError(copyFileNotRegularError(requestedPath, info.Mode()))
	}
	if info.Size() > MaxCopyFileFromContainerSize {
		return nil, c.publicError(copyFileFromContainerTooLargeError(requestedPath, info.Size()))
	}
	if err := qCtx.Err(); err != nil {
		return nil, c.publicError(err)
	}

	openFile := hooks.openFile
	if openFile == nil {
		openFile = openCopyFile
	}
	f, err := openFile(dst)
	if err != nil {
		// A no-follow open can fail after the path changes to a link
		// or special file. Prefer the same typed error in that case.
		if changed, statErr := os.Lstat(dst); statErr == nil && !changed.Mode().IsRegular() {
			return nil, c.publicError(copyFileNotRegularError(requestedPath, changed.Mode()))
		}
		if errors.Is(err, ErrCopyFileNotRegular) {
			return nil, c.publicError(fmt.Errorf("copy file from container %q: %w", requestedPath, err))
		}
		return nil, c.publicError(err)
	}
	openedInfo, statErr := f.Stat()
	if statErr != nil {
		return nil, errors.Join(c.publicError(statErr), c.publicError(f.Close()))
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, errors.Join(c.publicError(copyFileNotRegularError(requestedPath, openedInfo.Mode())), c.publicError(f.Close()))
	}
	if openedInfo.Size() > MaxCopyFileFromContainerSize {
		return nil, errors.Join(
			c.publicError(copyFileFromContainerTooLargeError(requestedPath, openedInfo.Size())),
			c.publicError(f.Close()),
		)
	}
	// The path can be replaced between Lstat and open without either
	// handle being a link. The inspected metadata is only trustworthy
	// while the opened handle still identifies that same file.
	if !os.SameFile(info, openedInfo) {
		return nil, errors.Join(
			c.publicError(fmt.Errorf("copy file from container %q: %w: %w", requestedPath, errCopyFileFromContainerChanged, ErrCopyFileNotRegular)),
			c.publicError(f.Close()),
		)
	}
	if err := qCtx.Err(); err != nil {
		return nil, errors.Join(c.publicError(err), c.publicError(f.Close()))
	}

	keepDir = true
	return newTempFileReader(f, cleanup, requestedPath, MaxCopyFileFromContainerSize), nil
}

// errCopyFileFromContainerChanged reports that the extracted path no longer
// referred to the file inspected before the no-follow open.
var errCopyFileFromContainerChanged = errors.New("copied container file changed while opening")

func copyFileFromContainerTooLargeError(containerPath string, size int64) error {
	return fmt.Errorf(
		"copy file from container %q: size %d exceeds limit %d: %w",
		containerPath,
		size,
		MaxCopyFileFromContainerSize,
		ErrCopyFileFromContainerTooLarge,
	)
}

func copyFileNotRegularError(containerPath string, mode os.FileMode) error {
	if mode.IsDir() {
		return fmt.Errorf("copy file from container %q: target is a directory: %w", containerPath, ErrCopyFileNotRegular)
	}
	return fmt.Errorf("copy file from container %q: target is not a regular file (mode %s): %w", containerPath, mode.Type(), ErrCopyFileNotRegular)
}

// tempFileReader bounds reads to the copy-out limit even if the extracted
// file grows after its inspected size was accepted, and releases the staging
// tree on Close.
type tempFileReader struct {
	file          *os.File
	cleanup       func() error
	containerPath string
	limit         int64
	remaining     int64
	exceeded      bool
}

func newTempFileReader(file *os.File, cleanup func() error, containerPath string, limit int64) *tempFileReader {
	return &tempFileReader{file: file, cleanup: cleanup, containerPath: containerPath, limit: limit, remaining: limit}
}

func (r *tempFileReader) Read(p []byte) (int, error) {
	if r.exceeded {
		return 0, copyFileFromContainerTooLargeReadError(r.containerPath, r.limit)
	}
	if r.remaining <= 0 {
		// The accepted size may already equal the limit. Probe for one
		// more byte so a grown file fails instead of returning EOF.
		var probe [1]byte
		n, err := r.file.Read(probe[:])
		if n > 0 {
			r.exceeded = true
			return 0, copyFileFromContainerTooLargeReadError(r.containerPath, r.limit)
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.file.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *tempFileReader) Close() error {
	return errors.Join(r.file.Close(), r.cleanup())
}

func copyFileFromContainerTooLargeReadError(containerPath string, limit int64) error {
	return fmt.Errorf("copy file from container %q: read exceeded %d-byte limit: %w", containerPath, limit, ErrCopyFileFromContainerTooLarge)
}
