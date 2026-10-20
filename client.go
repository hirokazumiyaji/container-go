package container

import (
	"context"
	"errors"
	"fmt"
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

// WithClientBackend sets the backend engine: "apple" or "docker".
func WithClientBackend(backend string) ClientOption {
	return func(c *clientConfig) error {
		if err := validateBackendEnv(backend); err != nil {
			return err
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
			return fmt.Errorf("client binary must not be empty")
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
		if runner == nil {
			return fmt.Errorf("client runner must not be nil")
		}
		c.runner = runner
		return nil
	}
}

// NewClient creates a new Client configured with the supplied options.
// Any unspecified settings default to the process environment at the time
// NewClient is called, and are frozen onto the returned Client instance.
func NewClient(opts ...ClientOption) (*Client, error) {
	cfg := &clientConfig{}
	for _, opt := range opts {
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
		return nil, err
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
		return nil, fmt.Errorf("docker target options cannot be used with %s backend", eng.name())
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
		resolved, err := de.resolveTarget(context.Background(), runner)
		if err != nil {
			if de.effectiveContext() != "" || de.effectiveHost() != "" {
				return nil, err
			}
			var configErr *ConfigError
			if errors.As(err, &configErr) {
				return nil, err
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

// Pull fetches an image into this Client's target store.
func (c *Client) Pull(ctx context.Context, image string) error {
	if err := validateImageReference(image); err != nil {
		return err
	}
	return safePublicError(pullWith(ctx, c.runner, c.eng, image))
}

// Prune removes stopped containers managed by this library on this Client's target.
func (c *Client) Prune(ctx context.Context) ([]string, error) {
	return pruneWith(ctx, c.runner, c.eng)
}

// PruneReuseGroup force-removes every container tagged with the given reuse group on this Client's target.
func (c *Client) PruneReuseGroup(ctx context.Context, group string) ([]string, error) {
	return pruneReuseGroupWith(ctx, c.runner, c.eng, group)
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
