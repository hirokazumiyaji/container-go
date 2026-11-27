package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

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
//
// A directory source keeps the backend cp convention: when containerPath is
// an existing directory, the source directory is copied under its basename.
// Use WithFiles when the contents should land directly in the destination.
//
// Use CopyContentToContainer for an in-memory regular file.
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error {
	if c.refusesSharedOps() {
		return fmt.Errorf("copy to container %s: %w", c.id, ErrSharedContainer)
	}
	if err := c.copyToContainer(ctx, hostPath, containerPath, true); err != nil {
		// Validation errors intentionally echo the rejected argument as their
		// Value; keep it for callers that switch on it.
		var validation *ValidationError
		if errors.As(err, &validation) {
			return err
		}
		return c.redactError(err, hostPath, containerPath)
	}
	return nil
}

// CopyContentToContainer copies content into the running container as one
// regular file. The bytes are written to the same private per-user staging tree
// used by CopyToContainer before the backend CLI runs, and that tree is removed
// on success, failure, and cancellation. mode sets the staged file's Unix
// permission bits; zero selects 0600. A zero mode therefore cannot request mode
// 0000. Non-permission bits are rejected with a ValidationError. Windows cannot
// represent arbitrary Unix bits and maps the read-only attribute only.
//
// Empty content is valid. Content is never placed in argv, errors, or
// diagnostics. The source bytes are bounded by MaxCopyToContainerSize, and the
// caller must not modify content while the call is in progress.
func (c *Container) CopyContentToContainer(ctx context.Context, content []byte, containerPath string, mode fs.FileMode) error {
	if c.refusesSharedOps() {
		return fmt.Errorf("copy content to container %s: %w", c.id, ErrSharedContainer)
	}
	if err := c.copyContentToContainer(ctx, content, containerPath, mode); err != nil {
		var validation *ValidationError
		if errors.As(err, &validation) {
			return err
		}
		// Content is deliberately not passed to redactError: the copy path
		// never renders it into an error, and hashing caller content would
		// retain a potentially large secret in the matcher.
		return c.redactError(err, containerPath)
	}
	return nil
}

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

func (c *Container) copyToContainer(ctx context.Context, hostPath, containerPath string, allowDirectory bool) error {
	if err := validateContainerPath(containerPath); err != nil {
		return newValidationErrorWithField("CopyToContainer", "containerPath", containerPath, validationCause(err))
	}
	abs, err := resolveHostPath(hostPath)
	if err != nil {
		return newValidationErrorWithField("CopyToContainer", "hostPath", hostPath, err)
	}
	// CopyToContainer defers a final link or special file to the snapshot,
	// where it is ErrCopySourceUnsupported, and treats a vanished source as
	// ErrCopySourceChanged rather than a validation error. Check only
	// existence here, without following a final link.
	if err := copySourcePathPresent(abs); err != nil {
		return err
	}

	cliCtx, cliCancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cliCancel()
	target, err := c.verifiedOperationTarget(cliCtx)
	if err != nil {
		return err
	}
	return c.copyFileWithTarget(ctx, target, File{HostPath: abs, ContainerPath: containerPath}, allowDirectory, false)
}

func (c *Container) copyContentToContainer(ctx context.Context, content []byte, containerPath string, mode fs.FileMode) error {
	if err := validateContainerPath(containerPath); err != nil {
		return newValidationErrorWithField("CopyContentToContainer", "containerPath", containerPath, validationCause(err))
	}
	if err := validateCopyMode("CopyContentToContainer", mode); err != nil {
		return err
	}
	if err := validateCopyContentSize("CopyContentToContainer", len(content)); err != nil {
		return err
	}
	cliCtx, cliCancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cliCancel()
	target, err := c.verifiedOperationTarget(cliCtx)
	if err != nil {
		return err
	}
	// A nil byte slice is valid empty content for this content-only API;
	// File uses nil to mean Content is not set, so make the empty source
	// explicit before it reaches the shared copy path.
	if content == nil {
		content = []byte{}
	}
	return c.copyFileWithTarget(ctx, target, File{Content: content, ContainerPath: containerPath, Mode: mode}, false, false)
}

