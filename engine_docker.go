package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// dockerEngine drives the `docker` CLI. Unlike Apple Container, the
// container IP is generally not reachable from the host (Docker
// Desktop), so exposed ports are published to daemon-assigned loopback
// ports and endpoints resolve to those.
type dockerEngine struct{}

// Verified against Docker Engine / CLI 29.x (local: 29.7.2).
// Matchers are command- and binary-aware. Docker's inspect path uses
// "no such object", while exec/stop/rm/logs use "no such container".
// Generic application/configuration wording is deliberately not a match.
// Observed wording:
//   - name conflict: "Conflict. The container name \"/x\" is already in use by container …"
//   - image missing: "Error response from daemon: No such image: …"
//   - inspect missing: "error: no such object: …"
//   - lifecycle missing: "Error response from daemon: No such container: …"
const (
	dockerStderrConflict     = "conflict. the container name "
	dockerStderrAlreadyInUse = "already in use by container"
	dockerStderrNoSuchImage  = "no such image:"
	dockerStderrNoSuchObj    = "no such object:"
	dockerStderrNoSuchCtr    = "no such container:"
)

var errInvalidDockerInspect = errors.New("invalid docker container inspect output")

var dockerDesktopUnableToStartRE = regexp.MustCompile(`(?i)^(error response from daemon: )?docker desktop is unable to start\b`)

func (dockerEngine) name() string   { return "docker" }
func (dockerEngine) binary() string { return "docker" }
func (dockerEngine) directIP() bool { return false }

