package container

import (
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

const (
	maxVolumeNameBytes = 255
	maxMountPathBytes  = 4096
	maxReuseGroupBytes = 128
)

func (c *config) effectiveReuseAttachTimeout() time.Duration {
	if c.reuseAttachTimeout > 0 {
		return c.reuseAttachTimeout
	}
	return reuseAttachTimeout
}

func (c *config) effectiveReusePollInterval() time.Duration {
	if c.reusePollInterval > 0 {
		return c.reusePollInterval
	}
	return reusePollInterval
}

// WithReuse enables process- and cross-process get-or-create for a
// stable WithName. Concurrent Run calls with the same name share one
// container; readiness strategies always re-run against it. WithFiles
// is copied for every caller, and PullAlways is fetched for every
// caller before attach. CPU, shm, capability, ulimit, extra-host, and
// hostname options are part of the reuse compatibility identity and
// must match an existing container. Capabilities follow a requested-subset
// rule: every requested --cap-add/--cap-drop value must be present, while
// an empty request accepts any inspected capability set. Memory, user, working
// directory, entrypoint, command, mounts, and custom labels must exactly match
// the creation fingerprint, including omitted options. Reuse group must match.
// Unfingerprinted containers must be recreated before attaching. Environment
// differences are ignored. Returned handles are shared: Cleanup, TerminateContainer,
// and the watchdog reaper do not remove them. Stop, CopyToContainer,
// CopyContentToContainer, and CopyFileFromContainer refuse with
// ErrSharedContainer so one process cannot disturb peers; use Shared to
// opt in deliberately.
// Explicit Terminate still removes the container — only use it when no
// other process still needs it.
func WithReuse() Option {
	return func(c *config) error {
		c.reuse = true
		return nil
	}
}

// WithReuseGroup tags a reused container for later PruneReuseGroup.
// The group is part of reuse compatibility and the shared-flight key.
// Attaching requires an exact group match. Requires WithReuse.
func WithReuseGroup(group string) Option {
	return func(c *config) error {
		if err := validateReuseGroup(group); err != nil {
			return err
		}
		c.reuseGroup = group
		return nil
	}
}

// WithReuseAttachTimeout sets the maximum duration to wait for another process's
// container creation to complete before attaching. Requires WithReuse. A positive d
// overrides the default 60-second timeout. Non-positive durations are rejected.
func WithReuseAttachTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return validationErrorf("WithReuseAttachTimeout", d, "timeout must be positive")
		}
		c.reuseAttachTimeout = d
		return nil
	}
}

// WithReusePollInterval sets the interval between inspect attempts while
// waiting for a reused container to become ready. Requires WithReuse. A
// positive d overrides the default 100-millisecond cadence. Non-positive
// durations are rejected.
func WithReusePollInterval(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return validationErrorf("WithReusePollInterval", d, "poll interval must be positive")
		}
		c.reusePollInterval = d
		return nil
	}
}

// nameRE is the shared safe container-name form; Apple applies an
// additional minimum-length rule in its backend validator.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// labelKeyRE is the Docker-style label key rule the CLI enforces,
// extended with slash-separated OCI segments.
var labelKeyRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:[./][a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)

func validateReuseGroup(group string) error {
	return validateReuseGroupFor("WithReuseGroup", group)
}

func validateReuseGroupFor(option, group string) error {
	if group == "" {
		return newValidationErrorWithField(option, "group", group, fmt.Errorf("reuse group must not be empty"))
	}
	if len(group) > maxReuseGroupBytes || !labelKeyRE.MatchString(group) {
		return newValidationErrorWithField(option, "group", group, fmt.Errorf("invalid reuse group %q", group))
	}
	return nil
}

// WithName sets the container name, which Apple Container uses as the
// container ID. Default is containergo-<random hex>.
func WithName(name string) Option {
	return func(c *config) error {
		if !nameRE.MatchString(name) {
			return validationErrorf("WithName", name, "invalid container name %q: must match %s", name, nameRE)
		}
		c.name = name
		return nil
	}
}

// WithEnv adds environment variables. They are passed to the CLI via a
// temporary env file so values never appear in the process table. On Unix the
// file is 0600 under a 0700 per-user cache directory; on Windows it is
// protected by a current-logon-only DACL under %LocalAppData%.
//
// Keys must be non-empty valid UTF-8 without '=', Unicode whitespace or
// control characters, a leading '#', or a leading byte-order mark. Values must
// be valid UTF-8 without Unicode control characters, NUL, CR/LF, U+2028, or
// U+2029. Spaces, '=', and other non-control Unicode are valid in values.
// This common policy is intentionally stricter than values some backends may
// accept.
func WithEnv(env map[string]string) Option {
	return func(c *config) error {
		for _, k := range sortedKeys(env) {
			v := env[k]
			if err := validateEnvironmentEntry("WithEnv", "environment variable", k, v); err != nil {
				return err
			}
			if len(v) > cli.MaxStreamOverlap || !cli.StreamValueFits(v) {
				return invalidOption("environment variable value", "is too large to redact safely from streamed diagnostics")
			}
		}
		for k, v := range env {
			c.env[k] = v
		}
		return nil
	}
}

