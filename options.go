package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/wait"
)

// Option configures Run.
type Option func(*config) error

// Runner executes one backend CLI invocation.
type Runner = cli.Runner

// ExecRunner executes backend CLI commands as child processes.
type ExecRunner = cli.ExecRunner

// ExternalRunner identifies runners that execute the CLI as real child
// processes for reaper registration.
type ExternalRunner = cli.ExternalRunner

// Streamer starts a long-lived CLI invocation and exposes its combined
// stdout and stderr as a stream.
type Streamer = cli.Streamer

const (
	maxVolumeNameBytes = 255
	maxMountPathBytes  = 4096
	maxReuseGroupBytes = 128
)

type config struct {
	runner               cli.Runner
	binary               string
	eng                  engine
	runTimeout           time.Duration
	queryTimeout         time.Duration
	reuseAttachTimeout   time.Duration
	reusePollInterval    time.Duration
	probeTimeout         time.Duration
	name                 string
	env                  map[string]string
	cmd                  []string
	entrypoint           string
	exposed              []portSpec
	published            []publishSpec
	labels               map[string]string
	mounts               []Mount
	files                []File
	waitStrategy         wait.Strategy
	cpus                 float64
	memory               domain.ByteSize
	shmSize              domain.ByteSize
	privileged           bool
	readOnlyRootFS       bool
	initProcess          bool
	dnsServers           []string
	stopSignal           string
	capAdd               []string
	capDrop              []string
	ulimits              []ulimitSpec
	extraHosts           []extraHostSpec
	hostname             string
	user                 string
	workdir              string
	network              string
	networkExplicit      bool
	networkAliases       []string
	platform             string
	pullPolicy           PullPolicy
	allowMutableImageTag bool
	reuse                bool
	reuseGroup           string
	reuseEnvFingerprint  bool
	creation             string
	// imageCache reuses image-presence answers across Runs when the
	// caller applies a WithImagePresenceCache Option. The Option owns the
	// cache so sequential Runs that reuse it share entries. Nil means no
	// cache, which is the default and preserves one inspect per Run.
	imageCache *imageCache

	// dockerMeta carries the Client's Docker metadata cache into the
	// operation. Package-level Run leaves it nil: caching across
	// process-global calls could serve one target's answer to another
	// after the environment changes.
	dockerMeta *dockerMetadataCache

	// imagePrepared is set when a reuse caller has completed its own
	// PullAlways fetch before entering the shared ensure flight.
	imagePrepared bool
	// reusedCreated is set only on the caller whose flight callback
	// created the container. Other callers still apply their own files
	// after attaching to the shared generation.
	reusedCreated bool
	// postReadyHooks run in option order after the container is ready and
	// before Run returns. A failure rolls back an ordinary create and
	// discards a WithReuse generation created by this call; an attach
	// caller leaves the shared generation in place.
	postReadyHooks []func(context.Context, *Container) error
	// testLogger receives the container's log output while its test runs.
	// It is nil unless WithTestLogger or WithTestLogOnFailure was applied.
	testLogger            testLogSink
	testLogOnFailure      bool
	diagnosticSecrets     []string
	waitDiagnosticSecrets []string
	dockerHost            *string
	dockerContext         *string
	dockerConfigDir       *string
	clientPinned          bool
	attachOutput          io.Writer
}

func newConfig() *config {
	return &config{
		runner: &cli.ExecRunner{},
		env:    map[string]string{},
		labels: map[string]string{},
	}
}

func withClientPinned() Option {
	return func(c *config) error {
		c.clientPinned = true
		return nil
	}
}

