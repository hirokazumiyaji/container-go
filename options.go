package container

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// Option configures Run.
type Option func(*config) error

const (
	// maxMemoryBytes is a portable, backend-neutral ceiling. Individual
	// backends may enforce a smaller capability limit in checkConfig.
	maxMemoryBytes     uint64 = 1 << 50 // 1 PiB
	maxVolumeNameBytes        = 255
	maxMountPathBytes         = 4096
	maxReuseGroupBytes        = 128
)

type config struct {
	runner       cli.Runner
	eng          engine
	name         string
	env          map[string]string
	cmd          []string
	entrypoint   string
	exposed      []portSpec
	published    []publishSpec
	labels       map[string]string
	mounts       []Mount
	files        []File
	waitStrategy wait.Strategy
	cpus         int
	memory       string
	user         string
	workdir      string
	network      string
	platform     string
	pullPolicy   PullPolicy
	reuse        bool
	reuseGroup   string
	creation     string
}

func newConfig() *config {
	return &config{
		runner: &cli.ExecRunner{},
		env:    map[string]string{},
		labels: map[string]string{},
	}
}

// validate checks invariants that may be assembled by multiple options.
// Collection cardinality is intentionally left to the backend and caller;
// there is no universal item-count policy for public options.
func (c *config) validate() error {
	if c.memory != "" {
		if err := validateMemorySize(c.memory); err != nil {
			return err
		}
	}
	if c.reuseGroup != "" {
		if err := validateReuseGroup(c.reuseGroup); err != nil {
			return err
		}
	}
	for _, m := range c.mounts {
		if err := m.validate(); err != nil {
			return err
		}
	}
	for _, f := range c.files {
		if err := validateContainerPath(f.ContainerPath); err != nil {
			return newValidationError("WithFiles", f, err)
		}
	}
	return nil
}

// allLabels merges the session labels the library always applies with
// user-supplied ones. Internal labels always win so callers cannot
// override the generation used for safe cleanup.
func (c *config) allLabels() map[string]string {
	labels := map[string]string{}
	for k, v := range c.labels {
		labels[k] = v
	}
	labels[managedLabel] = "true"
	labels[sessionLabel] = sessionID()
	if c.creation != "" {
		labels[creationLabel] = c.creation
	}
	if c.creation != "" {
		labels[creationLabel] = c.creation
	}
	if c.reuse {
		labels[reuseLabel] = "true"
	}
	if c.reuseGroup != "" {
		labels[reuseGroupLabel] = c.reuseGroup
	}
	return labels
}

// commonRunArgs builds the shared run flags after --name: labels,
// env-file, publish (explicit then extras), mounts, resources,
// entrypoint, image, and cmd. Engines supply only their prefix and any
// backend-specific publish entries (Docker auto-publish).
func (c *config) commonRunArgs(image, envFile string, extraPublish []string) []string {
	var args []string
	labels := c.allLabels()
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, p := range c.published {
		args = append(args, "--publish", p.raw)
	}
	for _, raw := range extraPublish {
		args = append(args, "--publish", raw)
	}
	for _, m := range c.mounts {
		if arg := m.arg(); arg != "" {
			args = append(args, "--mount", arg)
		}
	}
	if c.cpus > 0 {
		args = append(args, "--cpus", strconv.Itoa(c.cpus))
	}
	if c.memory != "" {
		args = append(args, "--memory", c.memory)
	}
	if c.user != "" {
		args = append(args, "--user", c.user)
	}
	if c.workdir != "" {
		args = append(args, "--workdir", c.workdir)
	}
	if c.network != "" {
		args = append(args, "--network", c.network)
	}
	if c.platform != "" {
		args = append(args, "--platform", c.platform)
	}
	if c.entrypoint != "" {
		args = append(args, "--entrypoint", c.entrypoint)
	}
	args = append(args, image)
	return append(args, c.cmd...)
}

// WithReuse enables process- and cross-process get-or-create for a
// stable WithName. Concurrent Run calls with the same name share one
// container; readiness strategies always re-run against it. Returned
// handles are shared: Cleanup, TerminateContainer, and the watchdog
// reaper do not remove them. Explicit Terminate still does — only use
// it when no other process still needs the container.
func WithReuse() Option {
	return func(c *config) error {
		c.reuse = true
		return nil
	}
}

// WithReuseGroup tags a reused container for later PruneReuseGroup.
// The group is not part of the reuse key; WithName alone identifies the
// shared container. Requires WithReuse.
func WithReuseGroup(group string) Option {
	return func(c *config) error {
		if err := validateReuseGroup(group); err != nil {
			return err
		}
		c.reuseGroup = group
		return nil
	}
}

// nameRE is Apple Container's container name rule; the name doubles as
// the container ID.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// labelKeyRE is the Docker-style label key rule the CLI enforces,
// extended with slash-separated OCI segments.
var labelKeyRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:[./][a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)

func validateReuseGroup(group string) error {
	return validateReuseGroupFor("WithReuseGroup", group)
}

