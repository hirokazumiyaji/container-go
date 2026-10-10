package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/strictjson"
)

// dockerEngine drives the `docker` CLI. Unlike Apple Container, the
// container IP is generally not reachable from the host (Docker
// Desktop), so on publishable network modes exposed ports are published
// to daemon-assigned loopback ports and endpoints resolve to those.
type dockerEngine struct{}

// Docker parses stop --time through a signed integer. Cap the value at
// MaxInt32 so the argument is safe for 32-bit Docker CLIs as well as 64-bit
// ones; the daemon's duration conversion is also safe at this limit.
const maxDockerStopSeconds int64 = math.MaxInt32

// Verified against Docker Engine / CLI 29.x (local: 29.7.2); copy-out
// requires client/server 29.7.0 or newer.
// Stderr substrings below are matched case-insensitively on CLIError.Stderr.
// Observed wording:
//   - name conflict: "Conflict. The container name \"/x\" is already in use by container …"
//   - image missing: "Error response from daemon: No such image: …"
//   - container missing: "error: no such object: …" (also historically
//     "No such container" / "not found")
const (
	dockerDeleteVolumesFlag  = "--volumes"
	dockerStderrConflict     = "conflict. the container name "
	dockerStderrAlreadyInUse = "already in use"
	dockerStderrName         = "name"
	dockerStderrNoSuchImage  = "no such image:"
	dockerStderrNotFound     = "not found"
	dockerStderrNoSuchObj    = "no such object"
	dockerStderrNoSuchCtr    = "no such container"

	// Docker 29.7.0 is the first client/server combination accepted for
	// this copy-out contract. Older cp extractors fail closed.
	dockerCopyOutMinimumVersion = "29.7.0"

	dockerNetworkHost    = "host"
	dockerNetworkNone    = "none"
	dockerNetworkDefault = "default"
	dockerNetworkBridge  = "bridge"
	dockerNetworkNAT     = "nat"
)

func (dockerEngine) name() string   { return "docker" }
func (dockerEngine) binary() string { return "docker" }
func (dockerEngine) directIP() bool { return false }

func (dockerEngine) requiresImmutableID() bool { return true }

// imageStoreID partitions presence-cache entries by the env-visible
// Docker client settings that change which store or platform variant a
// Run without WithPlatform would observe: DOCKER_HOST, DOCKER_CONTEXT,
// DOCKER_CONFIG (context metadata), and DOCKER_DEFAULT_PLATFORM. A
// context chosen only via `docker context use` with no env remains
// invisible here, matching the library's existing DOCKER_HOST-only
// remote detection.
func (dockerEngine) imageStoreID() string {
	return normalizeDockerHost(os.Getenv("DOCKER_HOST")) + "\x00" +
		os.Getenv("DOCKER_CONTEXT") + "\x00" +
		os.Getenv("DOCKER_CONFIG") + "\x00" +
		os.Getenv("DOCKER_DEFAULT_PLATFORM")
}

// dockerDefaultNetwork asks the daemon which platform default it exposes.
// The client OS is not authoritative for a remote Docker daemon, and the
// name "bridge" or "nat" alone does not prove that a user-defined network
// is the daemon default.
func dockerDefaultNetwork(ctx context.Context, runner cli.Runner, eng engine) (string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := runner.Run(qCtx, "version", "--format", "{{.Server.Os}}")
	if err != nil {
		classified := cli.Classify(ctx, runner, err, eng.probe())
		return "", fmt.Errorf("inspect Docker server platform: %w", classified)
	}
	serverOS := strings.ToLower(strings.TrimSpace(string(stdout)))
	switch serverOS {
	case "linux":
		return dockerNetworkBridge, nil
	case "windows":
		return dockerNetworkNAT, nil
	default:
		return "", fmt.Errorf("%w: unsupported Docker server OS %q", ErrNetworkMismatch, serverOS)
	}
}

