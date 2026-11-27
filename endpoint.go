package container

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// TargetHost returns the client-facing default host configured when this
// container was created.
func (c *Container) TargetHost() string {
	if c.targetHost != "" {
		return c.targetHost
	}
	if c.eng != nil {
		return c.eng.defaultHost()
	}
	return "127.0.0.1"
}

// ContainerIP returns the container's address on its first attached
// network. With the Docker backend on Docker Desktop this address is
// usually not reachable from the host; prefer Endpoint.
func (c *Container) ContainerIP(ctx context.Context) (string, error) {
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", c.redactError(err)
	}
	if info.ip == "" {
		c.clearDynamic()
		return "", c.redactError(errors.New("container has no reported IP address"), c.id, info.ip)
	}
	return info.ip, nil
}

// Host returns the address clients should connect to. When multiple
// published ports use different host IPs, prefer Endpoint for the
// specific port — Host returns only the first published binding's
// address (or the container IP / default host when nothing is published).
// Docker host networking returns the daemon host but does not infer a
// listening port; Docker none networking has no reachable host.
func (c *Container) Host(ctx context.Context) (string, error) {
	// Inspect before returning any address, including an explicitly
	// published direct-IP endpoint. A requested publish string is not
	// proof that the handle still names the same live container.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", err
	}
	if !c.eng.directIP() {
		if err := c.attachDefaultNetwork(ctx, info); err != nil {
			return "", err
		}
		if err := c.validateNetworkInfo(info); err != nil {
			return "", err
		}
	}
	var published *publishSpec
	if len(c.published) > 0 {
		published = &c.published[0]
	}
	host, _, err := domain.ResolveEndpoint(endpointNetwork(info), c.endpointBackend(), domain.EndpointRequest{
		Container: c.id,
		Published: published,
	})
	if err != nil {
		c.clearStaleEndpointSnapshot(err)
		return "", err
	}
	return host, nil
}

// MappedPort resolves a declared container port ("6379/tcp" or "6379")
// to the port clients should dial.
func (c *Container) MappedPort(ctx context.Context, port string) (int, error) {
	_, p, err := c.resolve(ctx, port, "MappedPort")
	return p, err
}

