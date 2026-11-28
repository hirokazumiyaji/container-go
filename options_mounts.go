package container

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// WithMounts adds bind, volume, or tmpfs mounts.
func WithMounts(mounts ...Mount) Option {
	return func(c *config) error {
		for _, m := range mounts {
			if err := m.validate(); err != nil {
				return err
			}
		}
		c.mounts = append(c.mounts, mounts...)
		return nil
	}
}

// MountType selects how a Mount is backed.
type MountType int

// volumeNameRE is the name grammar shared by both backends. Docker adds a
// minimum length requirement in dockerEngine.checkConfig; Apple accepts a
// single-character name.
var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

const (
	// MountBind mounts a host directory (virtiofs).
	MountBind MountType = iota
	// MountVolume mounts a named volume. Docker managed cleanup preserves
	// named volumes, including volumes backed by a custom driver.
	MountVolume
	// MountTmpfs mounts an in-memory filesystem.
	MountTmpfs
)

// Mount describes one filesystem mount.
type Mount struct {
	Type     MountType
	Source   string // host path in host OS syntax (bind) or volume name (volume); empty for tmpfs
	Target   string // absolute POSIX path inside the container
	ReadOnly bool
}

// BindMount returns a bind mount of the absolute host source at
// containerTarget. Only the first readOnly value is used.
func BindMount(hostSource, containerTarget string, readOnly ...bool) Mount {
	return Mount{
		Type:     MountBind,
		Source:   hostSource,
		Target:   containerTarget,
		ReadOnly: len(readOnly) > 0 && readOnly[0],
	}
}

// VolumeMount returns a named-volume mount at containerTarget. Only the first
// readOnly value is used.
func VolumeMount(volumeName, containerTarget string, readOnly ...bool) Mount {
	return Mount{
		Type:     MountVolume,
		Source:   volumeName,
		Target:   containerTarget,
		ReadOnly: len(readOnly) > 0 && readOnly[0],
	}
}

// TmpfsMount returns an in-memory filesystem mount at containerTarget. A tmpfs
// mount has no source, unlike BindMount and VolumeMount.
func TmpfsMount(containerTarget string) Mount {
	return Mount{Type: MountTmpfs, Target: containerTarget}
}

func mountValidationErrorf(m Mount, format string, args ...any) error {
	return newValidationErrorWithField("WithMounts", "mount", m, fmt.Errorf(format, args...))
}

func (m Mount) validate() error {
	if m.Type < MountBind || m.Type > MountTmpfs {
		return mountValidationErrorf(m, "unknown mount type %d", m.Type)
	}
	if len(m.Source) > maxMountPathBytes || len(m.Target) > maxMountPathBytes {
		return mountValidationErrorf(m, "mount paths exceed the %d-byte maximum", maxMountPathBytes)
	}
	if !utf8.ValidString(m.Source) || !utf8.ValidString(m.Target) {
		return mountValidationErrorf(m, "mount paths must be valid UTF-8")
	}
	if strings.ContainsAny(m.Source, ",=\x00") || strings.ContainsAny(m.Target, ",=\x00") {
		return mountValidationErrorf(m, "mount %q -> %q: paths must not contain ',' or '='", m.Source, m.Target)
	}
	if strings.ContainsAny(m.Source, "\r\n") || strings.ContainsAny(m.Target, "\r\n") {
		return mountValidationErrorf(m, "mount %q -> %q: paths must not contain control characters", m.Source, m.Target)
	}
	if !strings.HasPrefix(m.Target, "/") {
		return mountValidationErrorf(m, "mount target %q must be absolute", m.Target)
	}
	switch m.Type {
	case MountBind:
		if !filepath.IsAbs(m.Source) {
			return mountValidationErrorf(m, "bind mount source %q must be an absolute host path", m.Source)
		}
	case MountVolume:
		if m.Source == "" {
			return mountValidationErrorf(m, "volume mount for %q needs a volume name: anonymous volume lifecycle is backend-specific", m.Target)
		}
		// Docker's minimum length is checked in checkConfig because Apple
		// accepts one-character names.
		if len(m.Source) > maxVolumeNameBytes {
			return mountValidationErrorf(m, "volume name exceeds the %d-byte maximum: %q", maxVolumeNameBytes, m.Source)
		}
		if !volumeNameRE.MatchString(m.Source) {
			return mountValidationErrorf(m, "invalid volume name %q", m.Source)
		}
	case MountTmpfs:
		if m.Source != "" {
			return mountValidationErrorf(m, "tmpfs mount for %q must not have a source", m.Target)
		}
	default:
		return invalidOption("mount type", "is unknown")
	}
	return nil
}

func (m Mount) arg() string {
	var b strings.Builder
	switch m.Type {
	case MountBind:
		b.WriteString("type=bind,source=" + m.Source)
	case MountVolume:
		b.WriteString("type=volume,source=" + m.Source)
	case MountTmpfs:
		b.WriteString("type=tmpfs")
	default:
		// Run validates mounts before argv construction. Keep a direct
		// call from producing a partial --mount value if that invariant
		// is ever bypassed by internal code.
		return ""
	}
	b.WriteString(",target=" + m.Target)
	if m.ReadOnly {
		b.WriteString(",readonly")
	}
	return b.String()
}
