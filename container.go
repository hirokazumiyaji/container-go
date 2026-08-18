package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

const (
	managedLabel = "com.github.hirokazumiyaji.container-go"
	sessionLabel = "com.github.hirokazumiyaji.container-go.session"

	queryTimeout = 30 * time.Second
	// runTimeout also covers an implicit image pull.
	runTimeout = 10 * time.Minute
)

// sessionID identifies all containers created by this process.
var sessionID = sync.OnceValue(func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("container: read random session id: %v", err))
	}
	return hex.EncodeToString(b[:])
})

func newContainerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("container: read random container name: %v", err))
	}
	return "containergo-" + hex.EncodeToString(b[:])
}

// State is a container lifecycle state as reported by the CLI.
type State string

const (
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateStopping State = "stopping"
	StateUnknown  State = "unknown"
)

// Container is a handle to a container created by Run.
type Container struct {
	id        string
	runner    cli.Runner
	exposed   []portSpec
	published []publishSpec

	mu   sync.Mutex
	info *inspect.Container // cached first inspect; immutable fields only
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, the container is
// removed before returning.
func Run(ctx context.Context, image string, opts ...Option) (*Container, error) {
	cfg := newConfig()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}
	if !imageRE.MatchString(image) {
		return nil, fmt.Errorf("invalid image reference %q", image)
	}
	if cfg.name == "" {
		cfg.name = newContainerName()
	}

	args, cleanup, err := buildRunArgs(cfg, image)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	runCtx, cancel := withDefaultTimeout(ctx, runTimeout)
	defer cancel()
	if _, _, err := cfg.runner.Run(runCtx, args...); err != nil {
		return nil, cli.Classify(ctx, cfg.runner, err)
	}

	c := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		exposed:   cfg.exposed,
		published: cfg.published,
	}
	if _, err := c.cachedInfo(ctx); err != nil {
		_ = c.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	for _, f := range cfg.files {
		if err := c.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			_ = c.Terminate(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return c, nil
}

// buildRunArgs assembles the `container run` argv. The returned cleanup
// removes the temporary env file directory and must always be called.
func buildRunArgs(cfg *config, image string) (args []string, cleanup func(), err error) {
	cleanup = func() {}
	args = []string{"run", "--detach", "--name", cfg.name}

	labels := map[string]string{
		managedLabel: "true",
		sessionLabel: sessionID(),
	}
	for k, v := range cfg.labels {
		labels[k] = v
	}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}

	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { _ = os.RemoveAll(dir) }
		args = append(args, "--env-file", path)
	}

	for _, p := range cfg.published {
		args = append(args, "--publish", p.raw)
	}
	for _, m := range cfg.mounts {
		args = append(args, "--mount", m.arg())
	}
	if cfg.cpus > 0 {
		args = append(args, "--cpus", strconv.Itoa(cfg.cpus))
	}
	if cfg.memory != "" {
		args = append(args, "--memory", cfg.memory)
	}
	if cfg.user != "" {
		args = append(args, "--user", cfg.user)
	}
	if cfg.workdir != "" {
		args = append(args, "--workdir", cfg.workdir)
	}
	if cfg.network != "" {
		args = append(args, "--network", cfg.network)
	}
	if cfg.platform != "" {
		args = append(args, "--platform", cfg.platform)
	}
	if cfg.entrypoint != "" {
		args = append(args, "--entrypoint", cfg.entrypoint)
	}

	args = append(args, image)
	args = append(args, cfg.cmd...)
	return args, cleanup, nil
}

// writeEnvFile stores env vars in a 0600 file under a private temporary
// directory, keeping values out of the process table.
func writeEnvFile(env map[string]string) (path, dir string, err error) {
	dir, err = os.MkdirTemp("", "containergo-env-")
	if err != nil {
		return "", "", err
	}
	var b []byte
	for _, k := range sortedKeys(env) {
		b = append(b, k+"="+env[k]+"\n"...)
	}
	path = filepath.Join(dir, "env")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return path, dir, nil
}

// ID returns the container ID (identical to its name).
func (c *Container) ID() string { return c.id }

// State returns the current lifecycle state.
func (c *Container) State(ctx context.Context) (State, error) {
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return StateUnknown, err
	}
	return State(info.Status.State), nil
}

// Stop stops the container. A nil timeout uses the CLI default grace
// period (SIGTERM, then SIGKILL after 5 seconds).
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error {
	args := []string{"stop"}
	if timeout != nil {
		args = append(args, "--time", strconv.Itoa(int(timeout.Seconds())))
	}
	args = append(args, c.id)
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err := c.runner.Run(stopCtx, args...)
	return cli.Classify(ctx, c.runner, err)
}

// Terminate force-removes the container. Removing a container that no
// longer exists is a success.
func (c *Container) Terminate(ctx context.Context) error {
	delCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err := c.runner.Run(delCtx, "delete", "--force", c.id)
	if err == nil || isNotFound(err) {
		return nil
	}
	return cli.Classify(ctx, c.runner, err)
}

// ContainerIP returns the container's address on its first attached
// network.
func (c *Container) ContainerIP(ctx context.Context) (string, error) {
	info, err := c.cachedInfo(ctx)
	if err != nil {
		return "", err
	}
	return info.IPv4()
}

// Host returns the address clients should connect to: the published
// host address when ports are published, the container IP otherwise.
func (c *Container) Host(ctx context.Context) (string, error) {
	if len(c.published) > 0 {
		return c.published[0].connectAddr(), nil
	}
	return c.ContainerIP(ctx)
}

// MappedPort resolves a declared container port ("6379/tcp" or "6379")
// to the port clients should dial: the published host port when the
// port is published, the container port itself otherwise.
func (c *Container) MappedPort(ctx context.Context, port string) (int, error) {
	_, p, err := c.resolve(ctx, port)
	return p, err
}

// Endpoint returns "host:port" for a declared container port.
func (c *Container) Endpoint(ctx context.Context, port string) (string, error) {
	host, p, err := c.resolve(ctx, port)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// resolve maps a container port to the (host, port) pair to dial.
func (c *Container) resolve(ctx context.Context, port string) (string, int, error) {
	spec, err := parsePortSpec(port)
	if err != nil {
		return "", 0, err
	}
	for _, p := range c.published {
		if p.containerPort == spec.port && p.proto == spec.proto {
			return p.connectAddr(), p.hostPort, nil
		}
	}
	if !slices.Contains(c.exposed, spec) {
		return "", 0, fmt.Errorf("%w: %s", ErrPortNotExposed, spec)
	}
	ip, err := c.ContainerIP(ctx)
	if err != nil {
		return "", 0, err
	}
	return ip, spec.port, nil
}

// cachedInfo returns the first successful inspect result. Only fields
// that cannot change while the container exists (image, labels,
// published ports, network address) should be read from it.
func (c *Container) cachedInfo(ctx context.Context) (*inspect.Container, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil {
		return c.info, nil
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	c.info = info
	return info, nil
}

func (c *Container) inspectFresh(ctx context.Context) (*inspect.Container, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, "inspect", c.id)
	if err != nil {
		return nil, cli.Classify(ctx, c.runner, err)
	}
	containers, err := inspect.Decode(stdout)
	if err != nil {
		return nil, err
	}
	for i := range containers {
		if containers[i].ID == c.id {
			return &containers[i], nil
		}
	}
	return nil, fmt.Errorf("container %s not in inspect output", c.id)
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func durationOrZero(d *time.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return *d
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