// dockerVolumeNameRE is Docker's complete local-volume name grammar:
// an alphanumeric first character followed by at least one name character.
var dockerVolumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// checkConfig rejects options Docker cannot honor before any image or
// container command is issued, including volume names and publish
// options that cannot be honored on the selected network.
func (dockerEngine) checkConfig(ctx context.Context, cfg *config) error {
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
	if !dockerNetworkAllowsPublish(network) {
		if len(cfg.published) > 0 {
			return dockerPublishOptionError(network, "WithPublishedPort")
		}
		if len(cfg.exposed) > 0 {
			return dockerPublishOptionError(network, "WithExposedPorts")
		}
	}
	if isRemoteDockerHost() {
		// The CLI does not expose the daemon OS or shared filesystem, so reject
		// every remote bind mount conservatively: Docker resolves the source
		// on the daemon host, and this library cannot verify that a client
		// path exists there or has compatible OS syntax.
		for _, m := range cfg.mounts {
			if m.Type == MountBind {
				return fmt.Errorf(
					"%w: bind mount source %q on a remote Docker daemon is resolved on the daemon host; use a local Docker daemon or copy the data into the container",
					ErrUnsupportedCapability, m.Source,
				)
			}
		}
		// hostAddr is validated and canonicalized by parsePublishSpec.
		for _, p := range cfg.published {
			if ipIsLoopback(p.hostAddr) {
				err := &ConfigError{
					Backend: "docker",
					Network: network,
					Option:  "WithPublishedPort",
					Detail:  fmt.Sprintf("published port %q binds loopback on a remote DOCKER_HOST and would be unreachable", p.raw),
				}
				return newValidationError("WithPublishedPort", p.raw, errors.Join(err, ErrEndpointUnreachable))
			}
		}
	}
	option := ""
	if len(cfg.published) > 0 {
		option = "WithPublishedPort"
	} else if len(cfg.exposed) > 0 {
		option = "WithExposedPorts"
	}
	if option != "" && network != "" && network != dockerNetworkDefault {
		networkInfo, err := inspectDockerNetwork(ctx, cfg)
		if err != nil {
			return err
		}
		if reason := dockerNetworkPublishUnsupportedReason(networkInfo); reason != "" {
			return &ConfigError{
				Backend: "docker",
				Network: network,
				Option:  option,
				Detail:  reason + "; this library cannot create a reachable published endpoint on this network",
			}
		}
	}
	return nil
}

func inspectDockerNetwork(ctx context.Context, cfg *config) (dockerNetworkInfo, error) {
	queryCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := cfg.runner.Run(queryCtx, "network", "inspect", cfg.network)
	if err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		return dockerNetworkInfo{}, fmt.Errorf("inspect Docker network %q before starting container: %w", cfg.network, classified)
	}
	return parseDockerNetworkInspect(stdout, cfg.network)
}

type dockerNetworkInfo struct {
	Name     string
	Driver   string
	Internal bool
	Options  map[string]string
}

