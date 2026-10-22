package container

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// Client is a container client configured with pinned backend and target settings.
// Unlike process-global environment reads, a Client's operations (Run, Pull,
// Prune, PruneReuseGroup) consistently target the configured daemon and backend,
// even if process-level environment variables (such as DOCKER_HOST) change later.
type Client struct {
	eng    engine
	runner cli.Runner
}

// ClientOption configures a Client.
type ClientOption func(*clientConfig) error

type clientConfig struct {
	backend         string
	binary          string
	dockerHost      *string
	dockerContext   *string
	dockerConfigDir *string
	platform        *string
	runner          cli.Runner
}

// diagnosticValues lists the Client target values that must be redacted from
// construction errors. It mirrors config.diagnosticValues for the subset of
// settings a Client can freeze.
func (c *clientConfig) diagnosticValues() []string {
	values := make([]string, 0, 3)
	for _, value := range []*string{c.dockerHost, c.dockerContext, c.dockerConfigDir} {
		if value != nil {
			values = append(values, *value)
		}
	}
	return uniqueStrings(values)
}

// targetDiagnosticValues adds the target an engine resolved from the
// environment to the option values, mirroring config.diagnosticValues for
// Client.Run. Environment values did not pass option validation, so skip any
// value the bounded streaming window cannot redact.
func (c *clientConfig) targetDiagnosticValues(de dockerEngine, runner cli.Runner) []string {
	values := c.diagnosticValues()
	for _, value := range []string{de.effectiveHost(), de.effectiveContext(), de.effectiveConfigDir()} {
		if value != "" {
			values = append(values, value)
		}
	}
	// A caller-supplied runner can set the target in its own environment,
	// which the engine does not see until resolveTarget runs. Register those
	// values too: a failed resolution quotes them before a resolved engine
	// exists.
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG"} {
		if value, ok := runnerEnvLookup(runner, key); ok && value != "" {
			values = append(values, value)
		}
	}
	return uniqueStrings(values)
}

// dockerTargetOption names the option that set a Docker target override so a
// validation error points at the option the caller must change.
func (c *clientConfig) dockerTargetOption() string {
	switch {
	case c.dockerContext != nil:
		return "WithClientDockerContext"
	case c.dockerConfigDir != nil:
		return "WithClientDockerConfig"
	case c.platform != nil:
		return "WithClientPlatform"
	default:
		return "WithClientDockerHost"
	}
}

// WithClientBackend sets the backend engine: "apple" or "docker".
func WithClientBackend(backend string) ClientOption {
	return func(c *clientConfig) error {
		if err := validateBackendEnv(backend); err != nil {
			return newValidationError("WithClientBackend", nil, err)
		}
		c.backend = backend
		return nil
	}
}

// WithClientBinary sets the CLI executable path for the backend.
func WithClientBinary(binary string) ClientOption {
	return func(c *clientConfig) error {
		binary = strings.TrimSpace(binary)
		if binary == "" {
			return validationErrorf("WithClientBinary", binary, "binary must not be empty")
		}
		if strings.ContainsRune(binary, 0) || strings.ContainsAny(binary, "\r\n") {
			return validationErrorf("WithClientBinary", binary, "invalid binary path %q", binary)
		}
		c.binary = binary
		return nil
	}
}

// WithClientDockerHost sets the target DOCKER_HOST for Docker.
func WithClientDockerHost(host string) ClientOption {
	return func(c *clientConfig) error {
		if err := validateDockerTargetValue("WithClientDockerHost", host); err != nil {
			return err
		}
		c.dockerHost = &host
		return nil
	}
}

// WithClientDockerContext sets the target DOCKER_CONTEXT for Docker.
func WithClientDockerContext(context string) ClientOption {
	return func(c *clientConfig) error {
		if err := validateDockerTargetValue("WithClientDockerContext", context); err != nil {
			return err
		}
		c.dockerContext = &context
		return nil
	}
}

// WithClientDockerConfig sets the DOCKER_CONFIG directory for Docker.
func WithClientDockerConfig(dir string) ClientOption {
	return func(c *clientConfig) error {
		if err := validateDockerTargetValue("WithClientDockerConfig", dir); err != nil {
			return err
		}
		c.dockerConfigDir = &dir
		return nil
	}
}

// WithClientPlatform sets the target platform for Docker (DOCKER_DEFAULT_PLATFORM).
func WithClientPlatform(platform string) ClientOption {
	return func(c *clientConfig) error {
		if err := validateDockerTargetValue("WithClientPlatform", platform); err != nil {
			return err
		}
		c.platform = &platform
		return nil
	}
}

