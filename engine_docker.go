package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// dockerEngine drives the `docker` CLI. Unlike Apple Container, the
// container IP is generally not reachable from the host (Docker
// Desktop), so on publishable network modes exposed ports are published
// to daemon-assigned loopback ports and endpoints resolve to those.
type dockerEngine struct{}

// Verified against Docker Engine / CLI 29.x (local: 29.7.2).
// Stderr substrings below are matched case-insensitively on CLIError.Stderr.
// Observed wording:
//   - name conflict: "Conflict. The container name \"/x\" is already in use by container …"
//   - image missing: "Error response from daemon: No such image: …"
//   - container missing: "error: no such object: …" (also historically
//     "No such container" / "not found")
const (
	dockerStderrConflict     = "conflict"
	dockerStderrAlreadyInUse = "already in use"
	dockerStderrName         = "name"
	dockerStderrNoSuchImage  = "no such image"
	dockerStderrNotFound     = "not found"
	dockerStderrNoSuchObj    = "no such object"
	dockerStderrNoSuchCtr    = "no such container"

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

// checkConfig rejects publish options that Docker cannot honor before
// any image or container command is issued. Named networks are
// inspected because Docker's externally isolated networks cannot provide
// the host endpoints promised by WithExposedPorts or
// WithPublishedPort.
func (dockerEngine) checkConfig(ctx context.Context, cfg *config) error {
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
		// hostAddr is validated and canonicalized by parsePublishSpec.
		for _, p := range cfg.published {
			if ipIsLoopback(p.hostAddr) {
				err := &ConfigError{
					Backend: "docker",
					Network: network,
					Option:  "WithPublishedPort",
					Detail:  fmt.Sprintf("published port %q binds loopback on a remote DOCKER_HOST and would be unreachable", p.raw),
				}
				return errors.Join(err, ErrEndpointUnreachable)
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
		Args: []string{"version", "--format", "{{.Server.Version}}"},
		Hint: "start the Docker daemon",
	}
}

// defaultHost honors a tcp:// DOCKER_HOST (remote daemon); everything
// else publishes on loopback. Note: a `docker context` pointing at a
// remote daemon is not detected; only DOCKER_HOST is honored.
func (dockerEngine) defaultHost() string {
	if raw := os.Getenv("DOCKER_HOST"); strings.HasPrefix(raw, "tcp://") {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			host := canonicalIP(u.Hostname())
			if ipIsUnspecified(host) {
				if ipIs4(host) {
					return "127.0.0.1"
				}
				return "::1"
			}
			return host
		}
	}
	return "127.0.0.1"
}

// isRemoteDocker reports whether DOCKER_HOST points at a non-loopback
// tcp daemon. Auto-publish must bind 0.0.0.0 there; a loopback bind on
// the remote host is unreachable from the client.
func isRemoteDockerHost() bool {
	return !isLoopbackOrUnspecified((dockerEngine{}).defaultHost())
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
	// remote daemon (tcp:// DOCKER_HOST) it binds all interfaces so
	// the client can reach it via defaultHost().
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

func (dockerEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

// dockerInspect mirrors the fields of `docker inspect` output this
// library reads. Unknown fields are ignored.
type dockerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
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

func (dockerEngine) parseInspect(data []byte, target string) (*engineInfo, error) {
	var containers []dockerInspect
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	match := -1
	for i, c := range containers {
		if c.ID != "" && c.ID == target {
			match = i
			break
		}
	}
	if match < 0 && !dockerIDRE.MatchString(target) {
		for i, c := range containers {
			if strings.TrimPrefix(c.Name, "/") == target {
				match = i
				break
			}
		}
	}
	if match < 0 {
		return nil, fmt.Errorf("%w: container %s not in inspect output", ErrContainerNotFound, target)
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
	case "restarting", "removing":
		return StateStopping
	default:
		return StateUnknown
	}
}

func (dockerEngine) stopArgs(id string, timeout *time.Duration) []string {
	args := []string{"stop"}
	if timeout != nil {
		args = append(args, "--time", strconv.Itoa(int(timeout.Seconds())))
	}
	return append(args, id)
}

func (dockerEngine) deleteArgs(id string) []string {
	return []string{"rm", "--force", id}
}

func (dockerEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return []string{"cp", hostPath, id + ":" + containerPath}
}

func (dockerEngine) copyFromArgs(id, containerPath, hostPath string) []string {
	return []string{"cp", id + ":" + containerPath, hostPath}
}

func (dockerEngine) reaperSubcommand() string { return "rm" }

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

func (dockerEngine) logsArgs(id string, follow bool) []string {
	if follow {
		return []string{"logs", "--follow", id}
	}
	return []string{"logs", id}
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
		"ps", "--all", "--quiet",
		"--filter", "label=" + managedLabel + "=true",
		"--filter", "status=exited",
		"--format", "{{.Names}}",
	}
}

func (dockerEngine) parseStoppedManaged(data []byte) ([]string, error) {
	return splitNonEmptyLines(data), nil
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

// imageMissing matches the daemon's response for an absent image.
func (dockerEngine) imageMissing(err error) bool {
	return dockerStderrContains(err, dockerStderrNoSuchImage)
}

func (dockerEngine) parseImageExists(data []byte, _ string) bool {
	var images []json.RawMessage
	if err := json.Unmarshal(data, &images); err != nil {
		return false
	}
	return len(images) > 0
}

func (dockerEngine) listReuseGroupArgs(group string) []string {
	return []string{
		"ps", "--all", "--quiet",
		"--filter", "label=" + reuseGroupLabel + "=" + group,
		"--format", "{{.Names}}",
	}
}

func (dockerEngine) parseReuseGroupIDs(data []byte, _ string) ([]string, error) {
	return splitNonEmptyLines(data), nil
}

// nameConflict matches Docker's duplicate container name error.
func (dockerEngine) nameConflict(err error) bool {
	s, ok := dockerCLIStderr(err)
	if !ok {
		return false
	}
	return strings.Contains(s, dockerStderrConflict) ||
		(strings.Contains(s, dockerStderrAlreadyInUse) && strings.Contains(s, dockerStderrName))
}

// containerMissing matches a CLI failure for an absent container.
func (dockerEngine) containerMissing(err error) bool {
	s, ok := dockerCLIStderr(err)
	if !ok {
		return false
	}
	return strings.Contains(s, dockerStderrNotFound) ||
		strings.Contains(s, dockerStderrNoSuchObj) ||
		strings.Contains(s, dockerStderrNoSuchCtr)
}

func dockerCLIStderr(err error) (string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return "", false
	}
	return strings.ToLower(cliErr.Stderr), true
}

func dockerStderrContains(err error, substr string) bool {
	s, ok := dockerCLIStderr(err)
	return ok && strings.Contains(s, substr)
}
