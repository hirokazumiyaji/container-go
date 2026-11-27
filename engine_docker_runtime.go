package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

func (e dockerEngine) effectiveHost() string {
	if e.explicitHost || e.host != "" {
		return e.host
	}
	return os.Getenv("DOCKER_HOST")
}

func (e dockerEngine) effectiveContext() string {
	if e.explicitContext || e.context != "" {
		return e.context
	}
	if env := os.Getenv("DOCKER_CONTEXT"); env != "" {
		return env
	}
	if e.resolvedTarget != nil && e.resolvedTarget.context != "" {
		return e.resolvedTarget.context
	}
	return ""
}

func (e dockerEngine) effectiveConfigDir() string {
	if e.explicitConfigDir || e.configDir != "" {
		return e.configDir
	}
	return os.Getenv("DOCKER_CONFIG")
}

func (e dockerEngine) effectiveDefaultPlatform() string {
	if e.explicitDefaultPlatform || e.defaultPlatform != "" {
		return e.defaultPlatform
	}
	return os.Getenv("DOCKER_DEFAULT_PLATFORM")
}

// pinRunnerEnv applies a pinned target environment to a runner's own
// environment. Configured values override; a bare key is appended as a
// removal marker only when the runner does not set that key itself. The
// markers must stay in ExecRunner.Env because the inherited process
// environment is merged at exec time, not here.
func pinRunnerEnv(runnerEnv, pinned []string) []string {
	// Collapse duplicate keys first (last occurrence wins, matching
	// MergeEnv) so replacing a pinned key in place cannot leave an earlier
	// stale duplicate that first-match env lookup would prefer. Keys are
	// folded with envKeyFold because MergeEnv compares case-insensitively
	// on Windows.
	env := make([]string, 0, len(runnerEnv)+len(pinned))
	index := make(map[string]int, len(runnerEnv))
	for _, entry := range runnerEnv {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			// Index a bare removal marker too, so re-applying the pinned
			// environment cannot append the same marker twice.
			if key != "" {
				key = envKeyFold(key)
				if at, seen := index[key]; seen {
					env[at] = entry
					continue
				}
				index[key] = len(env)
			}
			env = append(env, entry)
			continue
		}
		key = envKeyFold(key)
		if at, duplicate := index[key]; duplicate {
			env[at] = entry
			continue
		}
		index[key] = len(env)
		env = append(env, entry)
	}
	for _, entry := range pinned {
		key, _, hasValue := strings.Cut(entry, "=")
		if key == "" {
			continue
		}
		key = envKeyFold(key)
		if at, exists := index[key]; exists {
			if hasValue {
				env[at] = entry
			}
			continue
		}
		index[key] = len(env)
		env = append(env, entry)
	}
	return env
}

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

type dockerContextInspect struct {
	Name      string `json:"Name"`
	Endpoints map[string]struct {
		Host string `json:"Host"`
	} `json:"Endpoints"`
}

func runnerEnvLookup(runner cli.Runner, key string) (string, bool) {
	entry, ok := runnerEnvEntry(runner, key)
	_, value, _ := strings.Cut(entry, "=")
	return value, ok
}

func runnerEnvEntry(runner cli.Runner, key string) (string, bool) {
	eenv, ok := runner.(interface{ ExternalEnv() []string })
	if !ok {
		return "", false
	}
	env := eenv.ExternalEnv()
	foldedKey := envKeyFold(key)
	for i := len(env) - 1; i >= 0; i-- {
		entryKey, _, _ := strings.Cut(env[i], "=")
		if envKeyFold(entryKey) == foldedKey {
			return env[i], true
		}
	}
	return "", false
}

// dockerContextNameRE is the context-name grammar Docker itself accepts: an
// ASCII alphanumeric first character followed by alphanumerics, dots,
// underscores, or hyphens. The leading character may not be a hyphen, so a
// name can never be parsed as a CLI flag.
var dockerContextNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func validDockerContextName(name string) bool {
	return dockerContextNameRE.MatchString(name)
}