// checkConfig rejects explicit loopback publish binds on a remote
// daemon: Docker would listen on the remote machine's loopback, which
// no rewrite of the client-facing address can make reachable.
func (dockerEngine) checkConfig(cfg *config) error {
	if !isRemoteDockerHost() {
		return nil
	}
	// hostAddr is validated as an IP literal by parsePublishSpec.
	for _, p := range cfg.published {
		if p.hostAddr != "" && net.ParseIP(p.hostAddr).IsLoopback() {
			return fmt.Errorf("published port %q binds loopback on a remote DOCKER_HOST and would be unreachable", p.raw)
		}
	}
	return nil
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
		"connection refused",
	} {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func dockerDesktopStartupFailure(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if dockerDesktopUnableToStartRE.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
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
	if isRemoteDockerHost() {
		if host := dockerHostName(); host != "" {
			return host
		}
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
// must be rewritten to defaultHost() on a remote daemon. IP literals
// use net.IP.IsLoopback / IsUnspecified so the full 127.0.0.0/8 and
// ::1 ranges are covered, not only a few spellings.
func isLoopbackOrUnspecified(addr string) bool {
	if addr == "" || strings.EqualFold(addr, "localhost") {
		return true
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsUnspecified()
}

// dockerConnectHost rewrites binds to the client-facing host. On a
// remote daemon, loopback and unspecified addresses become
// defaultHost(). Locally, unspecified binds still map to defaultHost(),
// but an explicit loopback (127.0.0.1, ::1, …) is preserved so an
// IPv6-only published port stays reachable.
func dockerConnectHost(addr string, eng engine) string {
	if isRemoteDockerHost() {
		if isLoopbackOrUnspecified(addr) {
			return eng.defaultHost()
		}
		return addr
	}
	switch addr {
	case "", "0.0.0.0", "::":
		return eng.defaultHost()
	}
	return addr
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
	var extraPublish []string
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
	return append(args, cfg.commonRunArgs(image, envFile, extraPublish)...)
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
	return []string{"inspect", "--type=container", id}
}

// dockerInspect mirrors the fields of `docker inspect` output this
// library reads. Unknown fields are ignored.
type dockerInspectState struct {
	Status string `json:"Status"`
}

type dockerInspect struct {
	ID     string              `json:"Id"`
	Name   string              `json:"Name"`
	State  *dockerInspectState `json:"State"`
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

func (dockerEngine) parseInspect(data []byte, id string) (*engineInfo, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, newInspectTargetNotFound(id, "empty inspect output")
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, fmt.Errorf("%w: expected a container array, got null", errInvalidDockerInspect)
	}
	var rawContainers []json.RawMessage
	if err := json.Unmarshal(data, &rawContainers); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	var c dockerInspect
	found := false
	for _, rawContainer := range rawContainers {
		var candidate dockerInspect
		if err := json.Unmarshal(rawContainer, &candidate); err != nil {
			return nil, fmt.Errorf("%w: invalid container entry: %v", errInvalidDockerInspect, err)
		}
		if !dockerIDRE.MatchString(candidate.ID) {
			return nil, fmt.Errorf("%w: container Id must be a canonical 64-hex value", errInvalidDockerInspect)
		}
		if candidate.State == nil || strings.TrimSpace(candidate.State.Status) == "" {
			return nil, fmt.Errorf("%w: container State.Status is required", errInvalidDockerInspect)
		}
		if dockerInspectTargetMatches(candidate, id) {
			c = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, newInspectTargetNotFound(id, "inspect output did not contain the requested target")
	}

	info := &engineInfo{
		state:  dockerState(c.State.Status),
		labels: c.Config.Labels,
		uid:    c.ID,
		image:  c.Config.Image,
		ip:     c.NetworkSettings.IPAddress,
	}
	if info.ip == "" {
		for _, n := range c.NetworkSettings.Networks {
			if n.IPAddress != "" {
				info.ip = n.IPAddress
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
				hostAddr:      b.HostIP,
				hostPort:      hostPort,
			})
		}
	}
	return info, nil
}

func dockerInspectTargetMatches(c dockerInspect, target string) bool {
	target = strings.TrimPrefix(strings.TrimSpace(target), "/")
	if target == "" {
		return false
	}
	if dockerTargetIsID(target) {
		return dockerIDMatches(c.ID, target)
	}
	if dockerIDMatches(c.ID, target) {
		return true
	}
	return strings.TrimPrefix(strings.TrimSpace(c.Name), "/") == target
}

func dockerTargetIsID(target string) bool {
	id := strings.TrimPrefix(strings.TrimSpace(target), "sha256:")
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func dockerIDMatches(got, want string) bool {
	return strings.EqualFold(
		strings.TrimPrefix(strings.TrimSpace(got), "sha256:"),
		strings.TrimPrefix(strings.TrimSpace(want), "sha256:"),
	)
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
		if branch.ctx.operation != "run" || !exactBranchTarget(branch, target) {
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
	if !strings.HasPrefix(line, dockerStderrConflict) ||
		!strings.Contains(line, dockerStderrAlreadyInUse) {
		return false
	}
	if target == "" {
		return true
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, dockerStderrConflict))
	if len(rest) < 2 || rest[0] != '"' {
		return false
	}
	rest = rest[1:]
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		rest = rest[:end]
	}
	return sameDockerContainerName(rest, target)
}

func sameDockerContainerName(got, want string) bool {
	got = strings.TrimPrefix(strings.Trim(strings.TrimSpace(got), `"'`), "/")
	want = strings.TrimPrefix(strings.Trim(strings.TrimSpace(want), `"'`), "/")
	return strings.EqualFold(got, want)
}

// containerMissing matches Docker's command-specific absent-container
// response. Docker uses "no such object" for inspect and "no such
// container" for the lifecycle/stream commands.
func (dockerEngine) containerMissing(err error) bool {
	for _, branch := range backendCLIErrorBranches(err, "docker") {
		prefix := ""
		switch branch.ctx.operation {
		case "inspect":
			prefix = dockerStderrNoSuchObj
		case "exec", "stop", "rm", "logs":
			prefix = dockerStderrNoSuchCtr
		default:
			continue
		}
		if hasBranchLine(branch, func(line string) bool {
			rest, ok := strings.CutPrefix(line, prefix)
			return ok && cliTargetListMatches(rest, branch.ctx.target)
		}) {
			return true
		}
	}
	return false
}
