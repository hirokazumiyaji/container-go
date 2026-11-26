package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// dockerDefaultNetwork asks the daemon which platform default it exposes.
// The client OS is not authoritative for a remote Docker daemon, and the
// name "bridge" or "nat" alone does not prove that a user-defined network
// is the daemon default.
func dockerDefaultNetwork(ctx context.Context, runner cli.Runner, eng engine, timeouts opTimeouts) (string, error) {
	qTimeout := timeouts.query
	if qTimeout <= 0 {
		qTimeout = queryTimeout
	}
	qCtx, cancel := context.WithTimeout(ctx, qTimeout)
	defer cancel()
	stdout, _, err := runner.Run(qCtx, "version", "--format", "{{.Server.Os}}")
	if contextErr := commandContextError(qCtx, err); contextErr != nil {
		return "", fmt.Errorf("inspect Docker server platform: %w", contextErr)
	}
	if err != nil {
		classified := classifyErrorWithProbe(ctx, runner, err, eng, timeouts.probe)
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

// probeRuntimeEnvironment resolves the daemon target and rejects options the
// resolved target cannot honor. This is the imperative shell around `docker
// context inspect` and `docker network inspect`; it records the resolved
// engine on cfg.
func (e dockerEngine) probeRuntimeEnvironment(ctx context.Context, cfg *config) error {
	network := cfg.network
	if !cfg.networkExplicit {
		network = ""
	}
	queryCtx := ctx
	queryCancel := func() {}
	if cfg.queryTimeout > 0 {
		queryCtx, queryCancel = context.WithTimeout(ctx, cfg.queryTimeout)
	}
	resolvedEng, err := e.resolveTarget(queryCtx, cfg.runner)
	queryCancel()
	if err != nil {
		return err
	}
	e = resolvedEng
	cfg.eng = resolvedEng
	applyEngineRunnerEnv(cfg)

	if e.isRemote() {
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
			if p.HostAddr.IsLoopback() {
				err := &ConfigError{
					Backend: "docker",
					Network: network,
					Option:  "WithPublishedPort",
					Detail:  fmt.Sprintf("published port %q binds loopback on a remote DOCKER_HOST and would be unreachable", p.Raw),
				}
				return newValidationError("WithPublishedPort", p.Raw, errors.Join(err, ErrEndpointUnreachable))
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
	queryCtx, cancel := context.WithTimeout(ctx, cfg.effectiveQueryTimeout())
	defer cancel()
	stdout, _, err := cfg.runner.Run(queryCtx, "network", "inspect", cfg.network)
	if err != nil {
		classified := classifyErrorWithProbe(ctx, cfg.runner, err, cfg.eng, cfg.effectiveProbeTimeout())
		return dockerNetworkInfo{}, fmt.Errorf("inspect Docker network %q before starting container: %w", cfg.network, classified)
	}
	return parseDockerNetworkInspect(stdout, cfg.network)
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
	return probeUnavailable(err, "docker", "version", dockerProbeLivenessText)
}

func dockerProbeLivenessText(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if dockerDesktopStartupFailure(lower) {
		return true
	}
	// Do not treat bare "error during connect" as daemon-down: the same
	// prefix introduces TLS, SSH, proxy, and configuration failures.
	// Windows named-pipe absence is handled by dockerWindowsNpipeUnavailable
	// so a missing file outside the Docker pipe namespace stays unclassified.
	if dockerWindowsNpipeUnavailable(line) {
		return true
	}
	for _, fragment := range []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"docker daemon is not running",
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"no such host",
		"host is down",
		"network is unreachable",
		"failed to connect to socket",
		"dial unix",
		"dial tcp",
	} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

var dockerDesktopUnableToStartRE = regexp.MustCompile(`(?i)^(?:error response from daemon:\s*)?docker desktop is unable to start\.?$`)

func dockerDesktopStartupFailure(line string) bool {
	// Only the bare Desktop startup failure is liveness evidence. A
	// configuration/proxy diagnosis after the phrase means the client
	// rejected a setting while still being reachable enough to say so.
	return dockerDesktopUnableToStartRE.MatchString(strings.TrimSpace(line))
}

// Windows reports a stopped daemon as a missing named pipe rather than a
// refused connection, so "error during connect" on its own is not usable
// evidence: the same prefix introduces TLS, SSH, and proxy failures.
// Require both halves of the real diagnostic instead — the Docker client's
// own pipe namespace plus the OS error that opening it produced. Any other
// pipe, or any other file-not-found, stays unclassified.
func dockerWindowsNpipeUnavailable(text string) bool {
	const missingFile = "the system cannot find the file specified"
	lower := strings.ToLower(text)
	if !strings.Contains(lower, missingFile) {
		return false
	}
	// Go's npipe package reports the forward-slash form while the Windows
	// CLI spells the same path with backslashes.
	normalized := strings.ReplaceAll(lower, `\`, "/")
	return strings.Contains(normalized, "//./pipe/docker") ||
		strings.Contains(normalized, "getnamedpipeinfo")
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

func (dockerEngine) checkCopyFileFromContainerVersion(ctx context.Context, runner cli.Runner, timeouts opTimeouts) error {
	qTimeout := timeouts.query
	if qTimeout <= 0 {
		qTimeout = queryTimeout
	}
	qCtx, cancel := context.WithTimeout(ctx, qTimeout)
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