// resolveDockerContextEndpoint is deliberately not cached process-wide. A
// Client freezes its resolved target at construction and every handle it
// creates carries that engine, so Client operations reuse one lookup. A
// package-level operation resolves once per call; a process-global cache
// could answer a changed DOCKER_CONTEXT or DOCKER_CONFIG with an earlier
// target's endpoint.
func resolveDockerContextEndpoint(ctx context.Context, runner cli.Runner, contextName string) (string, string, error) {
	if contextName != "" && !validDockerContextName(contextName) {
		return "", "", &ConfigError{
			Backend: "docker",
			Option:  "WithContext",
			Detail:  "invalid Docker context name",
		}
	}

	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()

	var args []string
	if contextName != "" {
		args = []string{"context", "inspect", "--", contextName}
	} else {
		args = []string{"context", "inspect"}
	}

	stdout, stderr, err := runner.Run(qCtx, args...)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if qCtx.Err() != nil {
			return "", "", fmt.Errorf("inspect Docker context timed out: %w", qCtx.Err())
		}
		errMsg := strings.TrimSpace(string(stderr))
		if errMsg == "" {
			errMsg = err.Error()
		}
		if isDockerContextUnavailable(err, errMsg) {
			return "", "", &dockerContextUnavailableError{detail: errMsg, cause: err}
		}
		configErr := &ConfigError{
			Backend: "docker",
			Option:  "context",
			Detail:  fmt.Sprintf("failed to inspect active Docker context: %s", errMsg),
		}
		if contextName != "" {
			configErr.Option = "WithContext"
			configErr.Detail = fmt.Sprintf("unknown Docker context %q: %s", contextName, errMsg)
		}
		return "", "", &dockerContextInspectError{ConfigError: configErr, cause: err}
	}

	if len(strings.TrimSpace(string(stdout))) == 0 {
		if contextName == "" {
			return "", "", &dockerContextUnavailableError{detail: "empty context inspect output"}
		}
		return "", "", &ConfigError{
			Backend: "docker",
			Option:  "WithContext",
			Detail:  fmt.Sprintf("Docker context %q not found", contextName),
		}
	}

	var contexts []dockerContextInspect
	if err := json.Unmarshal(stdout, &contexts); err != nil {
		return "", "", fmt.Errorf("decode Docker context inspect output: %w", err)
	}
	if len(contexts) == 0 {
		detail := "no context returned from Docker inspect"
		if contextName != "" {
			detail = fmt.Sprintf("Docker context %q not found", contextName)
		}
		return "", "", &ConfigError{
			Backend: "docker",
			Option:  "WithContext",
			Detail:  detail,
		}
	}
	// A named lookup must report the context that was requested. Without
	// this check a `--format` argument smuggled through an unvalidated name
	// could make `context inspect` return an endpoint for a different
	// context, flipping a remote daemon back to local.
	if contextName != "" && contexts[0].Name != contextName {
		return "", "", &ConfigError{
			Backend: "docker",
			Option:  "WithContext",
			Detail:  fmt.Sprintf("Docker context %q reported context %q", contextName, contexts[0].Name),
		}
	}
	endpointHost := contexts[0].Endpoints["docker"].Host
	if endpointHost == "" {
		if runtime.GOOS == "windows" {
			endpointHost = "npipe:////./pipe/docker_engine"
		} else {
			endpointHost = "unix:///var/run/docker.sock"
		}
	}
	resolvedName := contextName
	if resolvedName == "" && len(contexts) > 0 {
		resolvedName = contexts[0].Name
	}
	return resolvedName, endpointHost, nil
}

func (e dockerEngine) targetSelection(runner cli.Runner) (effectiveHost, effectiveCtx string, runnerHasHost, runnerSelectedContext bool) {
	effectiveHost = e.effectiveHost()
	if !e.explicitHost {
		if val, ok := runnerEnvLookup(runner, "DOCKER_HOST"); ok {
			effectiveHost = val
			runnerHasHost = true
		}
	}

	effectiveCtx = e.effectiveContext()
	if !e.explicitContext {
		if entry, ok := runnerEnvEntry(runner, "DOCKER_CONTEXT"); ok {
			if _, value, assigned := strings.Cut(entry, "="); assigned {
				effectiveCtx, runnerSelectedContext = value, true
			}
		}
	}

	return
}