func validateReuseGroupFor(option, group string) error {
	if group == "" {
		return newValidationErrorWithField(option, "group", group, fmt.Errorf("reuse group must not be empty"))
	}
	if len(group) > maxReuseGroupBytes || !labelKeyRE.MatchString(group) {
		return newValidationErrorWithField(option, "group", group, fmt.Errorf("invalid reuse group %q", group))
	}
	return nil
}

// imageRE keeps image references to registry-reference characters and,
// critically, rejects a leading dash so an image can never be parsed as
// a CLI flag.
var imageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]*$`)

func validateImageReference(image string) error {
	if !imageRE.MatchString(image) {
		return validationErrorf("image", image, "invalid image reference %q", image)
	}
	return nil
}

func withRunner(r cli.Runner) Option {
	return func(c *config) error {
		c.runner = r
		return nil
	}
}

func withEngine(e engine) Option {
	return func(c *config) error {
		c.eng = e
		return nil
	}
}

// WithName sets the container name, which Apple Container uses as the
// container ID. Default is containergo-<random hex>.
func WithName(name string) Option {
	return func(c *config) error {
		if !nameRE.MatchString(name) {
			return validationErrorf("WithName", name, "invalid container name %q: must match %s", name, nameRE)
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
				return validationErrorf("WithEnv", k, "invalid environment variable name %q", k)
			}
			if strings.ContainsAny(v, "\n\x00") {
				return validationErrorf("WithEnv", k, "environment variable %s: value must not contain newlines", k)
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

// WithEntrypoint overrides the image entrypoint. It is a single token
// per `docker run --entrypoint` semantics; for multi-token commands use
// WithCmd.
func WithEntrypoint(entrypoint string) Option {
	return func(c *config) error {
		if entrypoint == "" || strings.HasPrefix(entrypoint, "-") || strings.ContainsAny(entrypoint, "\n\x00") {
			return validationErrorf("WithEntrypoint", entrypoint, "invalid entrypoint %q", entrypoint)
		}
		c.entrypoint = entrypoint
		return nil
	}
}

// WithExposedPorts declares the container ports ("6379/tcp" or "6379")
// that MappedPort and Endpoint may resolve.
func WithExposedPorts(ports ...string) Option {
	return func(c *config) error {
		specs := make([]portSpec, 0, len(ports))
		for _, p := range ports {
			spec, err := parsePortSpec(p)
			if err != nil {
				return newValidationError("WithExposedPorts", p, err)
			}
			specs = append(specs, spec)
		}
		c.exposed = append(c.exposed, specs...)
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
			return newValidationError("WithPublishedPort", spec, err)
		}
		c.published = append(c.published, ps)
		return nil
	}
}

// WithLabels adds labels on top of the session labels the library
// always applies. Internal labels are reserved and rejected.
func WithLabels(labels map[string]string) Option {
	return func(c *config) error {
		for k, v := range labels {
			if len(k) > 128 || !labelKeyRE.MatchString(k) {
				return validationErrorf("WithLabels", k, "invalid label key %q", k)
			}
			switch k {
			case managedLabel, sessionLabel, reuseLabel, reuseGroupLabel, creationLabel:
				return validationErrorf("WithLabels", k, "label key %q is reserved", k)
			}
			if len(k)+len(v)+1 > 4096 {
				return validationErrorf("WithLabels", k, "label %s: key=value exceeds 4096 bytes", k)
			}
			if strings.ContainsAny(v, "\x00") {
				return validationErrorf("WithLabels", k, "label %s: value must not contain NUL", k)
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
		}
		c.mounts = append(c.mounts, mounts...)
		return nil
	}
}

// WithCPUs sets the number of CPUs for the container's VM.
func WithCPUs(n int) Option {
	return func(c *config) error {
		if n < 1 {
			return validationErrorf("WithCPUs", n, "cpus must be >= 1, got %d", n)
		}
		c.cpus = n
		return nil
	}
}

// memoryRE accepts sizes like "512M" or "1G". Unit support beyond
// this syntax is a backend capability and is checked separately.
var memoryRE = regexp.MustCompile(`^[0-9]+[KMGTP]?$`)

func validateMemorySize(size string) error {
	if !memoryRE.MatchString(size) {
		return validationErrorf("WithMemory", size, "invalid memory size %q", size)
	}

	digits := size
	unit := uint64(1)
	if last := size[len(size)-1]; last < '0' || last > '9' {
		digits = size[:len(size)-1]
		switch last {
		case 'K':
			unit = 1 << 10
		case 'M':
			unit = 1 << 20
		case 'G':
			unit = 1 << 30
		case 'T':
			unit = 1 << 40
		case 'P':
			unit = 1 << 50
		}
	}
	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return newValidationError("WithMemory", size, fmt.Errorf("invalid or overflowing memory size %q: %w", size, err))
	}
	if value == 0 {
		return validationErrorf("WithMemory", size, "memory size must be greater than zero: %q", size)
	}
	if value > maxMemoryBytes/unit {
		return validationErrorf("WithMemory", size, "memory size overflows the %d-byte maximum: %q", maxMemoryBytes, size)
	}
	return nil
}

// WithMemory sets the VM memory size, e.g. "512M" or "1G".
func WithMemory(size string) Option {
	return func(c *config) error {
		if err := validateMemorySize(size); err != nil {
			return err
		}
		c.memory = size
		return nil
	}
}

// userRE allows "name", "uid", or "uid:gid" style users.
var userRE = regexp.MustCompile(`^[a-zA-Z0-9._][a-zA-Z0-9._-]*(:[a-zA-Z0-9._-]+)?$`)

// WithUser sets the user ("name" or "uid[:gid]") for the init process.
func WithUser(u string) Option {
	return func(c *config) error {
		if !userRE.MatchString(u) {
			return validationErrorf("WithUser", u, "invalid user %q", u)
		}
		c.user = u
		return nil
	}
}

// WithWorkingDir sets the working directory of the init process.
func WithWorkingDir(dir string) Option {
	return func(c *config) error {
		if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\n\x00") {
			return validationErrorf("WithWorkingDir", dir, "working directory %q must be an absolute path", dir)
		}
		c.workdir = dir
		return nil
	}
}

// WithNetwork attaches the container to a named network instead of
// "default".
func WithNetwork(name string) Option {
	return func(c *config) error {
		if !nameRE.MatchString(name) {
			return validationErrorf("WithNetwork", name, "invalid network name %q", name)
		}
		c.network = name
		return nil
	}
}

// platformRE matches "os", "os/arch", or "os/arch/variant".
var platformRE = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9_-]+){0,2}$`)