func validateEnvironmentEntry(option, description, key, value string) error {
	if err := validateEnvKey(key); err != nil {
		return newValidationErrorWithField(option, "key", key,
			fmt.Errorf("invalid %s name %q: %w", description, key, err))
	}
	if err := validateEnvValue(value); err != nil {
		// Environment and exec values can contain credentials. Keep both
		// the value and its error text free of the rejected value.
		return newValidationErrorWithField(option, "value", nil,
			fmt.Errorf("%s %q has an invalid value: %w", description, key, err))
	}
	return nil
}

// validateCommandArguments rejects argv elements the backend cannot carry.
// A NUL byte cannot cross an exec boundary and invalid UTF-8 cannot be
// reproduced by the JSON diagnostics, so both must fail before any backend
// work starts. The rejected element is not included in the error because
// command arguments may contain credentials.
func validateCommandArguments(option string, cmd []string) error {
	for i, arg := range cmd {
		if strings.ContainsRune(arg, 0) {
			return newValidationErrorWithField(option, "arg", nil,
				fmt.Errorf("%s argument %d must not contain NUL bytes", option, i))
		}
		if !utf8.ValidString(arg) {
			return newValidationErrorWithField(option, "arg", nil,
				fmt.Errorf("%s argument %d must be valid UTF-8", option, i))
		}
	}
	return nil
}

// WithCmd overrides the arguments passed to the image's entrypoint.
func WithCmd(cmd ...string) Option {
	return func(c *config) error {
		if err := validateCommandArguments("WithCmd", cmd); err != nil {
			return err
		}
		c.cmd = cmd
		return nil
	}
}

// WithEntrypoint overrides the image entrypoint. It is a single token
// per `docker run --entrypoint` semantics; for multi-token commands use
// WithCmd.
func WithEntrypoint(entrypoint string) Option {
	return func(c *config) error {
		if entrypoint == "" || strings.HasPrefix(entrypoint, "-") ||
			strings.ContainsAny(entrypoint, "\r\n\x00") || !utf8.ValidString(entrypoint) {
			return validationErrorf("WithEntrypoint", entrypoint, "invalid entrypoint %q", entrypoint)
		}
		c.entrypoint = entrypoint
		return nil
	}
}

// WithLabels adds labels on top of the session labels the library
// always applies. Internal labels are reserved and rejected.
func WithLabels(labels map[string]string) Option {
	return func(c *config) error {
		for k, v := range labels {
			if len(k) > 128 || !labelKeyRE.MatchString(k) {
				return newValidationErrorWithField("WithLabels", "key", k, fmt.Errorf("invalid label key %q", k))
			}
			if domain.IsReservedLabel(k) {
				return newValidationErrorWithField("WithLabels", "key", k, fmt.Errorf("label key %q is reserved", k))
			}
			// Label values may carry secrets. Do not put them in Value or
			// the rendered diagnostic when the entry is rejected.
			if len(k)+len(v)+1 > 4096 {
				return newValidationErrorWithField("WithLabels", "value", nil,
					fmt.Errorf("label %q: key=value exceeds 4096 bytes", k))
			}
			if strings.ContainsAny(v, "\x00") {
				return newValidationErrorWithField("WithLabels", "value", nil,
					fmt.Errorf("label %q: value must not contain NUL", k))
			}
		}
		for k, v := range labels {
			c.labels[k] = v
		}
		return nil
	}
}

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

// WithWorkingDir sets the working directory of the init process.
func WithWorkingDir(dir string) Option {
	return func(c *config) error {
		if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\r\n\x00") || !utf8.ValidString(dir) {
			return validationErrorf("WithWorkingDir", dir,
				"working directory %q must be an absolute path without control characters or invalid UTF-8", dir)
		}
		c.workdir = dir
		return nil
	}
}

// platformRE matches the shared Docker-style "os", "os/arch", or
// "os/arch/variant" forms. Apple applies its stricter os/arch[/variant]
// grammar in checkConfig.
var platformRE = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9_-]+){0,2}$`)

// WithPlatform selects the image platform, e.g. "linux/amd64" (runs via
// Rosetta). Apple Container applies its own os/arch[/variant] and variant
// validation after this shared syntax check.
func WithPlatform(p string) Option {
	return func(c *config) error {
		if !platformRE.MatchString(p) {
			return validationErrorf("WithPlatform", p, "invalid platform %q", p)
		}
		c.platform = p
		return nil
	}
}

// WithAttachOutput streams the container's log output to the provided writer
// starting immediately after container creation. It requires a container this
// call creates, so Run rejects it together with WithReuse; a reuse caller may
// attach to a container another process created and would otherwise receive no
// stream at all.
func WithAttachOutput(w io.Writer) Option {
	return func(c *config) error {
		if nilWriter(w) {
			return validationErrorf("WithAttachOutput", nil, "output writer cannot be nil")
		}
		c.attachOutput = w
		return nil
	}
}

// nilWriter reports whether w is nil, including a typed-nil value stored in
// the interface. A plain interface comparison misses that case, which would
// otherwise panic in the background attach stream on its first write.
func nilWriter(w io.Writer) bool {
	if w == nil {
		return true
	}
	value := reflect.ValueOf(w)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
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
