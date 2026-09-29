package container

import (
	"encoding/json"
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
//   - Windows daemon down: "error during connect: open \\.\pipe\docker_engine: The system cannot find the file specified."
const (
	dockerStderrConflict     = "conflict. the container name "
	dockerStderrAlreadyInUse = "already in use by container"
	dockerStderrNoSuchImage  = "no such image:"
	dockerStderrNoSuchObj    = "no such object:"
	dockerStderrNoSuchCtr    = "no such container:"
)

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
		IsUnavailable: dockerProbeUnavailable,
	}
}

func dockerProbeUnavailable(err error) bool {
	text, ok := backendCLIErrorText(err, "docker", "version")
	if !ok {
		return false
	}
	// A reachable daemon can fail the client for TLS, certificate, SSH,
	// proxy, authentication, or endpoint-configuration reasons. None of
	// those failures prove that the daemon is stopped.
	if cli.IsProbeConfigurationError(err) {
		return false
	}
	if dockerWindowsNpipeUnavailable(text) {
		return true
	}
	for _, fragment := range []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"docker desktop is unable to start",
		"connection refused",
	} {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

// Windows reports a stopped daemon as a missing named pipe rather than a
// refused connection, so "error during connect" on its own is not usable
// evidence: the same prefix introduces TLS, SSH, and proxy failures.
// Require both halves of the real diagnostic instead — the Docker client's
// own pipe namespace plus the OS error that opening it produced. Any other
// pipe, or any other file-not-found, stays unclassified.
func dockerWindowsNpipeUnavailable(text string) bool {
	const missingFile = "the system cannot find the file specified"
	if !strings.Contains(text, missingFile) {
		return false
	}
	// Go's npipe package reports the forward-slash form while the Windows
	// CLI spells the same path with backslashes.
	normalized := strings.ReplaceAll(text, `\`, "/")
	return strings.Contains(normalized, "//./pipe/docker") ||
		strings.Contains(normalized, "getnamedpipeinfo")
}

// defaultHost honors a tcp:// DOCKER_HOST (remote daemon); everything
// else publishes on loopback. Note: a `docker context` pointing at a
// remote daemon is not detected; only DOCKER_HOST is honored.
func (dockerEngine) defaultHost() string {
	if raw := os.Getenv("DOCKER_HOST"); strings.HasPrefix(raw, "tcp://") {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return "127.0.0.1"
}

// isRemoteDocker reports whether DOCKER_HOST points at a non-loopback
// tcp daemon. Auto-publish must bind 0.0.0.0 there; a 127.0.0.1 bind on
// the remote host is unreachable from the client.
func isRemoteDockerHost() bool {
	return !isLoopbackOrUnspecified((dockerEngine{}).defaultHost())
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
	// remote daemon (tcp:// DOCKER_HOST) it binds all interfaces so
	// the client can reach it via defaultHost().
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

func (dockerEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

// dockerInspect mirrors the fields of `docker inspect` output this
// library reads. Unknown fields are ignored.
type dockerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
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
	var containers []dockerInspect
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(containers) == 0 {
		return nil, &inspectTargetNotFoundError{id: id}
	}
	c := containers[0]

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
	ctx, ok := backendCLIError(err, "docker")
	if !ok || ctx.operation != "image inspect" || !hasParsedCLITarget(ctx) {
		return false
	}
	return hasCLIErrorLine(err, func(line string) bool {
		rest, ok := strings.CutPrefix(line, dockerStderrNoSuchImage)
		return ok && cliTargetListMatches(rest, ctx.target)
	})
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
	ctx, ok := backendCLIError(err, "docker")
	if !ok || ctx.operation != "run" || !hasParsedCLITarget(ctx) {
		return false
	}
	return hasCLIErrorLine(err, func(line string) bool {
		return dockerNameConflictLine(line, ctx.target)
	})
}

func dockerNameConflictLine(line, target string) bool {
	if strings.TrimSpace(target) == "" || !strings.HasPrefix(line, dockerStderrConflict) ||
		!strings.Contains(line, dockerStderrAlreadyInUse) {
		return false
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
	ctx, ok := backendCLIError(err, "docker")
	if !ok || !hasParsedCLITarget(ctx) {
		return false
	}
	prefix := ""
	switch ctx.operation {
	case "inspect":
		prefix = dockerStderrNoSuchObj
	case "exec", "stop", "rm", "logs":
		prefix = dockerStderrNoSuchCtr
	default:
		return false
	}
	return hasCLIErrorLine(err, func(line string) bool {
		rest, ok := strings.CutPrefix(line, prefix)
		return ok && cliTargetListMatches(rest, ctx.target)
	})
}
