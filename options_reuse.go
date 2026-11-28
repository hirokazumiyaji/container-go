package container

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// WithReuse enables process- and cross-process get-or-create for a
// stable WithName. Concurrent Run calls with the same name share one
// container; readiness strategies always re-run against it. WithFiles
// is copied for every caller, and PullAlways is fetched for every
// caller before attach. CPU, shm, capability, ulimit, extra-host, and
// hostname options are part of the reuse compatibility identity and
// must match an existing container. Capabilities follow a requested-subset
// rule: every requested --cap-add/--cap-drop value must be present, while
// an empty request accepts any inspected capability set. Memory, user, working
// directory, entrypoint, command, mounts, custom labels, read-only root
// filesystem, init, DNS servers, and stop signal must exactly match the
// creation fingerprint, including omitted options. Reuse group must match.
// Unfingerprinted containers must be recreated before attaching. Environment
// differences are ignored unless WithReuseEnvFingerprint is also set.
// Returned handles are shared: Cleanup, TerminateContainer,
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

// WithReuseEnvFingerprint opts into environment matching for WithReuse.
// The library records a SHA-256 digest of the sorted environment key/value
// pairs in a reserved label; raw values are never written to labels or argv.
// An attach caller that sets this option must find the same digest and
// otherwise receives ErrReuseMismatch. Without the option, environment
// differences remain ignored on attach. Requires WithReuse.
func WithReuseEnvFingerprint() Option {
	return func(c *config) error {
		c.reuseEnvFingerprint = true
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