func parseDockerNetworkInspect(data []byte, requested string) (dockerNetworkInfo, error) {
	if requested == "" {
		return dockerNetworkInfo{}, errors.New("docker network inspect requires a non-empty network name")
	}
	var networks []dockerNetworkInfo
	if err := json.Unmarshal(data, &networks); err != nil {
		return dockerNetworkInfo{}, fmt.Errorf("decode Docker network inspect output: %w", err)
	}
	var match *dockerNetworkInfo
	for i := range networks {
		network := &networks[i]
		if network.Name != requested {
			continue
		}
		if match != nil {
			return dockerNetworkInfo{}, fmt.Errorf("docker network inspect returned duplicate entries for %q", requested)
		}
		match = network
	}
	if match == nil {
		return dockerNetworkInfo{}, fmt.Errorf("docker network %q missing from inspect output", requested)
	}
	if strings.TrimSpace(match.Driver) == "" {
		return dockerNetworkInfo{}, fmt.Errorf("docker network %q inspect returned no driver", requested)
	}
	return *match, nil
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

func dockerNetworkEndpointError(mode string) error {
	switch strings.TrimSpace(mode) {
	case dockerNetworkHost:
		return fmt.Errorf("%w: Docker host networking has no library-managed host port binding; Host returns the daemon host, but Endpoint does not infer a port", ErrPortNotExposed)
	case dockerNetworkNone:
		return errors.Join(
			fmt.Errorf("%w: Docker network mode %q has no network interface for a host port binding", ErrPortNotExposed, mode),
			fmt.Errorf("%w: Docker network mode %q has no reachable host", ErrNoReachableHost, mode),
		)
	default:
		return nil
	}
}

func (dockerEngine) probe() cli.Probe {
	// version --format reaches the daemon without the heavy info
	// collection; only reachability matters for ErrSystemNotRunning.
	return cli.Probe{
		Args:          []string{"version", "--format", "{{.Server.Version}}"},
		Hint:          "start the Docker daemon",
		Binary:        "docker",
		Operation:     "version",
		IsUnavailable: dockerProbeUnavailable,
	}
}

func dockerProbeUnavailable(err error) bool {
	branches := backendCLIErrorBranches(err, "docker")
	if len(branches) == 0 {
		return false
	}
	// A reachable daemon can fail the client for TLS, certificate, SSH,
	// proxy, authentication, or endpoint-configuration reasons. Those
	// diagnostics veto only the matching version branch.
	for _, branch := range branches {
		if branch.ctx.operation != "version" {
			continue
		}
		if cli.IsProbeConfigurationError(branch.cause) {
			return false
		}
	}
	for _, branch := range branches {
		if branch.ctx.operation != "version" {
			continue
		}
		stderr, stdout := branchLines(branch, true)
		for _, line := range stderr {
			if dockerProbeLivenessText(line) {
				return true
			}
		}
		for _, line := range stdout {
			if dockerProbeLivenessText(line) {
				return true
			}
		}
	}
	return false
}

func dockerProbeLivenessText(line string) bool {
	if dockerDesktopStartupFailure(line) {
		return true
	}
	for _, fragment := range []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"docker daemon is not running",
		"error during connect",
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"no such host",
		"host is down",
		"network is unreachable",
		"pipe: The system cannot find the file specified",
		"open //./pipe/docker_engine",
		"open \\\\.\\pipe\\docker_engine",
		"failed to connect to socket",
		"dial unix",
		"dial tcp",
	} {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

var dockerDesktopUnableToStartRE = regexp.MustCompile(`(?i)^(?:error response from daemon:\s*)?docker desktop is unable to start\b`)

func dockerDesktopStartupFailure(line string) bool {
	return dockerDesktopUnableToStartRE.MatchString(strings.TrimSpace(line))
}

// defaultHost returns the address the client should dial to reach the
// container. For a remote daemon that is the DOCKER_HOST host; otherwise it is
// this machine's loopback.
//
// Unlike the auto-publish bind decision, this must be scheme-aware for every
// remote transport. A client told to dial 127.0.0.1 for a container published
// on a remote daemon reaches nothing, which is the exact symptom the remote
// detection exists to prevent. Note: a `docker context` pointing at a remote
// daemon is not detected; only DOCKER_HOST is honored.
func (dockerEngine) defaultHost() string {
	host := dockerHostName()
	if host == "" {
		return "127.0.0.1"
	}
	canonical := canonicalIP(host)
	if ipIsUnspecified(canonical) {
		if ipIs4(canonical) {
			return "127.0.0.1"
		}
		return "::1"
	}
	if isRemoteDockerHost() {
		if canonical != "" {
			return canonical
		}
		return host
	}
	return "127.0.0.1"
}

// normalizeDockerHost applies the same normalization the Docker CLI performs
// before dialing: a value with no "://" is a TCP host (hostname, host:port, or
// :port), so "tcp://" is prepended. A bare numeric value such as "2375" is a
// hostname, not a port — matching the Docker CLI. Scheme-less ":2375" becomes
// "tcp://:2375", after which isRemoteDockerHost applies Docker's empty-host
// fallback (default hostname → local).
//
// url.Parse cannot read those forms — "127.0.0.1:2375" errors outright and
// "localhost:2375" parses as an opaque scheme with no host — so without this
// they would fall through to the fail-closed branch and be reported as remote.
// That would bind auto-published ports to 0.0.0.0 on the developer's own
// machine.
func normalizeDockerHost(raw string) string {
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	return "tcp://" + raw
}

// dockerHostName returns the hostname DOCKER_HOST names, or "" when it names
// no host (a local socket, or an unparsable value).
func dockerHostName() string {
	u, err := url.Parse(normalizeDockerHost(os.Getenv("DOCKER_HOST")))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// isRemoteDockerHost reports whether DOCKER_HOST points at a daemon that may
// live on another machine. Auto-publish must bind 0.0.0.0 there; a 127.0.0.1
// bind on the remote host is unreachable from the client.
//
// The scheme is checked directly rather than through defaultHost, which
// recognizes only tcp://. ssh:// is a first-class Docker remote transport, and
// the daemon there resolves bind-mount sources on its own host, so a
// configuration derived from defaultHost would report local and let the bind
// through unchanged. A loopback host is still this machine. An empty hostname
// after parse (tcp://:2375, or scheme-less :2375 after normalizeDockerHost)
// matches the Docker CLI's TCP parser, which substitutes its default hostname
// — a local daemon — so those forms are local too. An unparsable value is
// treated as remote so a malformed setting fails closed rather than
// permitting a bind the daemon would resolve in the wrong place.
//
// ssh:// is remote in the same sense as tcp://, not a special case. The CLI's
// SSH session tunnels only the Docker API (`docker system dial-stdio` over
// stdio); it does not forward published container ports, which the daemon
// allocates on the remote host. So the connect address is the remote hostname,
// and that hostname must be directly dialable: an ssh-config alias reachable
// only through a ProxyJump or bastion needs a manual `ssh -L` forward, which
// is outside what DOCKER_HOST can express.
func isRemoteDockerHost() bool {
	raw := normalizeDockerHost(os.Getenv("DOCKER_HOST"))
	if raw == "" {
		return false
	}
	// These transports name a socket on this machine. fd:// (systemd
	// socket activation) yields an empty hostname from url.Parse; list it
	// with the other local sockets so the classification does not depend
	// on the empty-host TCP fallback below.
	for _, local := range []string{"unix://", "npipe://", "fd://"} {
		if strings.HasPrefix(strings.ToLower(raw), local) {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" {
		// Docker's ParseTCPAddr does `if host == "" { host = defaultAddr.Hostname() }`.
		// That default is this machine, so tcp://:2375 / :2375 are local.
		return false
	}
	return !isLoopbackOrUnspecified(host)
}

// isLoopbackOrUnspecified reports addresses that mean "this host" and
// must be rewritten to defaultHost() on a remote daemon.
func isLoopbackOrUnspecified(addr string) bool {
	if addr == "" || strings.EqualFold(addr, "localhost") {
		return true
	}
	return ipIsLoopback(addr) || ipIsUnspecified(addr)
}

// dockerConnectHost rewrites unspecified binds to the client-facing
// host. On a remote daemon, loopback and unspecified addresses become
// defaultHost(). Locally, an unspecified IPv6 bind becomes ::1 rather
// than losing its address family; an explicit loopback is preserved.
func dockerConnectHost(addr string, eng engine) string {
	canonical := canonicalIP(addr)
	if canonical == "" {
		return eng.defaultHost()
	}
	if isRemoteDockerHost() && isLoopbackOrUnspecified(canonical) {
		return eng.defaultHost()
	}
	if ipIsUnspecified(canonical) {
		if ipIs4(canonical) {
			return "127.0.0.1"
		}
		return "::1"
	}
	return canonical
}

func dockerBindingConnectHost(b boundPort, eng engine) (string, error) {
	if isRemoteDockerHost() && ipIsLoopback(b.hostAddr) {
		return "", fmt.Errorf("%w: Docker port %d is bound to remote-daemon loopback %q", ErrEndpointUnreachable, b.hostPort, canonicalIP(b.hostAddr))
	}
	return dockerConnectHost(b.hostAddr, eng), nil
}

func (e dockerEngine) runArgs(cfg *config, image, envFile string) []string {
	// The pull policy fetches the image beforehand; --pull=never keeps
	// the run command from pulling a second time behind our back.
	args := []string{"run", "--detach", "--pull", "never", "--name", cfg.name}
	// Publish every declared port the user did not publish explicitly
	// to a daemon-assigned port. Locally this binds loopback; on a
	// remote daemon it binds all interfaces so the client can reach it
	// via defaultHost().
	bindAddr := "127.0.0.1"
	if isRemoteDockerHost() {
		bindAddr = "0.0.0.0"
	}
	// checkConfig rejects these combinations before Run reaches this
	// method. Keep the argv builder safe for direct backend tests too:
	// host and none networks must never receive a -p flag.
	runCfg := cfg
	if !cfg.networkExplicit {
		copy := *cfg
		copy.network = ""
		runCfg = &copy
	}
	publishable := dockerNetworkAllowsPublish(runCfg.network)
	if !publishable {
		copy := *runCfg
		copy.published = nil
		runCfg = &copy
	}
	var extraPublish []string
	if publishable {
		for _, spec := range cfg.exposed {
			published := false
			for _, p := range cfg.published {
				if p.containerPort == spec.port && p.proto == spec.proto {
					published = true
					break
				}
			}
			if !published {
				extraPublish = append(extraPublish, bindAddr+"::"+spec.String())
			}
		}
	}
	return append(args, runCfg.commonRunArgs(image, envFile, extraPublish)...)
}

// dockerIDRE matches the full container ID `docker run --detach` prints.
var dockerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (dockerEngine) parseRunID(stdout []byte) string {
	id := strings.TrimSpace(string(stdout))
	if !dockerIDRE.MatchString(id) {
		return ""
	}
	return id
}

func (dockerEngine) inspectArgs(id string) []string {
	// Docker resolves an unqualified target across object types by default.
	// Restrict the CLI lookup to containers so a network or volume cannot
	// shadow the requested container name.
	return []string{"inspect", "--type=container", id}
}

// dockerInspect mirrors the fields of `docker inspect` output this
// library reads. Unknown fields are ignored.
type dockerInspect struct {
	ID       string `json:"Id"`
	Name     string `json:"Name"`
	Platform string `json:"Platform"`
	// State is present on container inspect objects, but not on Docker
	// network or volume inspect objects.
	State *struct {
		Status string `json:"Status"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Ports     map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// dockerInspectFields are the entry fields the matcher below depends on to
// recognize the requested container and its state. Nullable collections
// the CLI uses for empty values (Config.Labels, NetworkSettings.Ports,
// which real output emits as null) are deliberately absent: a null there
// is an empty value, not unreadable output.
var dockerInspectFields = []string{"Id", "Name", "State", "State.Status"}

// parseInspect returns the one entry that matches target exactly: the full
// container ID for an ID target, or the slash-prefixed name for a logical
// name. Other entries are ignored, and output the parser cannot read is an
// error rather than a missing container.
func (dockerEngine) parseInspect(data []byte, target string) (*engineInfo, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, newInspectTargetNotFound(target, "empty inspect output")
	}
	// Decode entry by entry so an entry this parser cannot interpret is
	// reported as a schema failure instead of a zero value. A target-naming
	// entry that decoded to nothing would be classified ErrContainerNotFound
	// even though the container exists, which lets a delete that verified
	// nothing pass as a removal that happened.
	containers, err := strictjson.Array[dockerInspect](data, dockerInspectFields)
	if err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(containers) == 0 {
		return nil, newInspectTargetNotFound(target, "inspect output did not contain the requested target")
	}
	match := -1
	var malformedID string
	var candidateMissingState bool
	if dockerIDRE.MatchString(target) {
		for i, c := range containers {
			if dockerIDRE.MatchString(c.ID) && c.ID == target {
				if c.State == nil || strings.TrimSpace(c.State.Status) == "" {
					candidateMissingState = true
					continue
				}
				match = i
				break
			}
		}
	} else {
		for i, c := range containers {
			if target != "" && c.Name == "/"+target {
				if c.ID != "" && !dockerIDRE.MatchString(c.ID) {
					if malformedID == "" {
						malformedID = c.ID
					}
					continue
				}
				if c.State != nil && dockerIDRE.MatchString(c.ID) {
					match = i
					break
				}
			}
		}
	}
	if match < 0 {
		if candidateMissingState {
			return nil, fmt.Errorf("decode docker inspect output: container State.Status is required")
		}
		if malformedID != "" {
			return nil, fmt.Errorf("decode docker inspect output: container %s has invalid ID %q", target, malformedID)
		}
		return nil, newInspectTargetNotFound(target, "inspect output did not contain the requested target")
	}
	c := containers[match]

	networkNames := make([]string, 0, len(c.NetworkSettings.Networks))
	for name := range c.NetworkSettings.Networks {
		networkNames = append(networkNames, name)
	}
	sort.Strings(networkNames)

	info := &engineInfo{
		state:        dockerState(c.State.Status),
		labels:       c.Config.Labels,
		uid:          c.ID,
		image:        c.Config.Image,
		platform:     c.Platform,
		platformMeta: platformMetadataFromString(c.Platform),
		ip:           c.NetworkSettings.IPAddress,
		networkMode:  c.HostConfig.NetworkMode,
		networkNames: networkNames,
	}
	if info.networkMode == "" {
		// HostConfig.NetworkMode is present in current Docker inspect
		// output. The network map fallback keeps endpoint validation
		// meaningful for older CLI versions that omitted HostConfig;
		// some Docker versions leave it empty for user-defined networks.
		if _, ok := c.NetworkSettings.Networks[dockerNetworkHost]; ok {
			info.networkMode = dockerNetworkHost
		} else if _, ok := c.NetworkSettings.Networks[dockerNetworkNone]; ok {
			info.networkMode = dockerNetworkNone
		} else if len(c.NetworkSettings.Networks) == 1 {
			for name := range c.NetworkSettings.Networks {
				info.networkMode = name
			}
		}
	}
	if info.ip == "" {
		for _, name := range networkNames {
			if ip := c.NetworkSettings.Networks[name].IPAddress; ip != "" {
				info.ip = ip
				break
			}
		}
	}
	for portProto, bindings := range c.NetworkSettings.Ports {
		spec, err := parsePortSpec(portProto)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			hostPort, err := strconv.Atoi(b.HostPort)
			if err != nil {
				continue
			}
			info.bound = append(info.bound, boundPort{
				containerPort: spec.port,
				proto:         spec.proto,
				hostAddr:      canonicalIP(b.HostIP),
				hostPort:      hostPort,
			})
		}
	}
	return info, nil
}

// dockerState maps Docker's status vocabulary onto State.
func dockerState(s string) State {
	switch s {
	case "running":
		return StateRunning
	case "created":
		return StateCreated
	case "exited", "dead":
		return StateStopped
	case "restarting":
		// Docker can move a restarting container back to running; keep
		// it distinct so startup readiness retries instead of failing
		// fast as stopped.
		return StateRestarting
	case "removing":
		// A removing container cannot become ready; readiness treats the
		// backend removal transition as terminal.
		return StateStopping
	case "paused":
		// A paused container is stable but not executing; wait treats
		// it as terminal rather than misreporting it as stopped.
		return StatePaused
	default:
		return StateUnknown
	}
}

func (dockerEngine) stopArgs(id string, timeout *time.Duration) ([]string, error) {
	return stopArgsFor(id, timeout, maxDockerStopSeconds)
}

func (dockerEngine) deleteArgs(id string) []string {
	return []string{"rm", "--force", dockerDeleteVolumesFlag, id}
}

func (dockerEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return []string{"cp", hostPath, id + ":" + containerPath}
}

func (dockerEngine) copyFromArgs(id, containerPath, hostPath string) []string {
	return []string{"cp", id + ":" + containerPath, hostPath}
}

func (dockerEngine) checkCopyFileFromContainer() error { return nil }

type dockerVersionOutput struct {
	Client struct {
		Version string `json:"Version"`
	} `json:"Client"`
	Server struct {
		Version string `json:"Version"`
	} `json:"Server"`
}

type dockerVersion struct {
	major int
	minor int
	patch int
}

func (v dockerVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func (v dockerVersion) less(other dockerVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	return v.patch < other.patch
}

var dockerVersionRE = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$`)

// parseDockerVersion accepts only the plain stable x.y.z form emitted by
// supported Docker releases. Suffixes describe development or distribution
// builds whose copy-out behavior has not been verified, so they fail closed.
func parseDockerVersion(raw string) (dockerVersion, error) {
	if !dockerVersionRE.MatchString(raw) {
		return dockerVersion{}, fmt.Errorf("version %q is not a stable major.minor.patch release", raw)
	}

	parts := strings.Split(raw, ".")
	version := dockerVersion{}
	values := []*int{&version.major, &version.minor, &version.patch}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return dockerVersion{}, fmt.Errorf("version %q: %w", raw, err)
		}
		*values[i] = n
	}
	return version, nil
}

func parseDockerVersionPair(data []byte) (dockerVersion, dockerVersion, error) {
	var output dockerVersionOutput
	if err := json.Unmarshal(data, &output); err != nil {
		return dockerVersion{}, dockerVersion{}, fmt.Errorf("decode Docker version output: %w", err)
	}
	clientRaw := output.Client.Version
	serverRaw := output.Server.Version
	if clientRaw == "" {
		return dockerVersion{}, dockerVersion{}, fmt.Errorf("docker client version is empty")
	}
	if serverRaw == "" {
		return dockerVersion{}, dockerVersion{}, fmt.Errorf("docker server version is empty")
	}
	client, err := parseDockerVersion(clientRaw)
	if err != nil {
		return dockerVersion{}, dockerVersion{}, err
	}
	server, err := parseDockerVersion(serverRaw)
	if err != nil {
		return dockerVersion{}, dockerVersion{}, err
	}
	return client, server, nil
}

// checkCopyFileFromContainerVersion verifies both ends of the Docker
// client/daemon connection before any private destination is created.
func (dockerEngine) checkCopyFileFromContainerVersion(ctx context.Context, runner cli.Runner) error {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := runner.Run(qCtx, "version", "--format", "{{json .}}")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if qCtx.Err() != nil {
			return fmt.Errorf("%w: Docker version query timed out: %w", ErrCopyFileFromContainerUnsupported, qCtx.Err())
		}
		return fmt.Errorf("%w: cannot verify Docker client/server version: %w", ErrCopyFileFromContainerUnsupported, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if qCtx.Err() != nil {
		return fmt.Errorf("%w: Docker version query timed out: %w", ErrCopyFileFromContainerUnsupported, qCtx.Err())
	}

	client, server, err := parseDockerVersionPair(stdout)
	if err != nil {
		return fmt.Errorf("%w: invalid Docker client/server version: %v", ErrCopyFileFromContainerUnsupported, err)
	}
	minimum := dockerVersion{major: 29, minor: 7, patch: 0}
	if client.less(minimum) || server.less(minimum) {
		return fmt.Errorf(
			"%w: Docker client/server must both be >=%s (got %s/%s)",
			ErrCopyFileFromContainerUnsupported,
			dockerCopyOutMinimumVersion,
			client,
			server,
		)
	}
	return nil
}

func (dockerEngine) reaperSubcommand() string { return "rm" }

func (dockerEngine) reaperDeleteFlags() []string { return []string{dockerDeleteVolumesFlag} }

func (dockerEngine) execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string {
	args := []string{"exec"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if cfg.user != "" {
		args = append(args, "--user", cfg.user)
	}
	if cfg.workdir != "" {
		args = append(args, "--workdir", cfg.workdir)
	}
	args = append(args, id)
	return append(args, cmd...)
}

func (dockerEngine) logsFollowArgs(id string) []string {
	return []string{"logs", "--follow", id}
}

func (dockerEngine) logsArgsWithOptions(id string, opts LogsOptions) ([]string, error) {
	args := []string{"logs"}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		args = append(args, "--since", opts.Since.Format(time.RFC3339))
	}
	return append(args, id), nil
}

// logsTailArgs bounds diagnostics at the CLI: last 1000 lines, then
// trimmed to logTailLimit bytes in Go with a fixed-size ring.
func (dockerEngine) logsTailArgs(id string) []string {
	return []string{"logs", "--tail", "1000", id}
}

// listArgs filters daemon-side; the Docker CLI supports label and
// status filters directly.
func (dockerEngine) listArgs() []string {
	return []string{
		"ps", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=" + managedLabel + "=true",
		"--filter", "status=exited",
		"--filter", "status=dead",
	}
}

func (dockerEngine) parseStoppedManaged(data []byte) ([]string, error) {
	return parseDockerPruneIDs(data)
}

func parseDockerPruneIDs(data []byte) ([]string, error) {
	ids := splitNonEmptyLines(data)
	for _, id := range ids {
		if !dockerIDRE.MatchString(id) {
			return nil, fmt.Errorf("docker ps returned invalid container ID %q", id)
		}
	}
	return ids, nil
}

func (dockerEngine) imageInspectArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "inspect", "--platform", platform, image}
	}
	return []string{"image", "inspect", image}
}

func (dockerEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"pull", "--platform", platform, image}
	}
	return []string{"pull", image}
}