// WithPlatform selects the image platform, e.g. "linux/amd64" (runs via
// Rosetta).
func WithPlatform(p string) Option {
	return func(c *config) error {
		if !platformRE.MatchString(p) {
			return validationErrorf("WithPlatform", p, "invalid platform %q", p)
		}
		c.platform = p
		return nil
	}
}

// MountType selects how a Mount is backed.
type MountType int

// volumeNameRE matches the portable Docker/OCI volume-name grammar:
// one leading alphanumeric followed by name characters. Backend-specific
// volume capability checks remain in each engine.
var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

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

func mountValidationErrorf(m Mount, format string, args ...any) error {
	return newValidationErrorWithField("WithMounts", "mount", m, fmt.Errorf(format, args...))
}

func (m Mount) validate() error {
	if m.Type < MountBind || m.Type > MountTmpfs {
		return mountValidationErrorf(m, "unknown mount type %d", m.Type)
	}
	if len(m.Source) > maxMountPathBytes || len(m.Target) > maxMountPathBytes {
		return mountValidationErrorf(m, "mount paths exceed the %d-byte maximum", maxMountPathBytes)
	}
	if !utf8.ValidString(m.Source) || !utf8.ValidString(m.Target) {
		return mountValidationErrorf(m, "mount paths must be valid UTF-8")
	}
	if strings.ContainsAny(m.Source, ",=\x00") || strings.ContainsAny(m.Target, ",=\x00") {
		return mountValidationErrorf(m, "mount %q -> %q: paths must not contain ',' or '='", m.Source, m.Target)
	}
	if strings.ContainsAny(m.Source, "\r\n") || strings.ContainsAny(m.Target, "\r\n") {
		return mountValidationErrorf(m, "mount %q -> %q: paths must not contain control characters", m.Source, m.Target)
	}
	if !strings.HasPrefix(m.Target, "/") {
		return mountValidationErrorf(m, "mount target %q must be absolute", m.Target)
	}
	switch m.Type {
	case MountBind:
		if !strings.HasPrefix(m.Source, "/") {
			return mountValidationErrorf(m, "bind mount source %q must be an absolute host path", m.Source)
		}
	case MountVolume:
		if m.Source == "" {
			return mountValidationErrorf(m, "volume mount for %q needs a volume name: anonymous volumes are not cleaned up by --rm", m.Target)
		}
		if len(m.Source) > maxVolumeNameBytes {
			return mountValidationErrorf(m, "volume name exceeds the %d-byte maximum: %q", maxVolumeNameBytes, m.Source)
		}
		if !volumeNameRE.MatchString(m.Source) {
			return mountValidationErrorf(m, "invalid volume name %q", m.Source)
		}
	case MountTmpfs:
		if m.Source != "" {
			return mountValidationErrorf(m, "tmpfs mount for %q must not have a source", m.Target)
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
	default:
		// Run validates mounts before argv construction. Keep a direct
		// call from producing a partial --mount value if that invariant
		// is ever bypassed by internal code.
		return ""
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
	if spec.hostAddr != "" {
		if _, err := netip.ParseAddr(spec.hostAddr); err != nil {
			return publishSpec{}, fmt.Errorf("invalid publish spec %q: host address must be an IP: %w", s, err)
		}
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
