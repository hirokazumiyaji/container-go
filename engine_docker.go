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
)

func (dockerEngine) name() string   { return "docker" }
func (dockerEngine) binary() string { return "docker" }
func (dockerEngine) directIP() bool { return false }

func (dockerEngine) requiresImmutableID() bool { return true }

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
		Args: []string{"version", "--format", "{{.Server.Version}}"},
		Hint: "start the Docker daemon",
	}
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

func (dockerEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

// dockerInspect mirrors the fields of `docker inspect` output this
// library reads. Unknown fields are ignored.
type dockerInspect struct {
	ID       string `json:"Id"`
	Name     string `json:"Name"`
	Platform string `json:"Platform"`
	// ImageManifestDescriptor is the complete OCI platform metadata on
	// Docker versions that expose it. The top-level Platform field is
	// frequently only the OS (for example, "linux").
	ImageManifestDescriptor struct {
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"ImageManifestDescriptor"`

	descriptorOSPresent      bool
	descriptorArchPresent    bool
	descriptorVariantPresent bool
	State                    struct {
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

func (c *dockerInspect) UnmarshalJSON(data []byte) error {
	type plain dockerInspect
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = dockerInspect(decoded)
	var raw struct {
		ImageManifestDescriptor struct {
			Platform map[string]json.RawMessage `json:"platform"`
		} `json:"ImageManifestDescriptor"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, c.descriptorOSPresent = raw.ImageManifestDescriptor.Platform["os"]
	_, c.descriptorArchPresent = raw.ImageManifestDescriptor.Platform["architecture"]
	_, c.descriptorVariantPresent = raw.ImageManifestDescriptor.Platform["variant"]
	return nil
}

func (dockerEngine) parseInspect(data []byte, target string) (*engineInfo, error) {
	var containers []dockerInspect
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	match := -1
	for i, c := range containers {
		if dockerIDRE.MatchString(target) {
			if c.ID == target {
				match = i
				break
			}
			continue
		}
		if strings.TrimPrefix(c.Name, "/") == target {
			match = i
			break
		}
	}
	if match < 0 {
		return nil, fmt.Errorf("%w: container %s not in inspect output", ErrContainerNotFound, target)
	}
	c := containers[match]
	if !dockerIDRE.MatchString(c.ID) {
		return nil, fmt.Errorf("docker inspect for %s returned invalid container ID %q", target, c.ID)
	}

	platformMeta := platformMetadataFromString(c.Platform)
	descriptor := c.ImageManifestDescriptor.Platform
	descriptorMeta := platformMetadata{
		valid:        true,
		osSet:        c.descriptorOSPresent,
		archSet:      c.descriptorArchPresent,
		variantSet:   c.descriptorVariantPresent,
		os:           descriptor.OS,
		architecture: descriptor.Architecture,
		variant:      descriptor.Variant,
	}
	// A descriptor may omit OS on some API responses; the top-level value
	// remains the authoritative OS in that case. Do not discard an
	// explicitly empty architecture/variant: retaining its presence bit is
	// what makes architecture-specific selectors fail closed.
	if descriptorMeta.osSet {
		if descriptorMeta.os == "" && platformMeta.osSet {
			descriptorMeta.os = platformMeta.os
		}
		platformMeta.os = descriptorMeta.os
		platformMeta.osSet = true
	}
	if descriptorMeta.archSet {
		platformMeta.architecture = descriptorMeta.architecture
		platformMeta.archSet = true
	}
	if descriptorMeta.variantSet {
		platformMeta.variant = descriptorMeta.variant
		platformMeta.variantSet = true
	}
	if platformMeta.valid || c.descriptorOSPresent || c.descriptorArchPresent || c.descriptorVariantPresent {
		platformMeta.valid = true
	}

	info := &engineInfo{
		state:        dockerState(c.State.Status),
		labels:       c.Config.Labels,
		uid:          c.ID,
		image:        c.Config.Image,
		platform:     platformMeta.normalized(),
		platformMeta: platformMeta,
		ip:           c.NetworkSettings.IPAddress,
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
// status filters directly. Repeated status filters are alternatives, so
// both exited and dead containers are selected without admitting running
// or created containers.
func (dockerEngine) listArgs() []string {
	return []string{
		"ps", "--all", "--no-trunc",
		"--filter", "label=" + managedLabel + "=true",
		"--filter", "status=exited",
		"--filter", "status=dead",
		"--format", "{{.ID}}",
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

func (dockerEngine) platformCompatible(selector, actual string) bool {
	return dockerPlatformMatches(selector, actual)
}

func (dockerEngine) listReuseGroupArgs(group string) []string {
	return []string{
		"ps", "--all", "--no-trunc",
		"--filter", "label=" + reuseGroupLabel + "=" + group,
		"--format", "{{.ID}}",
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