func (c *Container) copyFileWithTarget(ctx context.Context, target string, f File, allowDirectory, directoryContents bool) (retErr error) {
	if err := validateContainerPath(f.ContainerPath); err != nil {
		return newValidationErrorWithField("CopyToContainer", "containerPath", f.ContainerPath, err)
	}

	var hostPath string
	if f.Content == nil {
		abs, err := filepath.Abs(f.HostPath)
		if err != nil {
			return fmt.Errorf("copy to container: %w", err)
		}
		hostPath = abs
	}

	stagingCtx, stagingCancel := withDefaultTimeout(ctx, copyStagingTimeout)
	var (
		staged  string
		cleanup func() error
		err     error
	)
	if f.Content != nil {
		staged, cleanup, err = stageCopyContent(stagingCtx, f.Content, f.Mode)
	} else {
		staged, cleanup, err = snapshotCopySource(stagingCtx, hostPath, copySourceOptions{allowDirectory: allowDirectory, mode: f.Mode})
	}
	stagingCancel()
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	source := staged
	if directoryContents {
		info, err := os.Stat(staged)
		if err != nil {
			return fmt.Errorf("copy to container: inspect staged source: %w", err)
		}
		if info.IsDir() {
			// A trailing "/." makes cp copy the directory's contents into
			// ContainerPath instead of nesting the source directory under it.
			// This keeps repeated WithReuse attaches from stacking a new
			// basename level at the destination.
			source += string(filepath.Separator) + "."
		}
	}
	return c.runCopyCommand(ctx, target, source, f.ContainerPath)
}

func (c *Container) runCopyCommand(ctx context.Context, target, source, containerPath string) error {
	cliCtx, cliCancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cliCancel()
	_, _, err := c.runner.Run(cliCtx, c.eng.copyToArgs(target, source, containerPath)...)
	if err != nil {
		classified := c.classifyOperation(ctx, err, "cp")
		return wrapNotFoundForOperation(c.eng, classified, "cp", target, c.id)
	}
	return nil
}

// checkCopyFileFromContainerVersion verifies the Docker client/server copy-out
// version once per handle and, through the Client metadata cache, once per
// target. The handle is bound to one target, so a successful answer cannot
// describe a different daemon. Failures are not recorded and a later copy
// retries them. Concurrent copies share the check through copyVersionMu; a
// waiting caller's context still bounds the wait.
func (c *Container) checkCopyFileFromContainerVersion(ctx context.Context, copyOut engineCopyOut) error {
	if !c.eng.capabilities().copyOutVersionCheck {
		return copyOut.checkCopyFileFromContainerVersion(ctx, c.runner, opTimeouts{query: c.effectiveQueryTimeout()})
	}
	p := c.pack()
	if p.copyVersionOK.Load() {
		return nil
	}
	if err := p.copyVersionMu.Lock(ctx); err != nil {
		return err
	}
	defer p.copyVersionMu.Unlock()
	if p.copyVersionOK.Load() {
		return nil
	}
	if err := dockerCopyFileFromContainerVersionCached(ctx, c.dockerMeta, c.runner, c.eng, opTimeouts{query: c.effectiveQueryTimeout()}); err != nil {
		return err
	}
	p.copyVersionOK.Store(true)
	return nil
}

// CopyFileFromContainer copies one regular file out of the running
// container and returns its content. It is supported by the Docker
// backend when the host can open copied files without following links or
// blocking on special files. Docker copy-out also requires client and
// server versions >=29.7.0. Apple Container, unsupported hosts, and
// unsupported Docker versions return ErrCopyFileFromContainerUnsupported
// before invoking the backend's copy-out command. The copied regular file
// must not exceed MaxCopyFileFromContainerSize. The returned reader provides
// only Read and Close: a read past that limit, for example after the copy
// grows, fails with ErrCopyFileFromContainerTooLarge. Close releases the
// temporary copy and removes its private staging directory.
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error) {
	return c.copyFileFromContainer(ctx, containerPath, copyOutHooks{})
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
	copyOut, ok := c.eng.(engineCopyOut)
	if !ok {
		return nil, c.publicError(unsupportedCopyOut(c.eng))
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
	if err := c.checkCopyFileFromContainerVersion(qCtx, copyOut); err != nil {
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
	if _, _, err := c.runner.Run(qCtx, copyOut.copyFromArgs(target, containerPath, dst)...); err != nil {
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