// Endpoint returns "host:port" for a port declared via WithExposedPorts
// or WithPublishedPort. It does not infer a host-network service port.
func (c *Container) Endpoint(ctx context.Context, port string) (string, error) {
	host, p, err := c.resolve(ctx, port, "Endpoint")
	if err != nil {
		return "", c.redactError(err, port)
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// resolve maps a container port to the (host, port) pair to dial.
func (c *Container) resolve(ctx context.Context, port, operation string) (string, int, error) {
	spec, err := domain.ParsePortSpec(port)
	if err != nil {
		return "", 0, newValidationErrorWithField(operation, "port", port, err)
	}
	var published *publishSpec
	for i := range c.published {
		p := &c.published[i]
		if p.ContainerPort == spec.Port && p.Proto == spec.Proto {
			published = p
			break
		}
	}
	if published == nil && !slices.Contains(c.exposed, spec) {
		return "", 0, fmt.Errorf("%w: %s", ErrPortNotExposed, spec)
	}
	// Resolve through an identity-checked dynamic inspect (short TTL)
	// before returning an endpoint. This is especially important for
	// direct-IP backends: a published address is otherwise just a stale
	// option value and can be returned for a replacement container
	// without any backend read.
	info, err := c.inspectDynamic(ctx)
	if err != nil {
		return "", 0, err
	}
	if !c.eng.directIP() {
		// Published-port mode: the backend assigned a host port at start.
		// Docker may discard -p in host mode, so the request is never used
		// as proof that a binding exists; inspect must confirm it.
		if err := c.attachDefaultNetwork(ctx, info); err != nil {
			return "", 0, err
		}
		if err := c.validateNetworkInfo(info); err != nil {
			return "", 0, err
		}
	}
	host, mapped, err := domain.ResolveEndpoint(endpointNetwork(info), c.endpointBackend(), domain.EndpointRequest{
		Container: c.id,
		Port:      spec,
		Published: published,
	})
	if err != nil {
		c.clearStaleEndpointSnapshot(err)
		return "", 0, c.redactError(err, port)
	}
	return host, mapped, nil
}

// endpointNetwork adapts an inspect snapshot to the pure endpoint resolver.
func endpointNetwork(info *engineInfo) domain.EndpointNetwork {
	return domain.EndpointNetwork{
		IP:       info.ip,
		Mode:     info.networkMode,
		Bindings: domainPortBindings(info.bound),
	}
}

// domainPortBindings adapts inspect bindings to the shared domain value. The
// endpoint resolver and reuse compatibility checks select bindings from the
// same table; keeping one converter here prevents the two paths from drifting.
func domainPortBindings(bound []boundPort) []domain.PortBinding {
	if len(bound) == 0 {
		return nil
	}
	bindings := make([]domain.PortBinding, len(bound))
	for i, binding := range bound {
		bindings[i] = domainPortBinding(binding)
	}
	return bindings
}

func domainPortBinding(binding boundPort) domain.PortBinding {
	return domain.PortBinding{
		ContainerPort: binding.containerPort,
		Proto:         binding.proto,
		HostAddr:      binding.hostAddr,
		HostPort:      binding.hostPort,
	}
}

func boundPortFromDomain(binding domain.PortBinding) boundPort {
	return boundPort{
		containerPort: binding.ContainerPort,
		proto:         binding.Proto,
		hostAddr:      binding.HostAddr,
		hostPort:      binding.HostPort,
	}
}

// endpointBackendFor describes the backend without a handle snapshot. Reuse
// compatibility uses it so its reachability check selects and validates the
// same bindings the endpoint resolver does.
func endpointBackendFor(eng engine) domain.EndpointBackend {
	return domain.EndpointBackend{
		DirectIP:    eng.directIP(),
		Remote:      eng.isRemote(),
		DefaultHost: eng.defaultHost(),
	}
}

func (c *Container) endpointBackend() domain.EndpointBackend {
	backend := endpointBackendFor(c.eng)
	// DefaultHost stays pinned to the handle's creation-time target. A later
	// DOCKER_HOST or context change must not redirect an existing handle's
	// endpoint to a different daemon.
	backend.DefaultHost = c.TargetHost()
	return backend
}

// clearStaleEndpointSnapshot drops the short-TTL cache when resolution failed
// only because the snapshot lacked the address or binding.
func (c *Container) clearStaleEndpointSnapshot(err error) {
	if domain.EndpointSnapshotStale(err) {
		c.clearDynamic()
	}
}

// validateNetworkInfo prevents a requested network mode from being
// mistaken for the mode the daemon actually used. A backend without the
// default-network capability does not report the mode, so this check is
// intentionally Docker-specific.
func (c *Container) validateNetworkInfo(info *engineInfo) error {
	if !c.eng.capabilities().defaultNetwork {
		return nil
	}
	requested := c.network
	if !c.networkExplicit {
		requested = ""
	}
	return dockerNetworkModeErrorDefault(requested, info.networkMode, info.networkNames, info.defaultNetwork)
}

func configNeedsDefaultNetwork(cfg *config) bool {
	return cfg != nil && cfg.eng != nil && cfg.eng.capabilities().defaultNetwork &&
		(!cfg.networkExplicit || cfg.network == dockerNetworkDefault)
}

func (c *Container) needsDefaultNetwork() bool {
	return c.eng.capabilities().defaultNetwork &&
		(!c.networkExplicit || c.network == dockerNetworkDefault)
}

func (c *Container) ensureDefaultNetwork(ctx context.Context) (string, error) {
	if !c.needsDefaultNetwork() {
		return "", nil
	}
	return c.ensureDefaultNetworkForConfig(ctx, nil)
}

// ensureDefaultNetworkForConfig resolves the daemon default for a caller that
// does not own the handle. The answer is stored on the handle, so concurrent
// reuse attaches sharing one ensure result perform one lookup; a failed
// lookup is not stored and the next caller retries. cfg supplies the caller's
// runner and optional Client cache for the miss path.
func (c *Container) ensureDefaultNetworkForConfig(ctx context.Context, cfg *config) (string, error) {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return "", err
	}
	defer c.pack().inspectMu.Unlock()
	if name := c.cachedDefaultNetwork(); name != "" {
		// The handle value is valid only for the handle's own target and
		// runner. A reuse waiter may carry a different runner (for example a
		// custom cli.Runner), and accepting the leader's answer would validate
		// its container against the wrong daemon.
		if cfg == nil || dockerDefaultNetworkKey(cfg.eng, cfg.runner) == dockerDefaultNetworkKey(c.eng, c.runner) {
			return name, nil
		}
	}
	runner, meta := c.runner, c.dockerMeta
	eng := c.eng
	timeouts := opTimeouts{query: c.effectiveQueryTimeout(), probe: c.effectiveProbeTimeout()}
	if cfg != nil {
		runner, meta, eng = cfg.runner, cfg.dockerMeta, cfg.eng
		timeouts = opTimeouts{query: cfg.effectiveQueryTimeout(), probe: cfg.effectiveProbeTimeout()}
	}
	name, err := dockerDefaultNetworkCached(ctx, meta, runner, eng, timeouts)
	if err != nil {
		return "", err
	}
	if cfg != nil && dockerDefaultNetworkKey(cfg.eng, cfg.runner) != dockerDefaultNetworkKey(c.eng, c.runner) {
		// Do not overwrite the handle's own answer with another runner's.
		return name, nil
	}
	return c.storeDefaultNetwork(name), nil
}

func (c *Container) attachDefaultNetwork(ctx context.Context, info *engineInfo) error {
	if !c.needsDefaultNetwork() {
		return nil
	}
	name, err := c.ensureDefaultNetwork(ctx)
	if err != nil {
		return err
	}
	info.defaultNetwork = name
	return nil
}

// attachDefaultNetworkForConfig resolves the daemon default for a reuse
// attach. Concurrent callers on one Client share the Client's cache, which is
// keyed by the resolved target, so they perform one lookup per target and a
// failed lookup is not retained.
func attachDefaultNetworkForConfig(ctx context.Context, cfg *config, info *engineInfo) error {
	if !configNeedsDefaultNetwork(cfg) {
		return nil
	}
	name, err := dockerDefaultNetworkCached(ctx, cfg.dockerMeta, cfg.runner, cfg.eng, opTimeouts{query: cfg.effectiveQueryTimeout(), probe: cfg.effectiveProbeTimeout()})
	if err != nil {
		return err
	}
	info.defaultNetwork = name
	return nil
}

func (c *Container) applyCachedDefaultNetwork(ctx context.Context, info *engineInfo) error {
	if err := c.pack().inspectMu.Lock(ctx); err != nil {
		return err
	}
	defer c.pack().inspectMu.Unlock()
	if name := c.cachedDefaultNetwork(); name != "" {
		info.defaultNetwork = name
	}
	return nil
}

// cachedDefaultNetwork and storeDefaultNetwork synchronize the lazily learned
// Docker daemon default with pack.mu. Endpoint's short-TTL cache reads the
// same field under that lock; writing it under inspectMu alone races.
func (c *Container) cachedDefaultNetwork() string {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.defaultNetwork
}

func (c *Container) storeDefaultNetwork(name string) string {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.defaultNetwork == "" && name != "" {
		p.defaultNetwork = name
	}
	return p.defaultNetwork
}
