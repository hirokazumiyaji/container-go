package container

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/portspec"
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
	runner          cli.Runner
	eng             engine
	name            string
	env             map[string]string
	cmd             []string
	entrypoint      string
	exposed         []portSpec
	published       []publishSpec
	labels          map[string]string
	mounts          []Mount
	files           []File
	waitStrategy    wait.Strategy
	cpus            int
	memory          string
	user            string
	workdir         string
	network         string
	networkExplicit bool
	platform        string
	pullPolicy      PullPolicy
	reuse           bool
	reuseGroup      string
	creation        string
	// imageCache reuses image-presence answers across Runs when the
	// caller applies a WithImagePresenceCache Option. The Option owns the
	// cache so sequential Runs that reuse it share entries. Nil means no
	// cache, which is the default and preserves one inspect per Run.
	imageCache *imageCache

	// imagePrepared is set when a reuse caller has completed its own
	// PullAlways fetch before entering the shared ensure flight.
	imagePrepared bool
	// reusedCreated is set only on the caller whose flight callback
	// created the container. Other callers still apply their own files
	// after attaching to the shared generation.
	reusedCreated bool
}

func newConfig() *config {
	return &config{
		runner: &cli.ExecRunner{},
		env:    map[string]string{},
		labels: map[string]string{},
	}
}

