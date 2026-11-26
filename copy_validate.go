package container

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// File is a host file or directory copied into the container right after
// start. Set exactly one of HostPath or Content. WithFiles accepts a regular
// file or a directory as HostPath; Content always creates one regular file.
type File struct {
	// HostPath is a host filesystem path. WithFiles resolves it when the
	// option is applied. A directory is copied as a tree, with symlinks and
	// special files anywhere in it rejected before the backend copy runs.
	HostPath string
	// Content is an in-memory regular file. A non-nil empty slice copies an
	// empty file; nil means Content is not set. Content is never included in
	// errors, argv, or diagnostics.
	Content []byte
	// ContainerPath is the absolute destination path in the container. For a
	// directory HostPath, the directory's contents are copied into this path;
	// the source directory itself is not nested under it.
	ContainerPath string
	// Mode overrides the Unix permission bits of the private staged copy.
	// Zero preserves a HostPath source's permissions and gives Content a 0600
	// default, so a zero Mode cannot request mode 0000. Only permission bits
	// (0777) are accepted; type, setuid, setgid, and sticky bits are rejected.
	// A directory HostPath preserves its tree's permissions, so a non-zero
	// Mode with a directory root is a ValidationError. Windows cannot
	// represent arbitrary Unix bits and maps the read-only attribute only.
	Mode fs.FileMode
}

// FileFromContent returns a File that copies content to containerPath. A nil
// content is normalized to an empty, non-nil slice so an empty file cannot be
// mistaken for an unset Content. Only the first mode value is used; zero keeps
// the 0600 default.
func FileFromContent(containerPath string, content []byte, mode ...fs.FileMode) File {
	if content == nil {
		content = []byte{}
	}
	f := File{Content: content, ContainerPath: containerPath}
	if len(mode) > 0 {
		f.Mode = mode[0]
	}
	return f
}

// FileFromHost returns a File that copies the host file or directory at
// hostPath to containerPath. WithFiles resolves hostPath when the option is
// applied. Only the first mode value is used; zero preserves the source's
// permissions, and a non-zero mode is invalid when hostPath is a directory.
func FileFromHost(hostPath, containerPath string, mode ...fs.FileMode) File {
	f := File{HostPath: hostPath, ContainerPath: containerPath}
	if len(mode) > 0 {
		f.Mode = mode[0]
	}
	return f
}

// WithFiles copies host files and directories into the container after it
// starts. Each File must set exactly one of HostPath or Content; setting both,
// or neither, is a ValidationError when the option is applied. Host paths are
// resolved and checked without following a final link at option evaluation; a
// symlink or special file root is rejected before any backend call. A directory
// is snapshotted tree-wise when the copy runs, bounded by
// MaxCopyToContainerEntries, MaxCopyToContainerDepth, and
// MaxCopyToContainerSize; a symlink or special file inside it fails the copy.
//
// A directory's contents are copied into ContainerPath, which is always
// treated as the destination directory itself. The source directory's basename
// is not added, so a container path that already exists is populated rather
// than nested under, and repeated WithReuse attaches copy the same tree to the
// same place. Docker defines the `<staged>/.` source form as directory
// contents. Apple Container receives the same form; the Apple integration
// scenario covers the directory-contents and repeated-attach cases when that
// backend is available and skips otherwise, so treat Apple's behavior as
// verified only on hosts where that test has run. A non-zero File.Mode with a
// directory HostPath is rejected because the source tree's permission bits are
// preserved.
//
// Content is bounded by MaxCopyToContainerSize at option evaluation and is read
// when the copy runs, so the caller must not modify it until Run returns. Copy
// failures fail Run; an ordinary newly created container is rolled back.
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
				abs, info, err := validateHostPath(f.HostPath)
				if err != nil {
					return newValidationErrorWithField("WithFiles", "hostPath", f.HostPath, err)
				}
				if info.IsDir() && f.Mode != 0 {
					return newValidationErrorWithField("WithFiles", "mode", f.Mode,
						fmt.Errorf("mode must be zero for a directory host path; the source tree's permissions are preserved"))
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

// validateHostPath resolves a host path and verifies that it is a regular file
// or directory. WithFiles must reject a symlink or special file when the option
// is applied so a bad root cannot surface only after the container starts. A
// directory's entries are checked while the snapshot is built. Lstat matches the
// copy path's O_NOFOLLOW open by refusing to follow a final link here.
func validateHostPath(hostPath string) (string, fs.FileInfo, error) {
	abs, err := resolveHostPath(hostPath)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("host path %q: %w", hostPath, err)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", nil, fmt.Errorf("host path %q is not a regular file or directory (mode %s)", hostPath, info.Mode().Type())
	}
	return abs, info, nil
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