// withClientDockerMeta shares the Client's Docker metadata cache with the
// operation, so reuse attaches and handles on one target reuse the same
// lookups. Package-level Run does not apply it.
func withClientDockerMeta(cache *dockerMetadataCache) Option {
	return func(c *config) error {
		c.dockerMeta = cache
		return nil
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
	if c.reuseEnvFingerprint && !c.reuse {
		return validationErrorf("WithReuseEnvFingerprint", nil, "WithReuseEnvFingerprint requires WithReuse")
	}
	if c.reuse && c.attachOutput != nil {
		// The reuse path attaches to a container this call may not have
		// created, so the single creation-time stream WithAttachOutput
		// promises cannot be established.
		return validationErrorf("WithAttachOutput", nil, "WithAttachOutput cannot be combined with WithReuse")
	}
	seenFiles := make(map[string]struct{}, len(c.files))
	for _, f := range c.files {
		clean := path.Clean(f.ContainerPath)
		if _, exists := seenFiles[clean]; exists {
			return newValidationErrorWithField("WithFiles", "containerPath", f.ContainerPath, fmt.Errorf("duplicate container path %q", f.ContainerPath))
		}
		seenFiles[clean] = struct{}{}
	}
	return c.validateDockerTarget()
}

// validateDockerTarget checks the Docker target invariants that depend on the
// resolved engine. validate() runs before engine detection, when c.eng is
// still nil for a plain Run, and Run checks the backend again once the engine
// is known. Values are never echoed: a DOCKER_HOST may carry credentials.
func (c *config) validateDockerTarget() error {
	if c.clientPinned && (c.dockerHost != nil || c.dockerContext != nil || c.dockerConfigDir != nil) {
		return validationErrorf(c.daemonTargetOption(), nil, "cannot override Docker target options on Client.Run")
	}
	if err := c.validateDockerTargetBackend(); err != nil {
		return err
	}
	if c.reuseAttachTimeout > 0 && !c.reuse {
		return validationErrorf("WithReuseAttachTimeout", c.reuseAttachTimeout, "WithReuseAttachTimeout requires WithReuse")
	}
	if c.reusePollInterval > 0 && !c.reuse {
		return validationErrorf("WithReusePollInterval", c.reusePollInterval, "WithReusePollInterval requires WithReuse")
	}
	return nil
}

// validateDockerTargetBackend rejects Docker target overrides on a backend
// that has no Docker target, naming the option the caller set. Run calls it
// again after engine detection because a plain Run resolves its engine after
// validate has already run with a nil engine.
func (c *config) validateDockerTargetBackend() error {
	if c.eng != nil && c.eng.target() == nil &&
		(c.dockerHost != nil || c.dockerContext != nil || c.dockerConfigDir != nil) {
		return validationErrorf(c.daemonTargetOption(), nil, "Docker target options require Docker backend")
	}
	return nil
}

func (c *config) effectiveRunTimeout() time.Duration {
	if c.runTimeout > 0 {
		return c.runTimeout
	}
	return runTimeout
}

func (c *config) effectiveQueryTimeout() time.Duration {
	if c.queryTimeout > 0 {
		return c.queryTimeout
	}
	return queryTimeout
}

func (c *config) effectiveReuseAttachTimeout() time.Duration {
	if c.reuseAttachTimeout > 0 {
		return c.reuseAttachTimeout
	}
	return reuseAttachTimeout
}

func (c *config) effectiveReusePollInterval() time.Duration {
	if c.reusePollInterval > 0 {
		return c.reusePollInterval
	}
	return reusePollInterval
}

func (c *config) effectiveProbeTimeout() time.Duration {
	if c.probeTimeout > 0 {
		return c.probeTimeout
	}
	return 0
}

// daemonTargetOption names the option that set a target override so a
// validation error points at the option the caller must change.
func (c *config) daemonTargetOption() string {
	switch {
	case c.dockerContext != nil:
		return "WithDockerContext"
	case c.dockerConfigDir != nil:
		return "WithDockerConfig"
	default:
		return "WithDockerHost"
	}
}

// allLabels merges the session labels the library always applies with
// user-supplied ones. Internal labels always win so callers cannot
// override the generation used for safe cleanup. validate rejects
// WithReuseGroup without WithReuse, which is also what lets BuildLabels
// decide whether to emit the reuse-group label.
func (c *config) allLabels() map[string]string {
	cfg := domain.LabelConfig{
		Session:    sessionID(),
		Creation:   c.creation,
		Reuse:      c.reuse,
		ReuseGroup: c.reuseGroup,
		Custom:     c.labels,
	}
	if c.reuse {
		// BuildLabels owns every reserved label, so the fingerprint is
		// passed in instead of writing into the returned map.
		cfg.ReuseConfig = c.reuseConfigHash()
		cfg.ReuseEnvFingerprint = c.reuseEnvFingerprintValue()
	}
	return domain.BuildLabels(cfg)
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
		args = append(args, "--publish", p.Raw)
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
		args = append(args, "--cpus", strconv.FormatFloat(c.cpus, 'f', -1, 64))
	}
	if !c.memory.IsZero() {
		args = append(args, "--memory", c.memory.String())
	}
	if !c.shmSize.IsZero() {
		args = append(args, "--shm-size", c.shmSize.String())
	}
	if c.privileged {
		args = append(args, "--privileged")
	}
	if c.readOnlyRootFS {
		args = append(args, "--read-only")
	}
	if c.initProcess {
		args = append(args, "--init")
	}
	for _, server := range c.dnsServers {
		args = append(args, "--dns", server)
	}
	if c.stopSignal != "" {
		args = append(args, "--stop-signal", c.stopSignal)
	}
	for _, capability := range c.capAdd {
		args = append(args, "--cap-add", capability)
	}
	for _, capability := range c.capDrop {
		args = append(args, "--cap-drop", capability)
	}
	for _, limit := range c.ulimits {
		args = append(args, "--ulimit", limit.arg())
	}
	for _, host := range c.extraHosts {
		args = append(args, "--add-host", host.arg())
	}
	if c.hostname != "" {
		args = append(args, "--hostname", c.hostname)
	}
	if c.user != "" {
		args = append(args, "--user", c.user)
	}
	if c.workdir != "" {
		args = append(args, "--workdir", c.workdir)
	}
	if c.networkExplicit && c.network != "" {
		args = append(args, "--network", c.network)
		for _, alias := range c.networkAliases {
			args = append(args, "--network-alias", alias)
		}
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

// WithRunTimeout sets the timeout budget for Run, including container creation
// and any image pull performed during run (whether implicit under PullMissing
// or explicit under PullAlways). A positive d overrides the default 10-minute
// timeout. Non-positive durations are rejected.
func WithRunTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return validationErrorf("WithRunTimeout", d, "timeout must be positive")
		}
		c.runTimeout = d
		return nil
	}
}

