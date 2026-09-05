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
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	managedLabel    = "com.github.hirokazumiyaji.container-go"
	sessionLabel    = "com.github.hirokazumiyaji.container-go.session"
	reuseLabel      = "com.github.hirokazumiyaji.container-go.reuse"
	reuseGroupLabel = "com.github.hirokazumiyaji.container-go.reuse-group"
	creationLabel   = "com.github.hirokazumiyaji.container-go.creation"

	queryTimeout = 30 * time.Second
	// runTimeout also covers an implicit image pull.
	runTimeout = 10 * time.Minute
	// reuseAttachTimeout bounds waiting for another process's create
	// to reach a usable state during WithReuse get-or-create.
	reuseAttachTimeout = 60 * time.Second
	reusePollInterval  = 100 * time.Millisecond
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

func newCreationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("container: read random creation id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// State is a container lifecycle state.
type State string

const (
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateStopping State = "stopping"
	StateCreated  State = "created"
	StateUnknown  State = "unknown"
)

// Container is a handle to a container created by Run.
type Container struct {
	id        string
	runner    cli.Runner
	eng       engine
	exposed   []portSpec
	published []publishSpec
	// reused marks a WithReuse handle. Cleanup, TerminateContainer,
	// and the watchdog reaper skip these so shared containers survive
	// process exit. Explicit Terminate still removes them.
	reused bool
	// creation is the unique generation ID stored in creationLabel.
	// Terminate and the reaper verify it before deleting so a stale
	// handle never removes a same-name replacement.
	creation string

	mu   sync.Mutex
	info *engineInfo // cached first inspect; immutable fields only
}

// Run pulls the image if needed, creates and starts a container, and
// returns a handle to it. On failure after creation, the container is
// removed before returning. WithReuse switches to get-or-create; see
// WithReuse for the shared-handle lifecycle.
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
	if cfg.reuse && cfg.name == "" {
		return nil, fmt.Errorf("WithReuse requires WithName")
	}
	if cfg.reuseGroup != "" && !cfg.reuse {
		return nil, fmt.Errorf("WithReuseGroup requires WithReuse")
	}
	if cfg.eng == nil {
		eng, err := detectEngine()
		if err != nil {
			return nil, err
		}
		cfg.eng = eng
	}
	applyEngineBinary(cfg)
	if cfg.reuse {
		return reuseRun(ctx, image, cfg)
	}
	if cfg.name == "" {
		cfg.name = newContainerName()
	}
	cfg.creation = newCreationID()

	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}

	runCtx, cancel := withDefaultTimeout(ctx, runTimeout)
	defer cancel()
	// The pull policy brings the image into the local store before the
	// run command; both share the aggregated flight so concurrent Runs
	// of the same image pull once.
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
	}
	if _, _, err := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, image, envFile)...); err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, classified
	}

	// The reaper only backs real CLI containers; with an injected
	// test runner there is nothing external to clean up.
	if er, ok := cfg.runner.(cli.ExternalRunner); ok && er.External() && !keepContainers() {
		bin := er.ExternalBinary()
		if bin == "" {
			bin = cfg.eng.binary()
		}
		registerWithGlobalReaper(bin, cfg.eng.reaperSubcommand(), cfg.name, cfg.creation)
	}

	c := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		creation:  cfg.creation,
	}
	for _, f := range cfg.files {
		if err := c.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			_ = c.Terminate(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	if cfg.waitStrategy != nil {
		if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: c}); err != nil {
			cleanupCtx := context.WithoutCancel(ctx)
			tail := c.logTail(cleanupCtx)
			_ = c.Terminate(cleanupCtx)
			if tail != "" {
				return nil, fmt.Errorf("container %s failed to become ready: %w\ncontainer logs:\n%s", c.id, err, tail)
			}
			return nil, fmt.Errorf("container %s failed to become ready: %w", c.id, err)
		}
	}
	return c, nil
}