// imageMissing matches only Docker's image-inspect response. Pull errors
// and arbitrary application output are not local-store absence evidence.
func (dockerEngine) imageMissing(err error) bool {
	return (dockerEngine{}).imageMissingForTarget(err, "")
}

func (dockerEngine) imageMissingForTarget(err error, target string) bool {
	branches := backendCLIErrorBranches(err, "docker")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if branch.ctx.operation != "image inspect" || !exactImageTarget(branch, target) {
			continue
		}
		if hasBranchImageLine(branch, dockerStderrNoSuchImage, branch.ctx.target, true) {
			return true
		}
	}
	return false
}

func (dockerEngine) parseImageExists(data []byte, _ string) bool {
	var images []json.RawMessage
	if err := json.Unmarshal(data, &images); err != nil {
		return false
	}
	return len(images) > 0
}

func (dockerEngine) platformCompatible(selector, actual string) bool {
	return dockerPlatformMatches(selector, actual)
}

// listReuseGroupArgs requests full container IDs: group prune verifies and
// deletes by immutable ID, so names or truncated IDs would be skipped.
func (dockerEngine) listReuseGroupArgs(group string) []string {
	return []string{
		"ps", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=" + reuseGroupLabel + "=" + group,
	}
}

func (dockerEngine) parseReuseGroupIDs(data []byte, _ string) ([]string, error) {
	return parseDockerPruneIDs(data)
}