func (e dockerEngine) resolveTarget(ctx context.Context, runner cli.Runner) (dockerEngine, error) {
	if e.resolvedTarget != nil && e.explicitTarget {
		return e, nil
	}

	effectiveHost, effectiveCtx, runnerHasHost, runnerSelectedContext := e.targetSelection(runner)

	resolvedCtx := effectiveCtx
	var targetHost string

	if e.explicitHost && !e.explicitContext {
		// Explicit host requested by caller takes precedence over ambient context.
		targetHost = effectiveHost
	} else if effectiveCtx != "" {
		if runner == nil {
			return e, &ConfigError{
				Backend: "docker",
				Option:  "WithContext",
				Detail:  fmt.Sprintf("cannot inspect Docker context %q without runner", effectiveCtx),
			}
		}
		ctxName, ctxEndpoint, err := resolveDockerContextEndpoint(ctx, runner, effectiveCtx)
		if err != nil {
			return e, err
		}
		// Docker context takes precedence over DOCKER_HOST.
		targetHost = ctxEndpoint
		if ctxName != "" {
			resolvedCtx = ctxName
		}
	} else if effectiveHost != "" {
		// No context set, explicit or ambient host is target.
		targetHost = effectiveHost
	} else if runner != nil {
		// Neither context nor host is set: inspect active context.
		ctxName, ctxEndpoint, err := resolveDockerContextEndpoint(ctx, runner, "")
		if err != nil {
			var unavail *dockerContextUnavailableError
			if errors.As(err, &unavail) {
				// The CLI or runner does not support `docker context`; fall back to default local socket.
				targetHost = ""
			} else {
				return e, err
			}
		} else {
			targetHost = ctxEndpoint
			if ctxName != "" {
				resolvedCtx = ctxName
			}
		}
	}

	ep, err := parseDockerEndpoint(targetHost)
	if err != nil {
		option := "WithDockerHost"
		if effectiveCtx != "" {
			option = "WithContext"
		}
		return e, cli.WithRedactor(&ConfigError{
			Backend: "docker",
			Option:  option,
			Detail:  err.Error(),
		}, cli.NewHashedRedactor(targetHost, resolvedCtx))
	}

	clone := e
	if !e.explicitConfigDir {
		if value, ok := runnerEnvLookup(runner, "DOCKER_CONFIG"); ok {
			clone.configDir = value
			clone.explicitConfigDir = true
		}
	}
	if resolvedCtx != "" {
		ep.context = resolvedCtx
	}
	clone.resolvedTarget = &ep
	if e.explicitHost && !e.explicitContext {
		clone.host = effectiveHost
		clone.explicitHost = true
		clone.explicitTarget = true
	} else if e.explicitContext || runnerSelectedContext {
		clone.context = resolvedCtx
		clone.host = ""
		clone.explicitContext = true
		clone.explicitTarget = true
	} else if runnerHasHost && targetHost == effectiveHost {
		clone.host = effectiveHost
		clone.explicitHost = true
		clone.explicitTarget = true
	}
	return clone, nil
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

func parseDockerEndpoint(raw string) (dockerTargetEndpoint, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return dockerTargetEndpoint{
			rawHost:    "",
			scheme:     "unix",
			host:       "",
			isRemote:   false,
			clientHost: "127.0.0.1",
		}, nil
	}
	normalized := normalizeDockerHost(trimmed)
	u, err := url.Parse(normalized)
	if err != nil {
		return dockerTargetEndpoint{}, fmt.Errorf("malformed Docker endpoint %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "unix", "npipe", "fd":
		return dockerTargetEndpoint{
			rawHost:    trimmed,
			scheme:     scheme,
			host:       "",
			isRemote:   false,
			clientHost: "127.0.0.1",
		}, nil
	case "tcp", "http", "https":
		host := u.Hostname()
		if host == "" {
			return dockerTargetEndpoint{
				rawHost:    trimmed,
				scheme:     scheme,
				host:       "",
				isRemote:   false,
				clientHost: "127.0.0.1",
			}, nil
		}
		canonical := canonicalIP(host)
		if isLoopbackOrUnspecified(host) {
			clientHost := "127.0.0.1"
			if ipIsUnspecified(canonical) && !ipIs4(canonical) {
				clientHost = "::1"
			}
			return dockerTargetEndpoint{
				rawHost:    trimmed,
				scheme:     scheme,
				host:       host,
				isRemote:   false,
				clientHost: clientHost,
			}, nil
		}
		clientHost := host
		if canonical != "" {
			clientHost = canonical
		}
		return dockerTargetEndpoint{
			rawHost:    trimmed,
			scheme:     scheme,
			host:       host,
			isRemote:   true,
			clientHost: clientHost,
		}, nil
	case "ssh":
		host := u.Hostname()
		if host == "" {
			return dockerTargetEndpoint{}, fmt.Errorf("docker ssh endpoint %q missing host", raw)
		}
		canonical := canonicalIP(host)
		if isLoopbackOrUnspecified(host) {
			clientHost := "127.0.0.1"
			if ipIsUnspecified(canonical) && !ipIs4(canonical) {
				clientHost = "::1"
			}
			return dockerTargetEndpoint{
				rawHost:    trimmed,
				scheme:     scheme,
				host:       host,
				isRemote:   false,
				clientHost: clientHost,
			}, nil
		}
		clientHost := host
		if canonical != "" {
			clientHost = canonical
		}
		return dockerTargetEndpoint{
			rawHost:    trimmed,
			scheme:     scheme,
			host:       host,
			isRemote:   true,
			clientHost: clientHost,
		}, nil
	default:
		return dockerTargetEndpoint{}, fmt.Errorf("%w: unsupported Docker endpoint scheme %q", ErrInvalidConfig, scheme)
	}
}

// isRemoteDockerHostValue reports whether the supplied DOCKER_HOST points at a
// daemon that may live on another machine.
func isRemoteDockerHostValue(rawHost string) bool {
	ep, err := parseDockerEndpoint(rawHost)
	if err != nil {
		return true
	}
	return ep.isRemote
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
	return isRemoteDockerHostValue(os.Getenv("DOCKER_HOST"))
}

// isLoopbackOrUnspecified reports addresses that mean "this host" and
// must be rewritten to defaultHost() on a remote daemon.
func isLoopbackOrUnspecified(addr string) bool {
	return domain.IsLoopbackOrUnspecified(addr)
}

// dockerConnectHost is the Docker backend's spelling of the shared connect
// address normalization. Endpoint resolution and reuse reachability both
// delegate to the domain rule so their answers cannot diverge.
func dockerConnectHost(addr string, eng engine) string {
	return domain.ConnectHost(addr, endpointBackendFor(eng))
}

func dockerBindingConnectHost(b boundPort, eng engine) (string, error) {
	return domain.BindingConnectHost(domainPortBinding(b), endpointBackendFor(eng))
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
