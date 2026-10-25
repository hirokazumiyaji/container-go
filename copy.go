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
	"time"
	"unicode/utf8"
)

const (
	// MaxCopyToContainerSize is the maximum aggregate number of regular-file
	// bytes copied into the private staging tree.
	MaxCopyToContainerSize int64 = 64 << 20
	// MaxCopyFileFromContainerSize is the maximum number of regular-file bytes
	// CopyFileFromContainer accepts from the container. The limit applies to
	// the single copied file and is checked before a reader is returned.
	MaxCopyFileFromContainerSize int64 = 64 << 20
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

// File is a regular file copied into the container right after start. Set
// exactly one of HostPath or Content. CopyToContainer may still accept a
// directory source; WithFiles does not.
type File struct {
	// HostPath is a host filesystem path. WithFiles resolves it when the
	// option is applied.
	HostPath string
	// Content is an in-memory regular file. A non-nil empty slice copies an
	// empty file; nil means Content is not set. Content is never included in
	// errors, argv, or diagnostics.
	Content []byte
	// ContainerPath is the absolute destination path in the container.
	ContainerPath string
	// Mode overrides the Unix permission bits of the private staged copy.
	// Zero preserves a HostPath source's permissions and gives Content a 0600
	// default, so a zero Mode cannot request mode 0000. Only permission bits
	// (0777) are accepted; type, setuid, setgid, and sticky bits are rejected.
	// Windows cannot represent arbitrary Unix bits and maps the read-only
	// attribute only.
	Mode fs.FileMode
}

// WithFiles copies regular files into the container after it starts. Each File
// must set exactly one of HostPath or Content; setting both, or neither, is a
// ValidationError when the option is applied. Host paths are resolved when the
// option is applied. Content is bounded by MaxCopyToContainerSize when the
// option is applied and is read when the copy runs, so the caller must not
// modify it until Run returns. Directory sources are rejected at copy time.
// Copy failures fail Run; an ordinary newly created container is rolled back.
// WithReuse applies the copy to every caller, including callers that attach to
// an existing shared container; an attach copy failure returns an error without
// deleting that shared container.
func WithFiles(files ...File) Option {
	return func(c *config) error {
		validated := make([]File, len(files))
		seen := make(map[string]struct{}, len(c.files)+len(files))
		for _, existing := range c.files {
			seen[path.Clean(existing.ContainerPath)] = struct{}{}
		}
		for i, f := range files {
			if err := validateContainerPath(f.ContainerPath); err != nil {
				return newValidationErrorWithField("WithFiles", "containerPath", f.ContainerPath, err)
			}
			clean := path.Clean(f.ContainerPath)
			if _, exists := seen[clean]; exists {
				return newValidationErrorWithField("WithFiles", "containerPath", f.ContainerPath, fmt.Errorf("duplicate container path %q", f.ContainerPath))
			}
			seen[clean] = struct{}{}
			if err := validateWithFilesSource(f); err != nil {
				return err
			}
			if f.Content == nil {
				abs, err := validateHostPath(f.HostPath)
				if err != nil {
					return newValidationErrorWithField("WithFiles", "hostPath", f.HostPath, err)
				}
				f.HostPath = abs
			}
			validated[i] = f
		}
		c.files = append(c.files, validated...)
		return nil
	}
}

// validateWithFilesSource enforces the source invariants that can be checked
// without touching the host filesystem. It runs before any backend call.
func validateWithFilesSource(f File) error {
	switch {
	case f.HostPath == "" && f.Content == nil:
		// Keep the legacy message for an empty host path so existing
		// callers that match it are not broken by the added Content field.
		return newValidationErrorWithField("WithFiles", "hostPath", f.HostPath,
			errors.New("host path must not be empty"))
	case f.HostPath != "" && f.Content != nil:
		// Content is secret material: never put it in ValidationError.Value.
		return newValidationErrorWithField("WithFiles", "content", nil,
			errors.New("hostPath and content are mutually exclusive"))
	}
	if err := validateCopyMode("WithFiles", f.Mode); err != nil {
		return err
	}
	if f.Content != nil {
		return validateCopyContentSize("WithFiles", len(f.Content))
	}
	return nil
}

// validateCopyMode accepts zero (unspecified) or Unix permission bits. Type,
// setuid, setgid, and sticky bits are not part of the snapshot contract.
func validateCopyMode(option string, mode fs.FileMode) error {
	if mode == 0 {
		return nil
	}
	if mode.Perm() != mode {
		return newValidationErrorWithField(option, "mode", mode,
			fmt.Errorf("mode %#o contains non-permission bits; only Unix permission bits are accepted", uint32(mode)))
	}
	return nil
}

// validateCopyContentSize keeps an in-memory source within the same aggregate
// byte limit as a host-path snapshot.
func validateCopyContentSize(option string, size int) error {
	if int64(size) <= MaxCopyToContainerSize {
		return nil
	}
	return newValidationErrorWithField(option, "content", nil, contentTooLargeError(size))
}

func contentTooLargeError(size int) error {
	return fmt.Errorf("content size %d exceeds MaxCopyToContainerSize (%d): %w", size, MaxCopyToContainerSize, ErrCopySourceTooLarge)
}

// resolveHostPath rejects inputs that cannot be host filesystem paths and
// resolves the rest before any backend work. Resolving here also prevents a
// later working-directory change from changing the source.
func resolveHostPath(hostPath string) (string, error) {
	if hostPath == "" {
		return "", fmt.Errorf("host path must not be empty")
	}
	if !utf8.ValidString(hostPath) || strings.ContainsRune(hostPath, 0) {
		return "", fmt.Errorf("host path must be valid UTF-8 without NUL bytes")
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return "", fmt.Errorf("resolve host path %q: %w", hostPath, err)
	}
	return abs, nil
}

// validateHostPath resolves a host path and checks that the resulting
// absolute path can be statted.
func validateHostPath(hostPath string) (string, error) {
	abs, err := resolveHostPath(hostPath)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("host path %q: %w", hostPath, err)
	}
	return abs, nil
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
		if err := c.copyFileWithTarget(runCtx, target, f, false); err != nil {
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

// validationCause unwraps a lower-level typed validation error so the caller
// can re-wrap it with its own Option and Field without losing the message.
func validationCause(err error) error {
	var validation *ValidationError
	if errors.As(err, &validation) {
		return validation.Err
	}
	return err
}

func (c *Container) copyToContainer(ctx context.Context, hostPath, containerPath string, allowDirectory bool) error {
	if err := validateContainerPath(containerPath); err != nil {
		return newValidationErrorWithField("CopyToContainer", "containerPath", containerPath, validationCause(err))
	}
	abs, err := resolveHostPath(hostPath)
	if err != nil {
		return newValidationErrorWithField("CopyToContainer", "hostPath", hostPath, err)
	}
	// Unlike validateHostPath this must not follow a final link: the
	// snapshot rejects it as unsupported, and a source that vanishes is
	// ErrCopySourceChanged rather than a validation error.
	if err := copySourcePathPresent(abs); err != nil {
		return err
	}

	cliCtx, cliCancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cliCancel()
	target, err := c.verifiedOperationTarget(cliCtx)
	if err != nil {
		return err
	}
	return c.copyFileWithTarget(ctx, target, File{HostPath: abs, ContainerPath: containerPath}, allowDirectory)
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
	return c.copyFileWithTarget(ctx, target, File{Content: content, ContainerPath: containerPath, Mode: mode}, false)
}

func (c *Container) copyFileWithTarget(ctx context.Context, target string, f File, allowDirectory bool) (retErr error) {
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

	return c.runCopyCommand(ctx, target, staged, f.ContainerPath)
}

func (c *Container) runCopyCommand(ctx context.Context, target, staged, containerPath string) error {
	cliCtx, cliCancel := withDefaultTimeout(ctx, c.effectiveQueryTimeout())
	defer cliCancel()
	_, _, err := c.runner.Run(cliCtx, c.eng.copyToArgs(target, staged, containerPath)...)
	if err != nil {
		classified := c.classifyOperation(ctx, err, "cp")
		return wrapNotFoundForOperation(c.eng, classified, "cp", target, c.id)
	}
	return nil
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

	dir, err := newCopyStagingDir(opened)
	if err != nil {
		_ = opened.file.Close()
		return "", nil, err
	}
	cleanup := func() error { return cleanupCopyStagingDir(dir) }
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
	root, err := prepareCopyStagingRoot()
	if err != nil {
		return "", nil, err
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		return "", nil, fmt.Errorf("copy to container: create private staging directory: %w", err)
	}
	cleanup := func() error { return cleanupCopyStagingDir(dir) }

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
func newCopyStagingDir(source *openedCopySource) (string, error) {
	if source.info.Mode().IsDir() && filepath.Dir(source.path) == source.path {
		return "", unsupportedCopySource(source.path)
	}
	root, err := prepareCopyStagingRootForSource(source)
	if err != nil {
		return "", err
	}
	if err := ensureCopyStagingOutsideSource(root, source); err != nil {
		return "", err
	}

	dir, err := createCopyStagingDir(root)
	if err != nil {
		return "", fmt.Errorf("copy to container: create private staging directory: %w", err)
	}
	if err := ensureCopyStagingOutsideSource(dir, source); err != nil {
		return "", errors.Join(err, cleanupCopyStagingDir(dir))
	}
	return dir, nil
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

// checkCopyFileFromContainerVersion verifies the Docker client/server copy-out
// version once per handle. The handle is bound to one target, so a successful
// answer cannot describe a different daemon. Failures are not recorded and a
// later copy retries them. Concurrent copies share the check through
// copyVersionMu; a waiting caller's context still bounds the wait.
func (c *Container) checkCopyFileFromContainerVersion(ctx context.Context) error {
	if c.eng.name() != "docker" {
		return c.eng.checkCopyFileFromContainerVersion(ctx, c.runner, opTimeouts{query: c.effectiveQueryTimeout()})
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
	if err := c.eng.checkCopyFileFromContainerVersion(ctx, c.runner, opTimeouts{query: c.effectiveQueryTimeout()}); err != nil {
		return err
	}
	p.copyVersionOK.Store(true)
	return nil
}

// CopyFileFromContainer copies one regular file out of the running
// container and returns its content. It is supported by the Docker
// backend when the host can open copied files without following links or
// blocking on special files. Docker copy-out also requires client and
// server versions >=29.7.0. Apple Container, unsupported hosts, unsupported
// Docker versions, and Windows Go 1.23 through 1.25 return
// ErrCopyFileFromContainerUnsupported before invoking the backend's
// copy-out command. The copied regular file must not exceed
// MaxCopyFileFromContainerSize. Close releases the temporary copy and
// removes its private staging directory.
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (rc io.ReadCloser, retErr error) {
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
	root, err := prepareCopyStagingRoot()
	if err != nil {
		return nil, c.publicError(fmt.Errorf("copy file from container: prepare private staging root: %w", err))
	}
	dir, err := createCopyStagingDir(root)
	if err != nil {
		return nil, c.publicError(fmt.Errorf("copy file from container: create private staging directory: %w", err))
	}
	keepDir := false
	defer func() {
		if keepDir {
			return
		}
		if cleanupErr := cleanupCopyStagingDir(dir); cleanupErr != nil {
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

	info, err := os.Lstat(dst)
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

	f, err := openCopyFile(dst)
	if err != nil {
		// A no-follow open can fail after the path changes to a link
		// or special file. Prefer the same typed error in that case.
		if changed, statErr := os.Lstat(dst); statErr == nil && !changed.Mode().IsRegular() {
			return nil, c.publicError(copyFileNotRegularError(requestedPath, changed.Mode()))
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
	if err := qCtx.Err(); err != nil {
		return nil, errors.Join(c.publicError(err), c.publicError(f.Close()))
	}

	keepDir = true
	return &tempFileReader{File: f, dir: dir}, nil
}

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

type tempFileReader struct {
	*os.File
	dir string
}

func (r *tempFileReader) Close() error {
	return errors.Join(r.File.Close(), cleanupCopyStagingDir(r.dir))
}

// validateContainerPath enforces the invariants the copy protocol
// relies on: absolute POSIX paths with '/' separators, valid UTF-8,
// and no NUL or backslash characters.
func validateContainerPath(p string) error {
	if !path.IsAbs(p) {
		return fmt.Errorf("container path %q must be absolute", p)
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("container path %q must use '/' as the path separator", p)
	}
	if !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return invalidOption("container path", "must be valid UTF-8 without NUL bytes")
	}
	return nil
}