// nameConflict matches Docker's duplicate container name error on a
// create/run command. A delete or application command containing the same
// words is not evidence that this library lost a name race.
func (dockerEngine) nameConflict(err error) bool {
	return (dockerEngine{}).nameConflictForTarget(err, "")
}

func (dockerEngine) nameConflictForTarget(err error, target string) bool {
	branches := backendCLIErrorBranches(err, "docker")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if branch.ctx.operation != "" && branch.ctx.operation != "run" {
			continue
		}
		if !exactBranchTarget(branch, target) {
			continue
		}
		if hasBranchLine(branch, func(line string) bool {
			return dockerNameConflictLine(line, branch.ctx.target)
		}) {
			return true
		}
	}
	return false
}

func dockerNameConflictLine(line, target string) bool {
	line = strings.ToLower(line)
	if !strings.HasPrefix(line, dockerStderrConflict) ||
		!strings.Contains(line, dockerStderrAlreadyInUse) {
		return false
	}
	if target == "" {
		return true
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, dockerStderrConflict))
	if len(rest) >= 2 && rest[0] == '"' {
		rest = rest[1:]
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			rest = rest[:end]
		}
		if sameDockerContainerName(rest, target) {
			return true
		}
	}
	return strings.Contains(line, strings.ToLower(target))
}

