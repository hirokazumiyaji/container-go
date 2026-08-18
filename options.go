package container

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// Option configures Run.
type Option func(*config) error

type config struct {
	runner     cli.Runner
	name       string
	env        map[string]string
	cmd        []string
	entrypoint string
	exposed    []portSpec
	published  []publishSpec
	labels     map[string]string
	mounts       []Mount
	files        []File
	waitStrategy wait.Strategy
	cpus       int
	memory     string
	user       string
	workdir    string
	network    string
	platform   string
}

func newConfig() *config {
	return &config{
		runner: &cli.ExecRunner{},
		env:    map[string]string{},
		labels: map[string]string{},
	}
}

// nameRE is Apple Container's container name rule; the name doubles as
// the container ID.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// labelKeyRE is the Docker-style label key rule the CLI enforces,
// extended with slash-separated OCI segments.
var labelKeyRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:[./][a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)

// imageRE keeps image references to registry-reference characters and,
// critically, rejects a leading dash so an image can never be parsed as
// a CLI flag.
var imageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]*$`)

func withRunner(r cli.Runner) Option {
	return func(c *config) error {
		c.runner = r
		return nil
	}
}

// WithName sets the container name, which Apple Container uses as the
// container ID. Default is containergo-<random hex>.
func WithName(name string) Option {
	return func(c *config) error {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("invalid container name %q: must match %s", name, nameRE)
		}
		c.name = name
		return nil
	}
}

// WithEnv adds environment variables. They are passed to the CLI via a
// temporary env file so values never appear in the process table.
func WithEnv(env map[string]string) Option {
	return func(c *config) error {
		for k, v := range env {
			if k == "" || strings.ContainsAny(k, "=\n\x00") {
				return fmt.Errorf("invalid environment variable name %q", k)
			}
			if strings.ContainsAny(v, "\n\x00") {
				return fmt.Errorf("environment variable %s: value must not contain newlines", k)
			}
			c.env[k] = v
		}
		return nil
	}
}

// WithCmd overrides the arguments passed to the image's entrypoint.
func WithCmd(cmd ...string) Option {
	return func(c *config) error {
		c.cmd = cmd
		return nil
	}
}

// WithEntrypoint overrides the image entrypoint.
func WithEntrypoint(entrypoint string) Option {
	return func(c *config) error {
		c.entrypoint = entrypoint
		return nil
	}
}

// WithExposedPorts declares the container ports ("6379/tcp" or "6379")
// that MappedPort and Endpoint may resolve.
func WithExposedPorts(ports ...string) Option {
	return func(c *config) error {
		for _, p := range ports {
			spec, err := parsePortSpec(p)
			if err != nil {
				return err
			}
			c.exposed = append(c.exposed, spec)
		}
		return nil
	}
}

// WithPublishedPort publishes a container port on the host
// ("[host-ip:]host-port:container-port[/proto]"). Without it, endpoints
// resolve to the container's own IP, which needs no host port at all.
func WithPublishedPort(spec string) Option {
	return func(c *config) error {
		ps, err := parsePublishSpec(spec)
		if err != nil {
			return err
		}
		c.published = append(c.published, ps)
		return nil
	}
}

// WithLabels adds labels on top of the session labels the library
// always applies.
func WithLabels(labels map[string]string) Option {
	return func(c *config) error {
		for k, v := range labels {
			if len(k) > 128 || !labelKeyRE.MatchString(k) {
				return fmt.Errorf("invalid label key %q", k)
			}
			if len(k)+len(v)+1 > 4096 {
				return fmt.Errorf("label %s: key=value exceeds 4096 bytes", k)
			}
			if strings.ContainsAny(v, "\x00") {
				return fmt.Errorf("label %s: value must not contain NUL", k)
			}
			c.labels[k] = v
		}
		return nil
	}
}

// WithMounts adds bind, volume, or tmpfs mounts.
func WithMounts(mounts ...Mount) Option {
	return func(c *config) error {
		for _, m := range mounts {
			if err := m.validate(); err != nil {
				return err
			}
			c.mounts = append(c.mounts, m)
		}
		return nil
	}
}

// WithCPUs sets the number of CPUs for the container's VM.
func WithCPUs(n int) Option {
	return func(c *config) error {
		if n < 1 {
			return fmt.Errorf("cpus must be >= 1, got %d", n)
		}
		c.cpus = n
		return nil
	}
}

// WithMemory sets the VM memory size, e.g. "512M" or "1G".
func WithMemory(size string) Option {
	return func(c *config) error {
		if !regexp.MustCompile(`^[0-9]+[KMGTP]?$`).MatchString(size) {
			return fmt.Errorf("invalid memory size %q", size)
		}
		c.memory = size
		return nil
	}
}

// WithUser sets the user ("name" or "uid[:gid]") for the init process.
func WithUser(u string) Option {
	return func(c *config) error {
		c.user = u
		return nil
	}
}

// WithWorkingDir sets the working directory of the init process.
func WithWorkingDir(dir string) Option {
	return func(c *config) error {
		c.workdir = dir
		return nil
	}
}

// WithNetwork attaches the container to a named network instead of
// "default".
func WithNetwork(name string) Option {
	return func(c *config) error {
		c.network = name
		return nil
	}
}

// WithPlatform selects the image platform, e.g. "linux/amd64" (runs via
// Rosetta).
func WithPlatform(p string) Option {
	return func(c *config) error {
		c.platform = p
		return nil
	}
}

// MountType selects how a Mount is backed.
type MountType int

const (
	// MountBind mounts a host directory (virtiofs).
	MountBind MountType = iota
	// MountVolume mounts a named volume.
	MountVolume
	// MountTmpfs mounts an in-memory filesystem.
	MountTmpfs
)

// Mount describes one filesystem mount.
type Mount struct {
	Type     MountType
	Source   string // host path (bind) or volume name (volume); empty for tmpfs
	Target   string // absolute path inside the container
	ReadOnly bool
}

func (m Mount) validate() error {
	if strings.ContainsAny(m.Source, ",=\x00") || strings.ContainsAny(m.Target, ",=\x00") {
		return fmt.Errorf("mount %q -> %q: paths must not contain ',' or '='", m.Source, m.Target)
	}
	if !strings.HasPrefix(m.Target, "/") {
		return fmt.Errorf("mount target %q must be absolute", m.Target)
	}
	switch m.Type {
	case MountBind:
		if !strings.HasPrefix(m.Source, "/") {
			return fmt.Errorf("bind mount source %q must be an absolute host path", m.Source)
		}
	case MountVolume:
		if m.Source == "" {
			return fmt.Errorf("volume mount for %q needs a volume name: anonymous volumes are not cleaned up by --rm", m.Target)
		}
	case MountTmpfs:
		if m.Source != "" {
			return fmt.Errorf("tmpfs mount for %q must not have a source", m.Target)
		}
	}
	return nil
}

func (m Mount) arg() string {
	var b strings.Builder
	switch m.Type {
	case MountBind:
		b.WriteString("type=bind,source=" + m.Source)
	case MountVolume:
		b.WriteString("type=volume,source=" + m.Source)
	case MountTmpfs:
		b.WriteString("type=tmpfs")
	}
	b.WriteString(",target=" + m.Target)
	if m.ReadOnly {
		b.WriteString(",readonly")
	}
	return b.String()
}

type portSpec struct {
	port  int
	proto string
}

func (p portSpec) String() string { return strconv.Itoa(p.port) + "/" + p.proto }

func parsePortSpec(s string) (portSpec, error) {
	portPart, proto, ok := strings.Cut(s, "/")
	if !ok {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return portSpec{}, fmt.Errorf("invalid port %q: protocol must be tcp or udp", s)
	}
	n, err := strconv.Atoi(portPart)
	if err != nil || n < 1 || n > 65535 {
		return portSpec{}, fmt.Errorf("invalid port %q: port must be 1-65535", s)
	}
	return portSpec{port: n, proto: proto}, nil
}

type publishSpec struct {
	hostAddr      string
	hostPort      int
	containerPort int
	proto         string
	raw           string
}

func parsePublishSpec(s string) (publishSpec, error) {
	spec := publishSpec{raw: s, proto: "tcp"}
	rest := s
	if portsPart, proto, ok := strings.Cut(s, "/"); ok {
		if proto != "tcp" && proto != "udp" {
			return publishSpec{}, fmt.Errorf("invalid publish spec %q: protocol must be tcp or udp", s)
		}
		spec.proto = proto
		rest = portsPart
	}

	if strings.HasPrefix(rest, "[") {
		// Bracketed IPv6 host: [addr]:host-port:container-port
		end := strings.Index(rest, "]:")
		if end < 0 {
			return publishSpec{}, fmt.Errorf("invalid publish spec %q", s)
		}
		spec.hostAddr = rest[1:end]
		rest = rest[end+2:]
	} else if parts := strings.Split(rest, ":"); len(parts) == 3 {
		spec.hostAddr = parts[0]
		rest = parts[1] + ":" + parts[2]
	}

	hostPart, ctrPart, ok := strings.Cut(rest, ":")
	if !ok || strings.Contains(ctrPart, ":") {
		return publishSpec{}, fmt.Errorf("invalid publish spec %q: want [host-ip:]host-port:container-port[/proto]", s)
	}
	var err error
	if spec.hostPort, err = parsePortNumber(hostPart); err != nil {
		return publishSpec{}, fmt.Errorf("invalid publish spec %q: %w", s, err)
	}
	if spec.containerPort, err = parsePortNumber(ctrPart); err != nil {
		return publishSpec{}, fmt.Errorf("invalid publish spec %q: %w", s, err)
	}
	return spec, nil
}

func parsePortNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %q must be 1-65535", s)
	}
	return n, nil
}

// connectAddr is the address clients should dial for a published port.
// An unspecified bind address is reachable via loopback.
func (p publishSpec) connectAddr() string {
	if p.hostAddr == "" || p.hostAddr == "0.0.0.0" || p.hostAddr == "::" {
		return "127.0.0.1"
	}
	return p.hostAddr
}
