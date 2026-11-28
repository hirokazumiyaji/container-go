package container

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/internal/strictjson"
)

func (e dockerEngine) effectiveHost() string {
	if e.explicitHost || e.host != "" {
		return e.host
	}
	if e.frozenEnv {
		return ""
	}
	return os.Getenv("DOCKER_HOST")
}

func (e dockerEngine) effectiveContext() string {
	if e.explicitContext || e.context != "" {
		return e.context
	}
	if !e.frozenEnv {
		if env := os.Getenv("DOCKER_CONTEXT"); env != "" {
			return env
		}
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
	if e.frozenEnv {
		return ""
	}
	return os.Getenv("DOCKER_CONFIG")
}

func (e dockerEngine) effectiveDefaultPlatform() string {
	if e.explicitDefaultPlatform || e.defaultPlatform != "" {
		return e.defaultPlatform
	}
	if e.frozenEnv {
		return ""
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

	contexts, err := strictjson.Array[dockerContextInspect](stdout, nil)
	if err != nil {
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

	// An explicit empty host clears the inherited DOCKER_HOST and leaves the
	// active Docker context in charge, so the context still has to be
	// inspected: the child CLI resolves a missing DOCKER_HOST from
	// ~/.docker/config.json. Treating that form as a local default would let a
	// remote active context bypass the remote-target checks.
	clearedHost := e.explicitHost && !e.explicitContext && strings.TrimSpace(effectiveHost) == ""

	resolvedCtx := effectiveCtx
	var targetHost string

	switch {
	case e.explicitHost && !e.explicitContext && !clearedHost:
		// Explicit host requested by caller takes precedence over ambient context.
		targetHost = effectiveHost
	case effectiveCtx != "":
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
	case effectiveHost != "":
		// No context set, explicit or ambient host is target.
		targetHost = effectiveHost
	case runner != nil:
		// Neither context nor host is set (or the host was explicitly
		// cleared): inspect the active context.
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
	switch {
	case e.explicitHost && !e.explicitContext && !clearedHost:
		clone.host = effectiveHost
		clone.explicitHost = true
		clone.explicitTarget = true
	case e.explicitContext || runnerSelectedContext || clearedHost:
		clone.context = resolvedCtx
		clone.host = ""
		clone.explicitContext = true
		clone.explicitTarget = true
		if clearedHost {
			// The cleared host must stay in effect even if a runner's own
			// environment exports DOCKER_HOST.
			clone.explicitHost = true
		}
	case runnerHasHost && targetHost == effectiveHost:
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
		canonical := domain.CanonicalIP(host)
		if domain.IsLoopbackOrUnspecified(host) {
			clientHost := "127.0.0.1"
			if domain.IPIsUnspecified(canonical) && !domain.IPIs4(canonical) {
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
		canonical := domain.CanonicalIP(host)
		if domain.IsLoopbackOrUnspecified(host) {
			clientHost := "127.0.0.1"
			if domain.IPIsUnspecified(canonical) && !domain.IPIs4(canonical) {
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

// dockerConnectHost is the Docker backend's spelling of the shared connect
// address normalization. Endpoint resolution and reuse reachability both
// delegate to the domain rule so their answers cannot diverge.
func dockerConnectHost(addr string, eng engine) string {
	return domain.ConnectHost(addr, endpointBackendFor(eng))
}

func dockerBindingConnectHost(b boundPort, eng engine) (string, error) {
	return domain.BindingConnectHost(domainPortBinding(b), endpointBackendFor(eng))
}