// validate checks invariants that depend on more than one option. Scalar
// option values are validated when their option is applied; keeping those
// checks there gives callers an error from the option itself. This second
// pass is intentionally limited to cross-option relationships and is called
// by Run after every option has been applied.
func (c *config) validate() error {
	if c.reuse && c.name == "" {
		return validationErrorf("WithReuse", nil, "WithReuse requires WithName")
	}
	if c.reuseGroup != "" && !c.reuse {
		return validationErrorf("WithReuseGroup", c.reuseGroup, "WithReuseGroup requires WithReuse")
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
	if c.networkExplicit && c.network != "" {
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
// container; readiness strategies always re-run against it. WithFiles
// is copied for every caller, and PullAlways is fetched for every
// caller before attach. Creation-only options are intentionally
// ignored when attaching; use distinct names when those differences
// matter. Returned handles are shared: Cleanup, TerminateContainer,
// and the watchdog reaper do not remove them. Explicit Terminate still
// does — only use it when no other process still needs the container.
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

// WithEnv adds environment variables. On Unix they are passed to the CLI via
// a per-user 0600 env file so values never appear in the process table. On
// Windows, Run returns ErrEnvFileUnsupported when this map is non-empty
// because chmod does not provide per-user file secrecy there.
//
// Keys must be non-empty valid UTF-8 without '=', Unicode whitespace or
// control characters, a leading '#', or a leading byte-order mark. Values must
// be valid UTF-8 without Unicode control characters, NUL, CR/LF, U+2028, or
// U+2029. Spaces, '=', and other non-control Unicode are valid in values.
// This common policy is intentionally stricter than values some backends may
// accept.
func WithEnv(env map[string]string) Option {
	return func(c *config) error {
		for _, k := range sortedKeys(env) {
			v := env[k]
			if err := validateEnvironmentEntry("WithEnv", "environment variable", k, v); err != nil {
				return err
			}
			c.env[k] = v
		}
		return nil
	}
}

func validateEnvironmentEntry(option, description, key, value string) error {
	if err := validateEnvKey(key); err != nil {
		return newValidationErrorWithField(option, "key", key,
			fmt.Errorf("invalid %s name %q: %w", description, key, err))
	}
	if err := validateEnvValue(value); err != nil {
		// Environment and exec values can contain credentials. Keep both
		// the value and its error text free of the rejected value.
		return newValidationErrorWithField(option, "value", nil,
			fmt.Errorf("%s %q has an invalid value: %w", description, key, err))
	}
	return nil
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
// that MappedPort and Endpoint may resolve. Docker auto-publishes these
// ports; host, none, internal, and isolated networks reject that
// combination before container creation.
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
// WithPublishedPort publishes a container port on the host
// ("[host-ip:]host-port:container-port[/proto]"). On Apple Container,
// endpoints resolve to the container's own IP when this is omitted; on
// Docker, WithExposedPorts auto-publishes instead. Docker rejects both
// publish forms on host, none, internal, and isolated networks.
// Publishing a port does not reorder WithExposedPorts declarations used by
// implicit wait probes.
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
				return newValidationErrorWithField("WithLabels", "key", k, fmt.Errorf("invalid label key %q", k))
			}
			switch k {
			case managedLabel, sessionLabel, reuseLabel, reuseGroupLabel, creationLabel:
				return newValidationErrorWithField("WithLabels", "key", k, fmt.Errorf("label key %q is reserved", k))
			}
			// Label values may carry secrets. Do not put them in Value or
			// the rendered diagnostic when the entry is rejected.
			if len(k)+len(v)+1 > 4096 {
				return newValidationErrorWithField("WithLabels", "value", nil,
					fmt.Errorf("label %q: key=value exceeds 4096 bytes", k))
			}
			if strings.ContainsAny(v, "\x00") {
				return newValidationErrorWithField("WithLabels", "value", nil,
					fmt.Errorf("label %q: value must not contain NUL", k))
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

// WithNetwork selects a network. Omitting it leaves Docker's
// daemon-selected default unchanged (bridge on Linux and nat on native
// Windows). Docker's "host" and "none" modes and externally isolated
// networks cannot be combined with WithExposedPorts or WithPublishedPort;
// the Docker backend rejects those combinations before creating the
// container.
func WithNetwork(name string) Option {
	return func(c *config) error {
		if !nameRE.MatchString(name) {
			return validationErrorf("WithNetwork", name, "invalid network name %q", name)
		}
		c.network = name
		c.networkExplicit = true
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

// volumeNameRE is the name grammar shared by both backends. Docker adds a
// minimum length requirement in dockerEngine.checkConfig; Apple accepts a
// single-character name.
var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

const (
	// MountBind mounts a host directory (virtiofs).
	MountBind MountType = iota
	// MountVolume mounts a named volume. Docker managed cleanup preserves
	// named volumes, including volumes backed by a custom driver.
	MountVolume
	// MountTmpfs mounts an in-memory filesystem.
	MountTmpfs
)

// Mount describes one filesystem mount.
type Mount struct {
	Type     MountType
	Source   string // host path in host OS syntax (bind) or volume name (volume); empty for tmpfs
	Target   string // absolute POSIX path inside the container
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
		if !filepath.IsAbs(m.Source) {
			return mountValidationErrorf(m, "bind mount source %q must be an absolute host path", m.Source)
		}
	case MountVolume:
		if m.Source == "" {
			return mountValidationErrorf(m, "volume mount for %q needs a volume name: anonymous volume lifecycle is backend-specific", m.Target)
		}
		// Docker's minimum length is checked in checkConfig because Apple
		// accepts one-character names.
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
	spec, err := portspec.Parse(s)
	if err != nil {
		return portSpec{}, err
	}
	return portSpec{port: spec.Port, proto: spec.Protocol}, nil
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
		addr, err := netip.ParseAddr(spec.hostAddr)
		if err != nil {
			return publishSpec{}, fmt.Errorf("invalid publish spec %q: host address must be an IP: %w", s, err)
		}
		spec.hostAddr = addr.Unmap().String()
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
// Preserve the address family when turning an unspecified bind into a
// loopback destination.
func (p publishSpec) connectAddr() string {
	if p.hostAddr == "" {
		return "127.0.0.1"
	}
	if ipIsUnspecified(p.hostAddr) {
		if ipIs4(p.hostAddr) {
			return "127.0.0.1"
		}
		return "::1"
	}
	return canonicalIP(p.hostAddr)
}

func canonicalIP(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	return ip.Unmap().String()
}

func ipIs4(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().Is4()
}

func ipIsLoopback(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().IsLoopback()
}

func ipIsUnspecified(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Unmap().IsUnspecified()
}