// WithClientRunner injects a custom cli.Runner.
func WithClientRunner(runner cli.Runner) ClientOption {
	return func(c *clientConfig) error {
		if runnerIsNil(runner) {
			return validationErrorf("WithClientRunner", nil, "runner must not be nil")
		}
		c.runner = runner
		return nil
	}
}

// NewClient creates a new Client configured with the supplied options.
// Any unspecified settings default to the process environment at the time
// NewClient is called, and are frozen onto the returned Client instance.
func NewClient(opts ...ClientOption) (*Client, error) {
	return newClient(context.Background(), "NewClient", opts...)
}

// NewClientContext creates a Client like NewClient while bounding backend
// target resolution (Docker context and CLI inspection) with ctx. The context
// applies only to construction; the returned Client's operations use the
// context passed to each operation.
func NewClientContext(ctx context.Context, opts ...ClientOption) (*Client, error) {
	return newClient(ctx, "NewClientContext", opts...)
}

func newClient(ctx context.Context, name string, opts ...ClientOption) (*Client, error) {
	cfg := &clientConfig{}
	for i, opt := range opts {
		if opt == nil {
			return nil, validationErrorf(name, i, "option %d is nil", i)
		}
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}

	backend := cfg.backend
	if backend == "" {
		if cfg.dockerHost != nil || cfg.dockerContext != nil || cfg.dockerConfigDir != nil || cfg.platform != nil {
			backend = "docker"
		} else {
			backend = os.Getenv(backendEnv)
		}
	}
	eng, err := detectEngineFor(runtime.GOOS, backend)
	if err != nil {
		return nil, safePublicError(err)
	}

	if _, ok := eng.(dockerEngine); ok {
		var de dockerEngine
		de.explicitTarget = true
		if cfg.dockerHost != nil {
			de.host = *cfg.dockerHost
			de.explicitHost = true
		} else {
			de.host = os.Getenv("DOCKER_HOST")
		}
		if cfg.dockerContext != nil {
			de.context = *cfg.dockerContext
			de.explicitContext = true
		} else {
			de.context = os.Getenv("DOCKER_CONTEXT")
		}
		if cfg.dockerConfigDir != nil {
			de.configDir = *cfg.dockerConfigDir
			de.explicitConfigDir = true
		} else {
			de.configDir = os.Getenv("DOCKER_CONFIG")
		}
		if cfg.platform != nil {
			de.defaultPlatform = *cfg.platform
			de.explicitDefaultPlatform = true
		} else {
			de.defaultPlatform = os.Getenv("DOCKER_DEFAULT_PLATFORM")
		}
		eng = de
	} else if cfg.dockerHost != nil || cfg.dockerContext != nil || cfg.dockerConfigDir != nil || cfg.platform != nil {
		return nil, validationErrorf(cfg.dockerTargetOption(), nil, "Docker target options require Docker backend")
	}

	binary := cfg.binary
	if binary == "" {
		binary = eng.binary()
	}

	runner := cfg.runner
	if runner == nil {
		var env []string
		if de, ok := eng.(dockerEngine); ok {
			env = de.pinnedTargetEnv()
		}
		runner = &cli.ExecRunner{Binary: binary, Env: env}
	} else if er, ok := runner.(*cli.ExecRunner); ok {
		cloned := &cli.ExecRunner{
			Binary: er.Binary,
			Env:    append([]string(nil), er.Env...),
		}
		if cloned.Binary == "" {
			cloned.Binary = binary
		}
		if de, ok := eng.(dockerEngine); ok {
			if targetEnv := de.pinnedTargetEnv(); len(targetEnv) > 0 {
				cloned.Env = pinRunnerEnv(cloned.Env, targetEnv)
			}
		}
		runner = cloned
	}

	if de, ok := eng.(dockerEngine); ok {
		resolved, err := de.resolveTarget(ctx, runner)
		if err != nil {
			// A cancellation or deadline belongs to this construction even
			// when the Client has no explicit target to fail against.
			if ctx.Err() != nil {
				return nil, safePublicError(err, cfg.targetDiagnosticValues(de, runner)...)
			}
			if de.effectiveContext() != "" || de.effectiveHost() != "" {
				return nil, safePublicError(err, cfg.targetDiagnosticValues(de, runner)...)
			}
			var configErr *ConfigError
			if errors.As(err, &configErr) {
				return nil, safePublicError(err, cfg.targetDiagnosticValues(de, runner)...)
			}
		} else {
			eng = resolved
			if er, ok := runner.(*cli.ExecRunner); ok {
				if targetEnv := resolved.pinnedTargetEnv(); len(targetEnv) > 0 {
					cloned := &cli.ExecRunner{
						Binary: er.Binary,
						Env:    pinRunnerEnv(er.Env, targetEnv),
					}
					runner = cloned
				}
			}
		}
	}

	return &Client{
		eng:    eng,
		runner: runner,
	}, nil
}