// WithQueryTimeout sets the limit for short backend inspection queries and the
// default budget for other operations. Payload transfers, logs, lifecycle changes,
// and verification retries preserve an existing caller deadline. A positive d
// overrides the default 30-second value.
// Non-positive durations are rejected.
func WithQueryTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return validationErrorf("WithQueryTimeout", d, "timeout must be positive")
		}
		c.queryTimeout = d
		return nil
	}
}

// WithProbeTimeout sets the upper bound for backend diagnostic liveness checks
// during error classification. A positive d overrides the default 5-second
// timeout. Non-positive durations are rejected.
func WithProbeTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return validationErrorf("WithProbeTimeout", d, "timeout must be positive")
		}
		c.probeTimeout = d
		return nil
	}
}

// nameRE is the shared safe container-name form; Apple applies an
// additional minimum-length rule in its backend validator.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// networkNameRE is the common syntactic guard. Apple applies its stricter
// lowercase/no-separator rule in appleEngine.checkConfig.
var networkNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// networkAliasRE is Docker's network-scoped DNS label: 1-63 letters, digits,
// or hyphens with an alphanumeric first and last character. A leading dash is
// rejected so an alias can never be parsed as a CLI flag.
var networkAliasRE = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

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

// runnerIsNil reports whether r is unusable as a runner. A typed-nil pointer
// stored in the interface passes an == nil test and then panics when the
// runner is used, so a nil runner has to be rejected by value rather than by
// interface identity.
func runnerIsNil(r Runner) bool {
	if r == nil {
		return true
	}
	value := reflect.ValueOf(r)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// runnerWithBinary returns a runner that runs binary. A caller-owned
// *cli.ExecRunner is copied rather than mutated: the library supports
// concurrent callers sharing one runner, and the binary override belongs to
// the operation that requested it. A custom runner that exposes SetBinary is
// updated in place because the library cannot copy an unknown implementation.
func runnerWithBinary(r Runner, binary string) Runner {
	if r == nil || binary == "" {
		return r
	}
	if _, ok := r.(*cli.ExecRunner); ok {
		cloned := cloneRunner(r).(*cli.ExecRunner)
		cloned.Binary = binary
		return cloned
	}
	if bs, ok := r.(interface{ SetBinary(string) }); ok {
		bs.SetBinary(binary)
	}
	return r
}

// WithRunner sets the runner used for CLI invocations. Only a
// *ExecRunner is copied when a binary override applies; another Runner
// that implements SetBinary, including a type embedding ExecRunner, is
// rewritten in place and must not be shared by concurrent operations.
func WithRunner(r Runner) Option {
	return func(c *config) error {
		if runnerIsNil(r) {
			return validationErrorf("WithRunner", nil, "runner must not be nil")
		}
		c.runner = r
		if c.binary != "" {
			c.runner = runnerWithBinary(c.runner, c.binary)
		}
		return nil
	}
}

// WithBinary sets the executable name or path for the container CLI.
// It overrides the backend's default CLI binary name. Only a *ExecRunner
// is copied when the override applies; a custom Runner that implements
// SetBinary, including a type embedding ExecRunner, is rewritten in place and
// must not be shared by concurrent operations.
func WithBinary(path string) Option {
	return func(c *config) error {
		if path == "" {
			return validationErrorf("WithBinary", path, "binary path must not be empty")
		}
		if strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
			return validationErrorf("WithBinary", path, "invalid binary path %q", path)
		}
		c.binary = path
		c.runner = runnerWithBinary(c.runner, path)
		return nil
	}
}

