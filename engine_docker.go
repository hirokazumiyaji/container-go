package container

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

type dockerTargetEndpoint struct {
	rawHost    string
	scheme     string
	host       string
	isRemote   bool
	clientHost string
	context    string
}

// dockerEngine drives the `docker` CLI. Unlike Apple Container, the
// container IP is generally not reachable from the host (Docker
// Desktop), so on publishable network modes exposed ports are published
// to daemon-assigned loopback ports and endpoints resolve to those.
type dockerEngine struct {
	explicitTarget          bool
	explicitHost            bool
	explicitContext         bool
	explicitConfigDir       bool
	explicitDefaultPlatform bool
	host                    string
	context                 string
	configDir               string
	defaultPlatform         string
	resolvedTarget          *dockerTargetEndpoint
}

func newDockerEngine(host, context, configDir, defaultPlatform string) dockerEngine {
	return dockerEngine{
		explicitTarget:          true,
		explicitHost:            host != "",
		explicitContext:         context != "",
		explicitConfigDir:       configDir != "",
		explicitDefaultPlatform: defaultPlatform != "",
		host:                    host,
		context:                 context,
		configDir:               configDir,
		defaultPlatform:         defaultPlatform,
	}
}

const (
	dockerDeleteVolumesFlag = "--volumes"

	// Docker 29.7.0 is the first client/server combination accepted for
	// this copy-out contract. Older cp extractors fail closed.
	dockerCopyOutMinimumVersion = "29.7.0"

	dockerNetworkHost    = domain.NetworkModeHost
	dockerNetworkNone    = domain.NetworkModeNone
	dockerNetworkDefault = "default"
	dockerNetworkBridge  = "bridge"
	dockerNetworkNAT     = "nat"
)

func (dockerEngine) name() string   { return "docker" }
func (dockerEngine) binary() string { return "docker" }
func (dockerEngine) directIP() bool { return false }

// imageStoreID partitions presence-cache entries by the env-visible
// Docker client settings that change which store or platform variant a
// Run without WithPlatform would observe: DOCKER_HOST, DOCKER_CONTEXT,
// DOCKER_CONFIG (context metadata), and DOCKER_DEFAULT_PLATFORM. A
// context chosen only via `docker context use` with no env remains
// invisible here, matching the library's existing DOCKER_HOST-only
// remote detection.
func (e dockerEngine) imageStoreID() string {
	rawHost := e.effectiveHost()
	if rawHost == "" && e.resolvedTarget != nil {
		rawHost = e.resolvedTarget.rawHost
	}
	return normalizeDockerHost(rawHost) + "\x00" +
		e.effectiveContext() + "\x00" +
		e.effectiveConfigDir() + "\x00" +
		e.effectiveDefaultPlatform()
}

func (e dockerEngine) targetID() string {
	storeID := e.imageStoreID()
	if storeID == "\x00\x00\x00" {
		return ""
	}
	return "docker\x00" + storeID
}

func (e dockerEngine) isRemote() bool {
	if e.resolvedTarget != nil {
		return e.resolvedTarget.isRemote
	}
	return isRemoteDockerHostValue(e.effectiveHost())
}

// dockerVolumeNameRE is Docker's complete local-volume name grammar:
// an alphanumeric first character followed by at least one name character.
var dockerVolumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// checkConfig composes the pure option contract with the runtime probes the
// selected daemon requires. The public behavior is unchanged: validation runs
// first, so an invalid option never spawns a CLI child process.
func (e dockerEngine) checkConfig(ctx context.Context, cfg *config) error {
	if err := e.validateConfig(cfg); err != nil {
		return err
	}
	return e.probeRuntimeEnvironment(ctx, cfg)
}

