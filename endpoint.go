package container

import (
	"context"

	"github.com/hirokazumiyaji/container-go/internal/domain"
	"time"
)

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

// cachedInfo returns the immutable identity snapshot. Dynamic fields are
// intentionally not cached; endpoint and lifecycle callers use
// inspectDynamic instead.
func (c *Container) cachedInfo(ctx context.Context) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.nameInspect {
		return c.inspectFresh(ctx)
	}
	c.pack().mu.Lock()
	cached := c.pack().info
	c.pack().mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.rememberIdentity(ctx, info); err != nil {
		return nil, err
	}
	c.pack().mu.Lock()
	cached = c.pack().info
	c.pack().mu.Unlock()
	return cached, nil
}

// inspectDynamic returns lifecycle, network, and port data, reusing a
// short-TTL snapshot on lockPack when the data is still usable. Callers
// that must observe stop/replace immediately (State, post-reuse checks)
// use inspectDynamicFresh instead.
func (c *Container) inspectDynamic(ctx context.Context) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.nameInspect {
		if info := c.cachedDynamic(); info != nil {
			return info, nil
		}
		if err := c.pack().dynamicMu.Lock(ctx); err != nil {
			return nil, err
		}
		defer c.pack().dynamicMu.Unlock()
		// A concurrent refresh may have populated the snapshot while we
		// waited; reuse it instead of launching another inspect.
		if info := c.cachedDynamic(); info != nil {
			return info, nil
		}
	}
	return c.refreshDynamic(ctx)
}

// inspectDynamicFresh always performs an identity-checked inspect and
// refreshes the short-TTL dynamic snapshot shared with Shared() handles.
func (c *Container) inspectDynamicFresh(ctx context.Context) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.nameInspect {
		if err := c.pack().dynamicMu.Lock(ctx); err != nil {
			return nil, err
		}
		defer c.pack().dynamicMu.Unlock()
	}
	return c.refreshDynamic(ctx)
}

func (c *Container) cachedDynamic() *engineInfo {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !c.usableDynamicSnapshot(p.dynamic) {
		return nil
	}
	if !p.currentTime().Before(p.dynamicAt.Add(dynamicInspectTTL)) {
		return nil
	}
	info := cloneEngineInfo(p.dynamic)
	if p.defaultNetwork != "" {
		info.defaultNetwork = p.defaultNetwork
	}
	return info
}

// cachedInspectSnapshot returns the most recent lifecycle/network snapshot
// even when it carries no endpoint data. Health waits reuse the 1 Hz
// lifecycle probe's inspect through it instead of spawning another CLI
// process for every check.
func (c *Container) cachedInspectSnapshot() *engineInfo {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneEngineInfo(p.dynamic)
}

// healthStatus reports the Docker HEALTHCHECK status from a lifecycle
// snapshot observed within the wait probe interval. A stale snapshot is
// re-inspected through the normal query-timeout path so a cached healthy
// result cannot mask a later unhealthy transition.
func (c *Container) healthStatus(ctx context.Context) (string, error) {
	p := c.pack()
	now := p.currentTime()
	if info := c.cachedInspectSnapshot(); info != nil {
		if !info.observedAt.IsZero() && now.Sub(info.observedAt) <= dynamicInspectTTL {
			return info.health, nil
		}
	}
	// Re-observe through the normal identity-checked inspect, bounded by
	// the same query timeout as other inspect-backed reads.
	// inspectDynamicFresh stores the result through the generation-checked
	// dynamic cache, so later polls reuse this observation until it ages out.
	inspectCtx, cancel := context.WithTimeout(ctx, c.effectiveQueryTimeout())
	defer cancel()
	info, err := c.inspectDynamicFresh(inspectCtx)
	if err != nil {
		return "", err
	}
	return info.health, nil
}

func (c *Container) refreshDynamic(ctx context.Context) (*engineInfo, error) {
	gen := c.dynamicGeneration()
	info, err := c.inspectFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.applyCachedDefaultNetwork(ctx, info); err != nil {
		return nil, err
	}
	if c.nameInspect {
		return info, nil
	}
	if err := c.rememberIdentity(ctx, info); err != nil {
		return nil, err
	}
	c.storeDynamic(info, gen)
	return info, nil
}

func (c *Container) dynamicGeneration() uint64 {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dynamicGen
}

func (c *Container) storeDynamic(info *engineInfo, gen uint64) {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Delete/Terminate bump dynamicGen while an inspect may still be in
	// flight. Drop that result so endpoint reads cannot revive a deleted
	// container's IP/port under a fresh TTL.
	if p.dynamicGen != gen {
		return
	}
	p.dynamic = cloneEngineInfo(info)
	p.dynamicAt = p.currentTime()
}

func (c *Container) clearDynamic() {
	p := c.pack()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dynamic = nil
	p.dynamicAt = time.Time{}
	p.dynamicGen++
}

// usableDynamicSnapshot reports whether a cached inspect can answer an
// endpoint read. It accepts address or host-binding payload, and also a
// Running snapshot that needs neither: Docker host networking reports no
// address, and a port-less container resolves to the target host. Callers
// that still cannot resolve (empty IP, missing host binding) clear the
// snapshot so the next read re-inspects immediately instead of waiting out
// the TTL.
func (c *Container) usableDynamicSnapshot(info *engineInfo) bool {
	if info == nil {
		return false
	}
	if info.ip != "" {
		return true
	}
	for _, b := range info.bound {
		if b.hostPort > 0 {
			return true
		}
	}
	// Docker host networking reports neither a container IP nor any host
	// binding, and a container with no configured ports needs neither to
	// answer Host. Cache that running observation instead of re-inspecting
	// on every read.
	if info.state != StateRunning {
		return false
	}
	if info.networkMode == dockerNetworkHost {
		return true
	}
	return len(c.published) == 0 && len(c.exposed) == 0
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