// cleanupFailedCreate best-effort removes the container this Run left
// behind after a failed create. It never deletes a pre-existing
// same-name container: name conflicts are skipped, and only a container
// carrying this process's managed+session labels is removed. When the
// creation generation is known it must also match.
func cleanupFailedCreate(ctx context.Context, cfg *config, runErr, classified error) {
	if cfg.eng.nameConflict(runErr) || cfg.eng.nameConflict(classified) {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	info, err := namedContainer(cfg, cfg.name).inspectFresh(cleanupCtx)
	if err != nil {
		return
	}
	if info.labels[managedLabel] != "true" {
		return
	}
	if sess, ok := info.labels[sessionLabel]; !ok || sess != sessionID() {
		return
	}
	if cfg.creation != "" {
		if actual, ok := info.labels[creationLabel]; ok && actual != cfg.creation {
			return
		}
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer delCancel()
	_, _, _ = cfg.runner.Run(delCtx, cfg.eng.deleteArgs(cfg.name)...)
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

func (c *Container) classify(ctx context.Context, err error) error {
	return cli.Classify(ctx, c.runner, err, c.eng.probe())
}

// State returns the current lifecycle state.
func (c *Container) State(ctx context.Context) (State, error) {
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return StateUnknown, err
	}
	return info.state, nil
}

// Stop stops the container. A nil timeout uses the CLI's default grace
// period before the process is killed.
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error {
	stopCtx, cancel := withDefaultTimeout(ctx, queryTimeout+durationOrZero(timeout))
	defer cancel()
	_, _, err := c.runner.Run(stopCtx, c.eng.stopArgs(c.id, timeout)...)
	return c.classify(ctx, err)
}

// Terminate force-removes the container. Removing a container that no
// longer exists is a success. When this handle knows its creation
// generation, a same-name replacement is never deleted: the inspect
// generation must match before the delete is issued.
func (c *Container) Terminate(ctx context.Context) error {
	if c.creation != "" && c.generationReplaced(ctx) {
		return fmt.Errorf("container %s was recreated; refusing to delete replaced container", c.id)
	}
	delCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err := c.runner.Run(delCtx, c.eng.deleteArgs(c.id)...)
	if err == nil || isNotFound(err) {
		return nil
	}
	return c.classify(ctx, err)
}

func (c *Container) generationReplaced(ctx context.Context) bool {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(c.id)...)
	if err != nil {
		// Missing container means nothing to protect; let the delete
		// run so idempotency holds. Other inspect failures fall
		// through to the delete attempt as well.
		return false
	}
	info, err := c.eng.parseInspect(stdout, c.id)
	if err != nil {
		return false
	}
	actual, ok := info.labels[creationLabel]
	if !ok || actual == "" {
		// An absent generation cannot prove ownership of this handle,
		// so treat it as a replacement and refuse the delete.
		return true
	}
	return actual != c.creation
}

// ContainerIP returns the container's address on its first attached
// network. With the Docker backend on Docker Desktop this address is
// usually not reachable from the host; prefer Endpoint.
func (c *Container) ContainerIP(ctx context.Context) (string, error) {
	info, err := c.cachedInfo(ctx)
	if err != nil {
		return "", err
	}
	if info.ip == "" {
		return "", fmt.Errorf("container %s has no reported IP address", c.id)
	}
	return info.ip, nil
}

// Host returns the address clients should connect to. When multiple
// published ports use different host IPs, prefer Endpoint for the
// specific port — Host returns only the first published binding's
// address (or the container IP / default host when nothing is published).
func (c *Container) Host(ctx context.Context) (string, error) {
	if len(c.published) > 0 {
		return c.published[0].connectAddr(), nil
	}
	if c.eng.directIP() {
		return c.ContainerIP(ctx)
	}
	return c.eng.defaultHost(), nil
}

// MappedPort resolves a declared container port ("6379/tcp" or "6379")
// to the port clients should dial.
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
	if c.eng.directIP() {
		ip, err := c.ContainerIP(ctx)
		if err != nil {
			return "", 0, err
		}
		return ip, spec.port, nil
	}
	// Published-port mode: the backend assigned a host port at start.
	info, err := c.cachedInfo(ctx)
	if err != nil {
		return "", 0, err
	}
	for _, b := range info.bound {
		if b.containerPort == spec.port && b.proto == spec.proto {
			host := b.hostAddr
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = c.eng.defaultHost()
			}
			return host, b.hostPort, nil
		}
	}
	return "", 0, fmt.Errorf("%w: %s has no host binding", ErrPortNotExposed, spec)
}

// cachedInfo returns the first successful inspect result. Only fields
// that cannot change while the container exists (labels, network
// address, port bindings) should be read from it.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
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

func (c *Container) inspectFresh(ctx context.Context) (*engineInfo, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := c.runner.Run(qCtx, c.eng.inspectArgs(c.id)...)
	if err != nil {
		return nil, c.classify(ctx, err)
	}
	return c.eng.parseInspect(stdout, c.id)
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

// splitNonEmptyLines splits CLI stdout on newlines and drops blank lines.
func splitNonEmptyLines(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