// validateConfig rejects options Docker cannot honor before any image or
// container command is issued, including volume names and publish options that
// cannot be honored on the selected network.
//
// It is the pure core of checkConfig: it reads only cfg, runs no subprocess,
// and never mutates cfg.
func (dockerEngine) validateConfig(cfg *config) error {
	// Docker's local volume driver adds a minimum length to the shared name
	// grammar. Apple Container accepts names (for example, one-character
	// names) that Docker rejects, so this check stays backend-specific.
	for _, m := range cfg.mounts {
		if m.Type != MountVolume {
			continue
		}
		if len(m.Source) > maxVolumeNameBytes {
			return mountValidationErrorf(m, "volume name exceeds the %d-byte maximum: %q", maxVolumeNameBytes, m.Source)
		}
		if m.Source == "" {
			return mountValidationErrorf(m, "volume name must not be empty")
		}
		if len(m.Source) == 1 {
			return mountValidationErrorf(m, "volume name %q is too short, names should be at least two alphanumeric characters", m.Source)
		}
		if !dockerVolumeNameRE.MatchString(m.Source) {
			return mountValidationErrorf(m, "invalid volume name %q", m.Source)
		}
	}
	network := cfg.network
	if !cfg.networkExplicit {
		network = ""
	}
	if len(cfg.networkAliases) > 0 {
		if err := dockerNetworkAliasConfigError(network); err != nil {
			return err
		}
	}
	if !dockerNetworkAllowsPublish(network) {
		if len(cfg.published) > 0 {
			return dockerPublishOptionError(network, "WithPublishedPort")
		}
		if len(cfg.exposed) > 0 {
			return dockerPublishOptionError(network, "WithExposedPorts")
		}
	}
	return nil
}

func dockerNetworkIsolationReason(network dockerNetworkInfo) string {
	if network.Internal {
		return "internal"
	}
	for key, value := range network.Options {
		key = strings.ToLower(key)
		if (strings.HasSuffix(key, "gateway_mode_ipv4") || strings.HasSuffix(key, "gateway_mode_ipv6")) &&
			strings.EqualFold(strings.TrimSpace(value), "isolated") {
			return "isolated"
		}
	}
	return ""
}

// dockerNetworkPublishUnsupportedReason reports network properties that
// make the library's host-published endpoint contract impossible to prove.
// Port publishing is implemented by the bridge-family drivers. In
// particular, macvlan and ipvlan endpoints are directly attached to an
// underlay and do not create host port mappings; an empty or third-party
// driver is not safe to assume capable either.
func dockerNetworkPublishUnsupportedReason(network dockerNetworkInfo) string {
	if reason := dockerNetworkIsolationReason(network); reason != "" {
		return reason
	}
	driver := strings.ToLower(strings.TrimSpace(network.Driver))
	switch driver {
	case "bridge", "nat", "overlay":
		return ""
	default:
		if driver == "" {
			return "network driver is unknown"
		}
		return fmt.Sprintf("network driver %q does not support host port publishing", driver)
	}
}

func dockerPublishOptionError(mode, option string) error {
	detail := fmt.Sprintf("Docker network mode %q cannot create a host port binding", mode)
	if mode == dockerNetworkHost {
		detail = "Docker host networking does not create host port bindings"
	}
	return &ConfigError{
		Backend: "docker",
		Network: mode,
		Option:  option,
		Detail:  detail,
	}
}

// dockerNetworkAliasConfigError reports network modes that cannot resolve
// network-scoped aliases. Docker registers --network-alias entries only for
// user-defined networks; the default bridge (bridge on Linux, nat on native
// Windows), host, and none modes all lack the embedded DNS resolver.
func dockerNetworkAliasConfigError(network string) error {
	switch network {
	case "", dockerNetworkDefault, dockerNetworkBridge, dockerNetworkNAT, dockerNetworkHost, dockerNetworkNone:
		return &ConfigError{
			Backend: "docker",
			Network: network,
			Option:  "WithNetworkAliases",
			Detail:  "network-scoped aliases require an explicitly selected user-defined network",
		}
	}
	return nil
}

// dockerReuseNetworkAliasesError verifies that an existing container would
// resolve every alias this caller asked for. Extra reported aliases are
// expected: Docker automatically adds the container name, short ID, and
// hostname to the endpoint's DNS names.
func dockerReuseNetworkAliasesError(cfg *config, info *engineInfo) error {
	for _, alias := range cfg.networkAliases {
		if !slices.Contains(info.networkAliases, alias) {
			return fmt.Errorf("%w: requested network alias %q is missing from the existing container", ErrNetworkMismatch, alias)
		}
	}
	return nil
}

// dockerNetworkAllowsPublish reports whether Docker can create a
// host-side binding for a port in the requested network mode. An empty
// mode is the daemon-selected default; named, bridge, and nat networks
// can publish ports, while host and none cannot.
func dockerNetworkAllowsPublish(mode string) bool {
	return mode != dockerNetworkHost && mode != dockerNetworkNone
}