func sameDockerContainerName(got, want string) bool {
	got = strings.TrimPrefix(strings.Trim(strings.TrimSpace(got), `"'`), "/")
	want = strings.TrimPrefix(strings.Trim(strings.TrimSpace(want), `"'`), "/")
	return strings.EqualFold(got, want)
}

// containerMissing matches a CLI failure for an absent container.
func (dockerEngine) containerMissing(err error) bool {
	if !cliErrorBelongsTo(err, "docker") {
		return false
	}
	command, args, ok := cliCommandParts(err)
	if !ok {
		return false
	}
	target := cliCommandTarget(command, args)
	if target == "" {
		return false
	}
	switch command {
	case "inspect":
		return hasCLIErrorLine(err, func(line string) bool {
			if rest, ok := strings.CutPrefix(line, dockerStderrNoSuchObj+":"); ok && cliTargetListMatches(rest, target) {
				return true
			}
			rest, ok := strings.CutPrefix(line, dockerStderrNoSuchCtr+":")
			return ok && cliTargetListMatches(rest, target)
		})
	case "rm", "delete", "stop", "exec", "logs", "cp":
		if hasCLIErrorLine(err, func(line string) bool {
			rest, ok := strings.CutPrefix(line, dockerStderrNoSuchCtr+":")
			return ok && cliTargetListMatches(rest, target)
		}) {
			return true
		}
		// Older Docker clients used a target-qualified generic phrase for
		// rm. Keep that narrow fallback; it is not used for exec/logs,
		// whose stderr may be application output.
		if command == "rm" || command == "delete" {
			return hasCLIErrorLine(err, func(line string) bool {
				rest, ok := strings.CutPrefix(line, dockerStderrNotFound+":")
				return ok && cliTargetListMatches(strings.TrimSpace(rest), target)
			})
		}
		return false
	default:
		return false
	}
}