func withRunner(r cli.Runner) Option {
	return WithRunner(r)
}

func withEngine(e engine) Option {
	return func(c *config) error {
		c.eng = e
		return nil
	}
}

// nilWriter reports whether w is nil, including a typed-nil value stored in
// the interface. A plain interface comparison misses that case, which would
// otherwise panic in the background attach stream on its first write.
func nilWriter(w io.Writer) bool {
	if w == nil {
		return true
	}
	return nilInterface(reflect.ValueOf(w))
}

// nilReader reports whether r is nil, including a typed-nil value stored in
// the interface. A typed-nil reader passes an interface comparison and panics
// on its first Read.
func nilReader(r io.Reader) bool {
	if r == nil {
		return true
	}
	return nilInterface(reflect.ValueOf(r))
}

// nilInterface reports whether a nil-capable dynamic value is nil.
func nilInterface(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// WithDockerHost overrides the target Docker daemon host (DOCKER_HOST)
// for this container.
func WithDockerHost(host string) Option {
	return func(c *config) error {
		if err := validateDockerTargetValue("WithDockerHost", host); err != nil {
			return err
		}
		c.dockerHost = &host
		return nil
	}
}

// WithDockerContext overrides the target Docker context (DOCKER_CONTEXT)
// for this container. A configured context takes precedence over an ambient
// DOCKER_HOST and over an explicit WithDockerHost when both are set; an
// explicit host still wins over an ambient context, matching the library's
// Docker target resolution.
func WithDockerContext(context string) Option {
	return func(c *config) error {
		if err := validateDockerContextValue("WithDockerContext", context); err != nil {
			return err
		}
		c.dockerContext = &context
		return nil
	}
}

// WithDockerConfig overrides the target Docker config directory (DOCKER_CONFIG)
// for this container.
func WithDockerConfig(dir string) Option {
	return func(c *config) error {
		if err := validateDockerTargetValue("WithDockerConfig", dir); err != nil {
			return err
		}
		c.dockerConfigDir = &dir
		return nil
	}
}

// validateDockerTargetValue rejects Docker target values that cannot be passed
// to a child process safely or that are too large to redact from streamed
// diagnostics. The rejected value is never echoed. An empty value is accepted
// on purpose: it pins the target as explicitly unset, so a DOCKER_HOST that
// appears later cannot repoint the child.
func validateDockerTargetValue(option, value string) error {
	if err := validateEnvValue(value); err != nil {
		return newValidationErrorWithField(option, "value", nil, fmt.Errorf("has an invalid value: %w", err))
	}
	if len(value) > cli.MaxStreamOverlap || !cli.StreamValueFits(value) {
		return invalidOption(option, "value is too large to redact safely from streamed diagnostics")
	}
	return nil
}

// validateDockerContextValue additionally enforces Docker's context-name
// grammar so an unvalidated name cannot be read as a `docker context inspect`
// flag. An empty value is accepted on purpose: it pins the context as
// explicitly unset.
func validateDockerContextValue(option, value string) error {
	if err := validateDockerTargetValue(option, value); err != nil {
		return err
	}
	if value != "" && !validDockerContextName(value) {
		return newValidationErrorWithField(option, "value", nil, errors.New("invalid Docker context name"))
	}
	return nil
}