// dockerNetworkModesMatch compares a requested mode with the inspected
// mode and current network membership when no daemon-default identity is
// available. The default-aware form below is used whenever Docker supplies
// its platform default; this wrapper remains useful to backend callers that
// only have concrete mode/name data.
func dockerNetworkModesMatch(requested, actual string, nameSets ...[]string) bool {
	var actualNames []string
	if len(nameSets) > 0 {
		actualNames = nameSets[0]
	}
	return dockerNetworkModesMatchDefault(requested, actual, actualNames, "")
}

func dockerNetworkModesMatchDefault(requested, actual string, actualNames []string, defaultNetwork string) bool {
	requested = strings.TrimSpace(requested)
	actual = strings.TrimSpace(actual)
	defaultNetwork = strings.TrimSpace(defaultNetwork)
	if actual == "" {
		return false
	}

	// HostConfig.NetworkMode describes how the container was configured;
	// NetworkSettings.Networks describes what it is attached to now. A
	// stale mode must not pass merely because its string equals the
	// requested mode after a network connect/disconnect.
	if actual == dockerNetworkDefault {
		if defaultNetwork == "" || !containsNetworkName(actualNames, defaultNetwork) {
			return false
		}
		if requested == "" || requested == dockerNetworkDefault {
			return true
		}
		return requested == defaultNetwork
	}
	// Docker creates no endpoint for "none" networking, so inspect
	// reports an empty NetworkSettings.Networks map. Membership proves
	// nothing there, and the mode string is the only identity Docker
	// reports; the none-specific errors come from
	// dockerNetworkEndpointError.
	if actual == dockerNetworkNone {
		if requested == "" || requested == dockerNetworkDefault {
			return defaultNetwork != "" && actual == defaultNetwork
		}
		return requested == actual
	}
	if !containsNetworkName(actualNames, actual) {
		return false
	}
	if requested == "" || requested == dockerNetworkDefault {
		return defaultNetwork != "" && actual == defaultNetwork && containsNetworkName(actualNames, defaultNetwork)
	}
	return requested == actual
}

func containsNetworkName(names []string, want string) bool {
	for _, name := range names {
		if strings.TrimSpace(name) == want {
			return true
		}
	}
	return false
}

func dockerNetworkModeError(requested, actual string, nameSets ...[]string) error {
	return dockerNetworkModeErrorDefault(requested, actual, firstNetworkNames(nameSets), "")
}

func dockerNetworkModeErrorDefault(requested, actual string, actualNames []string, defaultNetwork string) error {
	matches := dockerNetworkModesMatch(requested, actual, actualNames)
	if defaultNetwork != "" {
		matches = dockerNetworkModesMatchDefault(requested, actual, actualNames, defaultNetwork)
	}
	if matches {
		return nil
	}
	if defaultNetwork != "" {
		return fmt.Errorf("%w: requested Docker network mode %q, but inspect reported %q with networks %v (daemon default %q)", ErrNetworkMismatch, requested, actual, actualNames, defaultNetwork)
	}
	return fmt.Errorf("%w: requested Docker network mode %q, but inspect reported %q with networks %v; daemon default identity is unavailable", ErrNetworkMismatch, requested, actual, actualNames)
}

func firstNetworkNames(nameSets [][]string) []string {
	if len(nameSets) == 0 {
		return nil
	}
	return nameSets[0]
}

// defaultHost returns the address the client should dial to reach the
// container. For a remote daemon that is the remote endpoint host; otherwise it is
// this machine's loopback.
//
// Unlike the auto-publish bind decision, this must be scheme-aware for every
// remote transport. A client told to dial 127.0.0.1 for a container published
// on a remote daemon reaches nothing, which is the exact symptom the remote
// detection exists to prevent. Both DOCKER_HOST and Docker contexts (active
// or explicitly configured) are resolved.
func (e dockerEngine) defaultHost() string {
	if e.resolvedTarget != nil {
		return e.resolvedTarget.clientHost
	}
	host := e.effectiveHost()
	if host != "" {
		ep, err := parseDockerEndpoint(host)
		if err == nil {
			return ep.clientHost
		}
	}
	return "127.0.0.1"
}

func (dockerEngine) platformCompatible(selector, actual string) bool {
	return dockerPlatformMatches(selector, actual)
}