// Run creates and starts a container using this Client's target configuration.
func (c *Client) Run(ctx context.Context, image string, opts ...Option) (*Container, error) {
	clientOpts := []Option{
		withEngine(c.eng),
		withRunner(cloneRunner(c.runner)),
		withClientPinned(),
	}
	allOpts := append(clientOpts, opts...)
	return Run(ctx, image, allOpts...)
}

// cloneRunner copies an *cli.ExecRunner so per-Run options such as WithBinary
// cannot mutate the runner pinned on the Client.
func cloneRunner(r cli.Runner) cli.Runner {
	er, ok := r.(*cli.ExecRunner)
	if !ok {
		return r
	}
	cloned := *er
	cloned.Env = append([]string(nil), er.Env...)
	return &cloned
}

// Pull fetches an image into this Client's target store. PullOption values
// such as WithPullPlatform and WithPullTimeout apply to this pull. Docker
// target options are rejected because the Client pins its target.
func (c *Client) Pull(ctx context.Context, image string, opts ...PullOption) error {
	if err := validateImageReference(image); err != nil {
		return err
	}
	cfg, err := parsePullOptions("Client.Pull", opts)
	if err != nil {
		return err
	}
	if cfg.dockerHost != nil || cfg.dockerContext != nil {
		return validationErrorf(cfg.dockerTargetOption(), nil, "cannot override Docker target options on Client.Pull")
	}
	opCfg := cfg.operationConfig(c.pullRunner(cfg), c.eng)
	if err := pullWithConfig(ctx, opCfg, image); err != nil {
		return opCfg.publicError(err)
	}
	return nil
}

// Prune removes stopped containers managed by this library on this Client's
// target. PruneOption values such as WithPruneTimeout apply to this prune.
func (c *Client) Prune(ctx context.Context, opts ...PruneOption) ([]string, error) {
	cfg, err := parsePruneOptions("Client.Prune", opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneWithConfig(ctx, pinRunnerTargetEnv(c.eng, c.pruneRunner(cfg)), c.eng, cfg)
}

// PruneReuseGroup force-removes every container tagged with the given reuse group
// on this Client's target. PruneOption values such as WithPruneTimeout apply to
// this prune.
func (c *Client) PruneReuseGroup(ctx context.Context, group string, opts ...PruneOption) ([]string, error) {
	if err := validateReuseGroupFor("PruneReuseGroup", group); err != nil {
		return nil, err
	}
	cfg, err := parsePruneOptions("Client.PruneReuseGroup", opts)
	if err != nil {
		return nil, safePublicError(err)
	}
	return pruneReuseGroupWithConfig(ctx, pinRunnerTargetEnv(c.eng, c.pruneRunner(cfg)), c.eng, group, cfg)
}

// pullRunner selects the runner for a Client.Pull. A runner supplied through
// PullOption wins; otherwise the Client's pinned runner is cloned so a binary
// option cannot mutate it. cloneRunner only copies *cli.ExecRunner, so a custom
// pinned runner that implements SetBinary is mutated by WithPullBinary; pass a
// per-call runner instead when isolation is required.
func (c *Client) pullRunner(cfg pullConfig) Runner {
	if cfg.runner == nil && cfg.binary == "" {
		return c.runner
	}
	base := cfg.runner
	if base == nil {
		base = cloneRunner(c.runner)
	}
	return effectiveRunner(base, cfg.binary, c.eng)
}

// pruneRunner selects the runner for a Client prune. A runner supplied through
// PruneOption wins; otherwise the Client's pinned runner is cloned so a binary
// option cannot mutate it. cloneRunner only copies *cli.ExecRunner, so a custom
// pinned runner that implements SetBinary is mutated by WithPruneBinary; pass a
// per-call runner instead when isolation is required.
func (c *Client) pruneRunner(cfg pruneConfig) Runner {
	if cfg.runner == nil && cfg.binary == "" {
		return c.runner
	}
	base := cfg.runner
	if base == nil {
		base = cloneRunner(c.runner)
	}
	return effectiveRunner(base, cfg.binary, c.eng)
}

// Backend returns the backend engine name ("docker" or "apple").
func (c *Client) Backend() string {
	if c.eng != nil {
		return c.eng.name()
	}
	return ""
}

// DefaultHost returns the client-facing default host for this Client's target.
func (c *Client) DefaultHost() string {
	if c.eng != nil {
		return c.eng.defaultHost()
	}
	return "127.0.0.1"
}

// IsRemote reports whether this Client targets a remote daemon.
func (c *Client) IsRemote() bool {
	if c.eng != nil {
		return c.eng.isRemote()
	}
	return false
}
