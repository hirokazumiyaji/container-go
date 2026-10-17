package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

// appleEngine drives Apple Container's `container` CLI.
type appleEngine struct{}

// Apple Container parses stop --time as a signed 32-bit integer, even when
// the library itself runs on a 64-bit host.
const maxAppleStopSeconds int64 = math.MaxInt32

// Verified against Apple Container CLI 1.2.x–1.3.x (local: 1.3.0).
// Matchers are command-, binary-, code-, and target-aware. Apple's
// ContainerizationError envelope is either a direct code/message pair
// (run conflict, inspect, image inspect, exec) or an internalError wrapper
// whose cause is notFound (create race, stop, delete, logs). Legacy flat
// spellings remain accepted only for the same operation-specific forms.
// Generic application output containing "not found" is not a backend match;
// an application-prefixed typed message is rejected as well.
const (
	appleStderrNameConflict  = "container with id"
	appleStderrAlreadyExists = "already exists"
	appleStderrImageNotFound = "image not found:"
	// appleLogsGetPrefix and appleLogsOpenPrefix are the two exact markers
	// the 1.2/1.3 logs client wraps an absent container in. They appear
	// both in the flat spelling and as the root and cause messages of the
	// nested internalError envelope.
	appleLogsGetPrefix  = "failed to get logs for container "
	appleLogsOpenPrefix = "failed to open container logs: "
)

// These limits mirror the checks in Apple Container 1.2.2 and 1.3.0
// (Parser.resources, PublishPort, Utility, and ContainersService). Keeping
// them here makes a bad request fail before any image fetch or container
// create is attempted.
const (
	appleMinMemoryBytes uint64 = 200 * 1024 * 1024
	// Apple parses the input as a Double, converts to MiB, narrows to Int64,
	// then multiplies by 1 MiB into UInt64. MaxInt64 bytes is a conservative
	// bound that leaves room for floating-point rounding in that path.
	appleMaxMemoryBytes     uint64 = 1<<63 - 1
	applePublishedPortLimit        = 64
)

var (
	// Apple Container requires at least two characters for a container
	// name; a one-character name is valid for the other backend.
	appleContainerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`)
	// NetworkResource.nameValid is deliberately different from the
	// container-name rule: it is lowercase, allows one character, and
	// disallows a trailing separator.
	appleNetworkNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)
)

func (appleEngine) name() string   { return "apple" }
func (appleEngine) binary() string { return "container" }
func (appleEngine) directIP() bool { return true }

// checkConfig applies the parts of Apple Container's CLI contract that are
// independent of the current service state. The Apple CLI performs these
// checks only after it has already resolved the image, so doing them here
// avoids surprising pulls and makes the backend limits visible to callers.
func (appleEngine) checkConfig(_ context.Context, cfg *config) error {
	if err := resolveEffectivePlatform(cfg); err != nil {
		return err
	}
	if cfg.pullPolicy == PullNever {
		return fmt.Errorf("%w: Apple Container's run command always resolves images; use PullMissing or PullAlways", ErrPullNeverUnsupported)
	}
	if cfg.platform != "" {
		if err := validateApplePlatform(cfg.platform); err != nil {
			return err
		}
		cfg.platform = canonicalizeApplePlatformSelector(cfg.platform)
	}

	if cfg.name != "" && !appleContainerNameRE.MatchString(cfg.name) {
		return fmt.Errorf("invalid Apple container name %q: must be 2-63 characters and match %s", cfg.name, appleContainerNameRE)
	}
	if cfg.network != "" {
		// Apple accepts comma-separated network properties, but this
		// public option intentionally represents a name only. Rejecting
		// the separator here avoids passing an option string as a name.
		if strings.Contains(cfg.network, ",") {
			return fmt.Errorf("invalid Apple network name %q: comma-separated network properties are not supported", cfg.network)
		}
		if !appleNetworkNameRE.MatchString(cfg.network) {
			return fmt.Errorf("invalid Apple network name %q: must match %s", cfg.network, appleNetworkNameRE)
		}
	}

	if len(cfg.published) > applePublishedPortLimit {
		return fmt.Errorf("apple backend supports at most %d published-port descriptors, got %d", applePublishedPortLimit, len(cfg.published))
	}
	seenPublished := make(map[string]struct{}, len(cfg.published))
	for _, p := range cfg.published {
		if p.hostPort <= 1 || p.containerPort <= 1 {
			return fmt.Errorf("apple backend requires published ports to be 2-65535, got %q", p.raw)
		}
		// Match Apple Container's PublishPort.hasOverlaps key:
		// "\(hostPort)/\(proto)" without hostAddress. Distinct bind
		// addresses on the same host port and protocol still overlap
		// upstream, so include them here before the CLI is called.
		key := fmt.Sprintf("%d/%s", p.hostPort, p.proto)
		if _, exists := seenPublished[key]; exists {
			return fmt.Errorf("apple backend published-port descriptors overlap at %q", p.raw)
		}
		seenPublished[key] = struct{}{}
	}

	if cfg.memory != "" {
		if _, err := appleMemoryBytes(cfg.memory); err != nil {
			return err
		}
	}
	return nil
}

// appleMemoryBytes parses the integer/unit form accepted by WithMemory and
// applies the limits used by Apple Container's run/create path. Apple parses
// the numeric value as a Swift Double, converts it to MiB, narrows it to
// Int64, and multiplies by 1 MiB into UInt64. The API server requires at least
// 200 MiB. Use MaxInt64 bytes as a conservative upper bound so values that
// could round across the final UInt64 conversion are rejected before reaching
// the CLI.
func appleMemoryBytes(size string) (uint64, error) {
	if !memoryRE.MatchString(size) {
		return 0, fmt.Errorf("invalid Apple memory size %q: use an integer with an optional K, M, G, T, or P suffix", size)
	}

	digits := size
	multiplier := uint64(1)
	// memoryRE has already restricted the optional suffix to K/M/G/T/P.
	if last := size[len(size)-1]; last < '0' || last > '9' {
		digits = size[:len(size)-1]
		switch last {
		case 'K':
			multiplier = 1 << 10
		case 'M':
			multiplier = 1 << 20
		case 'G':
			multiplier = 1 << 30
		case 'T':
			multiplier = 1 << 40
		case 'P':
			multiplier = 1 << 50
		}
	}

	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("apple memory size %q overflows the supported 64-bit range", size)
		}
		return 0, fmt.Errorf("invalid Apple memory size %q: %w", size, err)
	}
	if n > appleMaxMemoryBytes/multiplier {
		return 0, fmt.Errorf("apple memory size %q overflows the supported 64-bit range", size)
	}
	bytes := n * multiplier
	if bytes < appleMinMemoryBytes {
		return 0, fmt.Errorf("apple backend requires at least 200 MiB of memory, got %q", size)
	}
	return bytes, nil
}

// canonicalizeApplePlatformSelector makes the default variant explicit
// where Apple requires a stable selector for image operations. In
// particular, linux/armel is v6 rather than the generic ARM default.
func canonicalizeApplePlatformSelector(platform string) string {
	if platform == "linux/armel" {
		return platform + "/v6"
	}
	return platform
}

// materializeAppleRunPlatform records the platform Apple will use for a
// Run with no explicit selector. Pull alone intentionally remains
// platform-agnostic, so it must not call this helper.
func materializeAppleRunPlatform(cfg *config) {
	if cfg == nil || cfg.eng == nil || cfg.eng.name() != "apple" || cfg.platform != "" {
		return
	}
	cfg.platform = appleHostPlatform()
}

// appleHostPlatform is the Linux platform selected by Apple's default
// linux/<host-architecture> run settings. The variant is left implicit for
// arm64, matching Apple's CLI while the platform matcher treats it as v8.
func appleHostPlatform() string {
	architecture := runtime.GOARCH
	switch architecture {
	case "aarch64":
		architecture = "arm64"
	case "x86_64":
		architecture = "amd64"
	}
	return "linux/" + architecture
}

// appleImageIdentityMetadataPresent reports whether an Apple identity has
// usable platform/variant fields, without deciding whether they match the
// requested platform.
func appleImageIdentityMetadataPresent(identity imageIdentity, platform string) bool {
	if platform == "" {
		return true
	}
	if identity.platform == "" || !validImageDigest(identity.variantDigest) {
		return false
	}
	_, ok := parseApplePlatformSelector(identity.platform)
	return ok
}

// appleImageIdentityComplete reports whether an Apple identity carries
// all metadata needed to verify the create and the selected variant. A
// provisional digest-only identity must not be returned to Run.
func appleImageIdentityComplete(identity imageIdentity, platform string) bool {
	if !identity.pinned || identity.repository == "" || !imageIdentityIsVerified(identity) {
		return false
	}
	if !appleImageIdentityMetadataPresent(identity, platform) {
		return false
	}
	return platform == "" || applePlatformMetadataCompatible(platform, identity.platform)
}

// validateApplePlatform mirrors the platform grammar accepted by Apple
// Container. Docker keeps the generic platformRE grammar.
func validateApplePlatform(platform string) error {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid platform %q for Apple backend: expected os/arch[/variant]", platform)
	}
	if len(parts) > 3 || (len(parts) == 3 && parts[2] == "") {
		return fmt.Errorf("invalid platform %q for Apple backend: expected os/arch[/variant]", platform)
	}
	if parts[0] != "linux" {
		return fmt.Errorf("apple backend supports only linux platforms, got %q", platform)
	}
	if len(parts) == 2 {
		return nil
	}

	arch, variant := parts[1], parts[2]
	valid := false
	switch arch {
	case "arm":
		valid = variant == "v5" || variant == "v6" || variant == "v7" || variant == "v8"
	case "armhf":
		valid = variant == "v7"
	case "armel":
		valid = variant == "v6"
	case "aarch64", "arm64":
		valid = variant == "v8" || variant == "8"
	case "x86_64", "x86-64", "amd64":
		valid = variant == "v1"
	}
	if !valid {
		return fmt.Errorf("invalid platform %q for Apple backend: variant %q is not valid for architecture %q", platform, variant, arch)
	}
	return nil
}

// applePlatform is the normalized selector used when matching an Apple
// image variant. Apple treats architecture aliases and the default arm64
// variant as equivalent.
type applePlatform struct {
	os           string
	architecture string
	variant      string
}

// parseApplePlatformSelector parses a platform selector using the same
// defaults as ContainerizationOCI.Platform(from:): bare arm/armhf selectors
// mean v7, bare armel means v6 (matching explicit linux/armel/v6), bare
// aarch64/arm64 means v8, and x86 aliases default to no variant.
func parseApplePlatformSelector(platform string) (applePlatform, bool) {
	parts := strings.Split(platform, "/")
	if len(parts) < 1 || len(parts) > 3 || parts[0] == "" {
		return applePlatform{}, false
	}
	if len(parts) > 1 && parts[1] == "" {
		return applePlatform{}, false
	}
	if len(parts) == 3 && parts[2] == "" {
		return applePlatform{}, false
	}
	arch := ""
	if len(parts) > 1 {
		arch = parts[1]
	}
	variant := ""
	if len(parts) == 3 {
		variant = parts[2]
	} else if arch != "" {
		switch arch {
		case "arm", "armhf":
			variant = "v7"
		case "armel":
			variant = "v6"
		case "aarch64", "arm64":
			variant = "v8"
		}
	}
	return canonicalApplePlatform(parts[0], arch, variant), true
}

func canonicalApplePlatform(osName, architecture, variant string) applePlatform {
	p := applePlatform{os: osName, architecture: architecture, variant: variant}
	switch architecture {
	case "aarch64", "arm64":
		p.architecture = "arm64"
		if variant == "" || variant == "v8" || variant == "8" {
			p.variant = "v8"
		}
	case "x86_64", "x86-64", "amd64":
		p.architecture = "amd64"
		if variant == "v1" {
			p.variant = ""
		}
	case "armhf", "armel":
		p.architecture = "arm"
	}
	return p
}

func applePlatformsEqual(want, have applePlatform) bool {
	if want.os != have.os {
		return false
	}
	if want.architecture != "" && want.architecture != have.architecture {
		return false
	}
	if want.variant != "" && want.variant != have.variant {
		return false
	}
	return true
}

func formatApplePlatform(platform applePlatform) string {
	if platform.os == "" || platform.architecture == "" {
		return ""
	}
	value := platform.os + "/" + platform.architecture
	if platform.variant != "" {
		value += "/" + platform.variant
	}
	return value
}

// imageStoreID is empty: Apple Container talks to one local store.
func (appleEngine) imageStoreID() string { return "" }
func (appleEngine) targetID() string     { return "" }
func (appleEngine) isRemote() bool       { return false }

func (appleEngine) defaultHost() string { return "127.0.0.1" }

func (appleEngine) probe() cli.Probe {
	return cli.Probe{
		Args:          []string{"system", "status"},
		Hint:          "Ensure container system service has been started with `container system start`.",
		Binary:        "container",
		Operation:     "system",
		IsUnavailable: appleProbeUnavailable,
	}
}

func appleProbeUnavailable(err error) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "container")
	var systemBranches []cliErrorBranch
	for _, branch := range branches {
		if branch.ctx.operation == "system" {
			systemBranches = append(systemBranches, branch)
		}
	}
	if len(systemBranches) == 0 {
		return false
	}
	for _, branch := range systemBranches {
		if cli.IsProbeConfigurationError(branch.cause) {
			return false
		}
	}
	var nonCLI []error
	collectNonCLIErrorBranches(err, &nonCLI)
	for _, extra := range nonCLI {
		if cli.IsProbeConfigurationError(extra) {
			return false
		}
	}
	for _, branch := range systemBranches {
		stderr, stdout := branchLines(branch, true)
		for _, line := range stderr {
			if appleProbeLivenessText(line) {
				return true
			}
		}
		for _, line := range stdout {
			if appleProbeLivenessText(line) {
				return true
			}
		}
	}
	for _, extra := range nonCLI {
		if appleProbeLivenessText(strings.ToLower(extra.Error())) {
			return true
		}
	}
	return false
}

func appleProbeLivenessText(line string) bool {
	for _, fragment := range []string{
		"xpc connection",
		"container-apiserver",
		"plugins are unavailable",
		"start the container system services",
		"system is not running",
		"system service is not running",
		"apiserver is not running",
		"not registered with launchd",
		"connection refused",
	} {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	return append(args, cfg.commonRunArgs(image, envFile, nil)...)
}

func (appleEngine) parseRunID([]byte) string { return "" }

func (appleEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

func (appleEngine) parseInspect(data []byte, id string) (*engineInfo, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, newInspectTargetNotFound(id, "empty inspect output")
	}
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if strings.TrimSpace(c.ID) == "" && strings.TrimSpace(c.Status.State) != "" {
			return nil, fmt.Errorf("decode container inspect output: container id is required")
		}
	}
	var matchedMissingStatus bool
	for _, c := range containers {
		if !appleInspectTargetMatches(c, id) {
			continue
		}
		if strings.TrimSpace(c.Status.State) == "" {
			matchedMissingStatus = true
			continue
		}
		image := c.Configuration.Image.Reference
		imageDigest := c.Configuration.Image.Descriptor.Digest
		if image != "" && validImageDigest(imageDigest) {
			image = stripImageDigest(image) + "@" + imageDigest
		}
		platform := ""
		if p := c.Configuration.Platform; p.OS != "" || p.Architecture != "" || p.Variant != "" {
			candidate := p.OS + "/" + p.Architecture
			if p.Variant != "" {
				candidate += "/" + p.Variant
			}
			parsed, ok := parseApplePlatformSelector(candidate)
			if !ok {
				return nil, fmt.Errorf("container %s has invalid platform %q", id, candidate)
			}
			platform = formatApplePlatform(parsed)
		}
		info := &engineInfo{
			state:              appleState(c.Status.State),
			name:               c.ID,
			labels:             c.Configuration.Labels,
			image:              image,
			imageDigest:        imageDigest,
			imageVariantDigest: c.Configuration.Image.VariantDigest,
		}
		pConfig := c.Configuration.Platform
		platformMeta := platformMetadataFromParts(
			pConfig.OS,
			pConfig.Architecture,
			pConfig.Variant,
			pConfig.OSPresent || pConfig.OS != "",
			pConfig.ArchPresent || pConfig.Architecture != "",
			pConfig.VariantPresent || pConfig.Variant != "",
		)
		if platform != "" {
			info.platform = platform
		} else {
			info.platform = platformMeta.normalized()
		}
		info.platformMeta = platformMeta
		if ip, err := c.IPv4(); err == nil {
			info.ip = ip
		}
		info.createdAt = parseInspectTime(c.Configuration.CreationDate)
		if c.Status.StartedDate != nil {
			info.startedAt = parseInspectTime(*c.Status.StartedDate)
		}
		if c.Status.ExitedAt != nil {
			info.finishedAt = parseInspectTime(*c.Status.ExitedAt)
		}
		if c.Status.ExitCode != nil {
			info.exitCode = *c.Status.ExitCode
		}
		for _, p := range c.Configuration.PublishedPorts {
			info.bound = append(info.bound, boundPort{
				containerPort: p.ContainerPort,
				proto:         p.Proto,
				hostAddr:      canonicalIP(p.HostAddress),
				hostPort:      p.HostPort,
			})
		}
		return info, nil
	}
	if matchedMissingStatus {
		return nil, fmt.Errorf("decode container inspect output: container status.state is required")
	}
	return nil, newInspectTargetNotFound(id, "inspect output did not contain the requested target")
}

func appleInspectTargetMatches(c inspect.Container, target string) bool {
	target = strings.TrimPrefix(strings.TrimSpace(target), "/")
	if target == "" {
		return false
	}
	return strings.TrimPrefix(strings.TrimSpace(c.ID), "/") == target
}

func appleState(state string) State {
	switch state {
	case string(StateRunning):
		return StateRunning
	case string(StateStopped):
		return StateStopped
	case string(StateStopping):
		return StateStopping
	case string(StateCreated):
		return StateCreated
	default:
		return StateUnknown
	}
}

func (appleEngine) startArgs(id string) []string {
	return []string{"start", id}
}

func (appleEngine) restartArgs(id string, timeout *time.Duration) ([]string, error) {
	return nil, fmt.Errorf("%w: apple backend does not support restart CLI subcommand", ErrUnsupportedCapability)
}

func (appleEngine) killArgs(id string, signal string) []string {
	if signal != "" {
		return []string{"kill", "--signal", signal, id}
	}
	return []string{"kill", id}
}

func (appleEngine) pauseArgs(id string) ([]string, error) {
	return nil, fmt.Errorf("%w: apple backend does not support pause", ErrUnsupportedCapability)
}

func (appleEngine) unpauseArgs(id string) ([]string, error) {
	return nil, fmt.Errorf("%w: apple backend does not support unpause", ErrUnsupportedCapability)
}

func (appleEngine) stopArgs(id string, timeout *time.Duration) ([]string, error) {
	return stopArgsFor(id, timeout, maxAppleStopSeconds)
}

func (appleEngine) deleteArgs(id string) []string {
	return []string{"delete", "--force", id}
}

func (appleEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return []string{"cp", hostPath, id + ":" + containerPath}
}

func (appleEngine) copyFromArgs(id, containerPath, hostPath string) []string {
	return []string{"cp", id + ":" + containerPath, hostPath}
}

// Apple Container's public cp command has no mode that preserves source
// types or prevents it from dereferencing special files, so fail closed.
func (appleEngine) checkCopyFileFromContainer() error {
	return fmt.Errorf(
		"%w: Apple Container cp has no type-preserving/no-follow copy-out mode",
		ErrCopyFileFromContainerUnsupported,
	)
}

func (appleEngine) checkCopyFileFromContainerVersion(context.Context, cli.Runner) error {
	return appleEngine{}.checkCopyFileFromContainer()
}

func (appleEngine) reaperSubcommand() string { return "delete" }

func (appleEngine) reaperDeleteFlags() []string { return nil }

func (appleEngine) execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string {
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

func (appleEngine) logsFollowArgs(id string) []string {
	return []string{"logs", "--follow", id}
}

func (appleEngine) logsArgsWithOptions(id string, opts LogsOptions) ([]string, error) {
	if !opts.Since.IsZero() {
		return nil, fmt.Errorf("%w: apple backend does not support logs since", ErrUnsupportedCapability)
	}
	args := []string{"logs"}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	}
	return append(args, id), nil
}

func (appleEngine) logsTailArgs(id string) []string {
	return []string{"logs", "-n", "1000", id}
}

func (appleEngine) listArgs() []string {
	return []string{"ls", "--all", "--format", "json"}
}

// parseStoppedManaged filters client-side: the Apple CLI exposes no
// label or status filter.
func (appleEngine) parseStoppedManaged(data []byte) ([]string, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		if c.Configuration.Labels[managedLabel] == "true" && c.Status.State == string(StateStopped) {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

type appleImageDescriptor struct {
	Digest      string            `json:"digest"`
	MediaType   string            `json:"mediaType"`
	Annotations map[string]string `json:"annotations"`
}

type appleImageVariant struct {
	Digest   string `json:"digest"`
	Platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type appleImageInspectRecord struct {
	ID            string               `json:"id"`
	Reference     string               `json:"reference"`
	Name          string               `json:"name"`
	Descriptor    appleImageDescriptor `json:"descriptor"`
	Configuration struct {
		Name       string               `json:"name"`
		Reference  string               `json:"reference"`
		Descriptor appleImageDescriptor `json:"descriptor"`
		Platform   struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
		Image struct {
			Reference  string               `json:"reference"`
			Descriptor appleImageDescriptor `json:"descriptor"`
		} `json:"image"`
	} `json:"configuration"`
	Variants []appleImageVariant `json:"variants"`
}

func (appleEngine) imageInspectArgs(image, _ string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "pull", "--platform", platform, image}
	}
	return []string{"image", "pull", image}
}

// imageMissing matches backend-specific image-absence reasons. Apple
// reports local inspect misses as `image not found: <name>` and registry
// pull misses as a MANIFEST_UNKNOWN code or an explicit HTTP 404 response.
func (appleEngine) imageMissing(err error) bool {
	return (appleEngine{}).imageMissingForTarget(err, "")
}

func (appleEngine) imageMissingForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "container")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if !exactImageTarget(branch, target) {
			continue
		}
		if appleImageOperationalFailure(branch.stderr) {
			continue
		}
		if branch.ctx.operation == "image inspect" {
			if hasBranchImageLine(branch, appleStderrImageNotFound, branch.ctx.target, true) {
				return true
			}
			if hasAppleTypedImageLine(branch, branch.ctx.target) {
				return true
			}
		}
		if branch.ctx.operation == "image pull" {
			if appleImagePullManifestMissing(branch.ctx.err) {
				return true
			}
			if hasBranchImageLine(branch, appleStderrImageNotFound, branch.ctx.target, true) ||
				hasBranchImageLine(branch, appleStderrImageNotFound, "", false) {
				return true
			}
		}
	}
	return false
}

func appleImagePullManifestMissing(cliErr *cli.CLIError) bool {
	if cliErr == nil || len(cliErr.Args) < 3 ||
		!strings.EqualFold(cliErr.Args[0], "image") ||
		!strings.EqualFold(cliErr.Args[1], "pull") {
		return false
	}
	target := strings.TrimSpace(cliErr.Args[len(cliErr.Args)-1])
	if target == "" || strings.HasPrefix(target, "--") {
		return false
	}
	stderr := strings.ToLower(cliErr.Stderr)
	if appleManifestUnknownCode(stderr) {
		return true
	}
	for _, marker := range []string{
		"response: 404", "response 404", "response status 404", "status code: 404", "status: 404", "status 404", "http status 404", "status=404",
		"http 404", "http/1.1 404", "http/2 404", "404 not found",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func appleManifestUnknownCode(stderr string) bool {
	stderr = strings.ToLower(stderr)
	const code = "manifest_unknown"
	for start := 0; ; {
		index := strings.Index(stderr[start:], code)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(code)
		beforeOK := index == 0 || !isASCIIWordByte(stderr[index-1])
		afterOK := end == len(stderr) || !isASCIIWordByte(stderr[end])
		if beforeOK && afterOK {
			return true
		}
		start = end
	}
}

func isASCIIWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func appleImageOperationalFailure(stderr string) bool {
	stderr = strings.ToLower(stderr)
	for _, marker := range []string{
		"permission denied", "access denied", "unauthorized", "forbidden", "authentication required",
		"connection refused", "connection reset", "timed out", "no such host", "network is unreachable",
		"context deadline exceeded", "deadline exceeded", "i/o timeout", "network error", "xpc connection error",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func (appleEngine) imageIdentityNeedsLocalCheck() bool { return true }

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	identity, exists := (appleEngine{}).parseImageIdentity(data, "", platform)
	return exists && !identity.notLocal
}

// parseImageIdentity reads the root index descriptor exposed by current
// Apple Container releases and also accepts the older flat
// ImageDescription shape. When a platform is selected, the variant is
// validated separately while the root descriptor remains the run and
// reuse identity.
func (appleEngine) parseImageIdentity(data []byte, image, platform string) (imageIdentity, bool) {
	var records []appleImageInspectRecord
	if err := json.Unmarshal(data, &records); err != nil {
		// A successful CLI invocation with malformed JSON is not proof
		// that the image is absent. Treat it as an identity-unavailable
		// result so PullMissing cannot turn a parser failure into a fetch.
		return imageIdentity{}, true
	}
	if len(records) == 0 {
		return imageIdentity{}, false
	}
	record := records[0]
	if platform == "" && (record.Configuration.Platform.OS != "" || record.Configuration.Platform.Architecture != "" || record.Configuration.Platform.Variant != "") {
		candidate := record.Configuration.Platform.OS + "/" + record.Configuration.Platform.Architecture
		if record.Configuration.Platform.Variant != "" {
			candidate += "/" + record.Configuration.Platform.Variant
		}
		parsed, ok := parseApplePlatformSelector(candidate)
		if !ok {
			return imageIdentity{}, true
		}
		platform = formatApplePlatform(parsed)
	}
	rootDigest, rootOK := appleRootDescriptorDigest(record)
	variantDigest := ""
	if platform != "" {
		if len(record.Variants) == 0 && !rootOK {
			if len(record.ID) == 64 && domain.IsHex(record.ID) {
				// An old ID-only record proves that some local content
				// exists, but it cannot prove that the host variant is
				// addressable by a digest reference.
				return imageIdentity{
					notLocal:       true,
					notLocalReason: "Apple image inspect has no platform variant metadata",
					platform:       platform,
				}, true
			}
			// Without a usable root descriptor or a local ID, the record
			// is identity-unavailable rather than evidence that a requested
			// platform is absent from the local store.
			return imageIdentity{}, true
		}
		var variantOK bool
		variantDigest, variantOK = applePlatformVariantDigest(record, platform)
		if !variantOK {
			return imageIdentity{
				notLocal:       true,
				notLocalReason: "Apple image inspect has no complete matching platform variant",
				platform:       platform,
			}, true
		}
		if !rootOK {
			// A selected variant is not a substitute for the root
			// index identity used by run and reuse.
			return imageIdentity{}, true
		}
	}
	if !rootOK && appleDescriptorPresent(record) {
		// A malformed descriptor is not equivalent to an older
		// descriptor-less response. Do not fall back to an ID when the
		// backend supplied an unusable descriptor field.
		return imageIdentity{}, true
	}
	return finishAppleImageIdentity(image, record, rootDigest, platform, variantDigest), true
}

// appleRootDescriptorDigest returns the index/root descriptor digest,
// rejecting conflicting or malformed descriptor fields.
func appleRootDescriptorDigest(record appleImageInspectRecord) (string, bool) {
	var digest string
	for _, candidate := range []string{
		record.Configuration.Descriptor.Digest,
		record.Configuration.Image.Descriptor.Digest,
		record.Descriptor.Digest,
	} {
		if candidate == "" {
			continue
		}
		if !validImageDigest(candidate) {
			return "", false
		}
		if digest != "" && !strings.EqualFold(digest, candidate) {
			return "", false
		}
		digest = candidate
	}
	return digest, digest != ""
}

func appleDescriptorPresent(record appleImageInspectRecord) bool {
	return record.Configuration.Descriptor.Digest != "" ||
		record.Configuration.Image.Descriptor.Digest != "" ||
		record.Descriptor.Digest != ""
}

// applePlatformVariantDigest validates every listed variant before
// selecting one. An empty or incomplete list is not local evidence for
// an explicit platform request.
func applePlatformVariantDigest(record appleImageInspectRecord, platform string) (string, bool) {
	if len(record.Variants) == 0 {
		return "", false
	}
	want, ok := parseApplePlatformSelector(platform)
	if !ok {
		return "", false
	}
	selected := ""
	for _, variant := range record.Variants {
		if variant.Platform.OS == "" || variant.Platform.Architecture == "" || !validImageDigest(variant.Digest) {
			return "", false
		}
		have := canonicalApplePlatform(variant.Platform.OS, variant.Platform.Architecture, variant.Platform.Variant)
		if applePlatformsEqual(want, have) {
			if selected != "" && !strings.EqualFold(selected, variant.Digest) {
				return "", false
			}
			selected = variant.Digest
		}
	}
	return selected, selected != ""
}

func finishAppleImageIdentity(requested string, record appleImageInspectRecord, digest, platform, variantDigest string) imageIdentity {
	reference := record.Configuration.Name
	if reference == "" {
		reference = record.Configuration.Reference
	}
	if reference == "" {
		reference = record.Configuration.Image.Reference
	}
	if reference == "" {
		reference = record.Reference
	}
	if reference == "" {
		reference = record.Name
	}

	// A bare sha256:... is Docker's image-ID syntax, not an Apple
	// repository reference. It is safe to handle only when the inspected
	// Apple record proves the same identity through its descriptor and
	// gives us a repository base for the normalized run reference.
	if isImageID(requested) || isBareImageID(requested) {
		if imageReferenceBase(reference) == "" || !validImageDigest(digest) {
			return imageIdentity{}
		}
		if !appleImageIDMatchesDescriptor(requested, record.ID, digest) {
			return imageIdentity{mismatch: true}
		}
	} else if requested != "" && reference != "" && !appleImageReferencesCompatible(requested, reference) {
		return imageIdentity{mismatch: true}
	}

	if validImageDigest(digest) {
		// The descriptor is authoritative. Do not copy an ID-shaped field
		// into imageIdentity.id: that field is a Docker-only local-ID ABI.
		runReference := appleRunReferenceBase(requested, reference)
		identity := imageReferenceWithDigest(runReference, reference, digest, "")
		identity.rootDigest = digest
		identity.platform = platform
		identity.variantDigest = variantDigest
		identity.repository = imageRepository(runReference)
		identity.appleSynthetic = appleSyntheticIndex(record, digest, variantDigest)
		// A caller may pin the actual manifest of a single-manifest image.
		// That digest is registry-addressable even though Apple's synthetic
		// root index digest is not, so retain the caller's tag plus digest.
		if requestedDigest := imageDigest(requested); validImageDigest(requestedDigest) && validImageDigest(variantDigest) && strings.EqualFold(requestedDigest, variantDigest) {
			if base := imageReferenceBase(requested); base != "" {
				identity.reference = base + "@" + variantDigest
				identity.appleSynthetic = false
			}
		}
		return identity
	}
	if isImageID(record.ID) {
		// Apple must not treat a Docker-style ID field as an immutable
		// run target without a descriptor proving what that ID names.
		return imageIdentity{}
	}
	if len(record.ID) == 64 && domain.IsHex(record.ID) {
		// Older Apple image-inspect responses exposed the local content ID
		// without a descriptor. Keep the compatibility fallback, but do
		// not classify it as a Docker image ID.
		digest := "sha256:" + record.ID
		identity := imageReferenceWithDigest(appleRunReferenceBase(requested, reference), reference, digest, "")
		identity.rootDigest = digest
		identity.platform = platform
		identity.variantDigest = variantDigest
		identity.repository = imageRepository(identity.reference)
		return identity
	}
	return imageIdentity{}
}

// appleSyntheticIndex identifies Apple's explicitly annotated local index
// wrapper. A one-entry OCI index is not, by itself, evidence of a
// synthetic root: platform-scoped inspect responses can legitimately expose
// only one manifest from a multi-platform index. Callers use the exact-root
// probe when this reliable signal is absent.
func appleSyntheticIndex(record appleImageInspectRecord, _, _ string) bool {
	for _, descriptor := range []appleImageDescriptor{
		record.Configuration.Descriptor,
		record.Configuration.Image.Descriptor,
		record.Descriptor,
	} {
		value, ok := descriptor.Annotations["com.apple.containerization.index.indirect"]
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "true") || value == "1" {
			return true
		}
	}
	return false
}

// appleRunReferenceBase preserves a backend's canonical custom-registry
// name when the caller used an unqualified reference. Docker Hub's familiar
// short form remains unchanged for compatibility.
func appleRunReferenceBase(requested, reported string) string {
	requestedBase := imageReferenceBase(requested)
	reportedBase := imageReferenceBase(reported)
	if requestedBase == "" {
		return reportedBase
	}
	if isUnqualifiedImageReference(requestedBase) && reportedBase != "" && !isDockerRegistryReference(reportedBase) {
		return reportedBase
	}
	return requestedBase
}

func appleImageReferencesCompatible(requested, actual string) bool {
	requestedDigest := imageDigest(requested)
	actualDigest := imageDigest(actual)
	if requestedDigest != "" && !validImageDigest(requestedDigest) {
		return false
	}
	if actualDigest != "" && !validImageDigest(actualDigest) {
		return false
	}
	if imagesCompatible(requested, actual) {
		return true
	}
	// A custom default registry is reported canonically by Apple, while the
	// caller may have supplied an unqualified name. When a digest is
	// present, the tag is only a mutable alias spelling: content and
	// repository provenance, not the tag, determine compatibility. If both
	// sides carry digests, their claims must agree; a descriptor-backed
	// identity is checked separately against the selected root/variant.
	if requestedDigest != "" && actualDigest != "" && !strings.EqualFold(requestedDigest, actualDigest) {
		return false
	}
	if requestedDigest != "" || actualDigest != "" {
		return imageRepositoryPathsCompatibleIgnoringTags(requested, actual)
	}
	return imageRepositoryPathsCompatible(requested, actual)
}

func imageRepositoryPathsCompatibleIgnoringTags(a, b string) bool {
	abase := imageRepositoryBaseWithoutTag(imageReferenceBase(a))
	bbase := imageRepositoryBaseWithoutTag(imageReferenceBase(b))
	return imageRepositoryPathsCompatible(abase, bbase)
}

// appleImageIDMatchesDescriptor verifies the only safe interpretation of
// an ID-shaped value supplied to Apple: the descriptor must independently
// report the same digest, and any non-empty Apple ID field must agree.
func appleImageIDMatchesDescriptor(requested, recordID, digest string) bool {
	requestedDigest := requested
	if isBareImageID(requested) {
		requestedDigest = "sha256:" + requested
	}
	if (!isImageID(requested) && !isBareImageID(requested)) || !validImageDigest(digest) || !strings.EqualFold(digest, requestedDigest) {
		return false
	}
	if recordID == "" {
		return true
	}
	if strings.HasPrefix(recordID, "sha256:") {
		return isImageID(recordID) && strings.EqualFold(recordID, digest)
	}
	return len(recordID) == 64 && domain.IsHex(recordID) && strings.EqualFold("sha256:"+recordID, digest)
}

func (appleEngine) platformCompatible(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
}

func (appleEngine) listReuseGroupArgs(string) []string {
	return []string{"ls", "--all", "--format", "json"}
}

func (appleEngine) parseReuseGroupIDs(data []byte, group string) ([]string, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		if c.Configuration.Labels[reuseGroupLabel] == group {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// nameConflict matches Apple Container's duplicate-name wording on a
// create/run command. Other commands may legitimately contain the words
// "already" or "exists" in application/configuration diagnostics.
func (appleEngine) nameConflict(err error) bool {
	return (appleEngine{}).nameConflictForTarget(err, "")
}

func (appleEngine) nameConflictForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "container")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if branch.ctx.operation != "" && branch.ctx.operation != "run" {
			continue
		}
		if !exactBranchTarget(branch, target) {
			continue
		}
		branchTarget := branch.ctx.target
		if hasAppleContainerizationError(branch.cause, "exists", func(message string) bool {
			return appleNameConflictLine(message, branchTarget) || appleServerConflictLine(message, branchTarget)
		}) {
			return true
		}
		if hasAppleContainerizationErrorPath(branch.cause, "internalerror", func(message string) bool {
			return message == "failed to create container"
		}, "exists", func(message string) bool {
			return appleServerConflictLine(message, branchTarget) || appleNameConflictLine(message, branchTarget)
		}) {
			return true
		}
		if hasBranchLine(branch, func(line string) bool {
			return appleNameConflictLine(line, branchTarget) || appleServerConflictLine(line, branchTarget)
		}) {
			return true
		}
		if hasBranchLine(branch, func(line string) bool {
			return appleTypedNameConflictLine(line, branchTarget)
		}) {
			return true
		}
	}
	return false
}

func appleNameConflictLine(line, target string) bool {
	line = strings.TrimSpace(strings.ToLower(line))
	if strings.Contains(line, "image configuration") || strings.Contains(line, "container configuration") {
		return false
	}
	if strings.HasPrefix(line, "error:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "error:"))
	}
	if rest, ok := strings.CutPrefix(line, appleStderrNameConflict); ok {
		rest = strings.TrimSpace(rest)
		suffix := " " + appleStderrAlreadyExists
		if strings.HasSuffix(rest, suffix) {
			id := strings.TrimSpace(strings.TrimSuffix(rest, suffix))
			id = strings.Trim(id, `"'`)
			if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
				(target == "" || sameCLITarget(id, target)) {
				return true
			}
		}
	}
	// Quoted create conflict: container "wanted" already exists
	if rest, ok := strings.CutPrefix(line, "container "); ok {
		rest = strings.TrimSpace(rest)
		suffix := " " + appleStderrAlreadyExists
		if strings.HasSuffix(rest, suffix) {
			id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, suffix)), `"'`)
			if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
				(target == "" || sameCLITarget(id, target)) {
				return true
			}
		}
	}
	if rest, ok := strings.CutPrefix(line, "already exists: container"); ok {
		id := strings.TrimSpace(rest)
		id = strings.Trim(id, `"'`)
		if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
			(target == "" || sameCLITarget(id, target)) {
			return true
		}
	}
	// Server-side create race spelling: "container already exists: <id>".
	if appleServerConflictLine(line, target) {
		return true
	}
	// Reject loose "already"/"exist"/"in use"/"taken" co-occurrence: that
	// matches application phrases such as "address already in use".
	return false
}

func appleServerConflictLine(message, target string) bool {
	rest, ok := strings.CutPrefix(message, "container already exists:")
	if !ok {
		return false
	}
	id := strings.Trim(strings.TrimSpace(rest), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || sameCLITarget(id, target))
}

// containerMissing matches the command-specific Apple Container forms
// for an absent container. The command, binary, and exact target are
// required so application output containing "not found" is not treated
// as a backend result. Typed envelopes are accepted through the escape-
// aware notFound unwrapper, then validated with operation-specific leaves.
func (appleEngine) containerMissing(err error) bool {
	ctx, ok := backendCLIError(err, "container")
	if !ok || !hasParsedCLITarget(ctx) {
		return false
	}
	switch ctx.operation {
	case "inspect", "exec", "stop", "delete", "rm", "logs":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleContainerMissingLine(line, ctx.target, ctx.operation)
		})
	default:
		return false
	}
}

func appleContainerMissingLine(line, target, command string) bool {
	if appleTypedNotFoundLine(line, target, command) {
		return true
	}
	switch command {
	case "inspect":
		rest, ok := strings.CutPrefix(line, "container not found:")
		return ok && cliTargetListMatches(rest, target)
	case "exec":
		return appleExecMissingLine(line, target)
	case "stop":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleStateMissingLine(line, target, "failed to stop container:")
	case "delete", "rm":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleStateMissingLine(line, target, "failed to delete container:")
	case "logs":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleLogsMissingLine(line, target) || appleExecMissingLine(line, target)
	default:
		return false
	}
}

// appleTypedNotFoundLine handles the structured ContainerizationError
// spelling emitted by Apple Container 1.3.0. The CLI prints descriptions
// such as notFound: "container not found: id" and can wrap that value in
// internalError: "..." (cause: "...") layers. Only these typed forms
// are accepted; an arbitrary application line containing "not found" is
// deliberately not a backend absence result.
func appleTypedNotFoundLine(line, target, command string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleContainerNotFoundMessage(line, message, target, command)
}

// appleTypedNotFoundMessage unwraps only the typed wrapper grammar. The
// cause value is treated as a quoted string at each level, then escapes are
// decoded before the next wrapper is examined. This handles both escaped
// forms emitted by the CLI and the older unescaped nested spelling without
// searching arbitrary stderr for the words "not found".
func appleTypedNotFoundMessage(line string) (string, bool) {
	current := strings.ToLower(strings.TrimSpace(line))
	if strings.HasPrefix(current, "error:") {
		current = strings.TrimSpace(strings.TrimPrefix(current, "error:"))
	}
	for range 16 {
		current = strings.TrimSpace(current)
		current = strings.TrimSpace(strings.Trim(current, "()"))
		switch {
		case strings.HasPrefix(current, "notfound:"):
			message := decodeAppleQuotedValue(strings.TrimSpace(strings.TrimPrefix(current, "notfound:")))
			return message, message != ""
		case strings.HasPrefix(current, "internalerror:"), strings.HasPrefix(current, "cause:"):
			index := strings.Index(current, "cause:")
			if index < 0 {
				return "", false
			}
			current = decodeAppleQuotedValue(strings.TrimSpace(current[index+len("cause:"):]))
			if current == "" {
				return "", false
			}
		default:
			return "", false
		}
	}
	return "", false
}

// decodeAppleQuotedValue removes one outer quoted value and decodes the
// escaping used when Swift's diagnostic description is rendered. The
// fallback is intentionally permissive for the CLI's older unescaped nested
// form, but it still requires the value to be bounded by a quote.
func decodeAppleQuotedValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		value = value[1:]
		if value[len(value)-1] == quote {
			value = value[:len(value)-1]
		}
	}
	var b strings.Builder
	b.Grow(len(value))
	escaped := false
	for _, r := range value {
		if !escaped {
			if r == '\\' {
				escaped = true
				continue
			}
			b.WriteRune(r)
			continue
		}
		switch r {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"', '\\':
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
		escaped = false
	}
	if escaped {
		b.WriteByte('\\')
	}
	return strings.TrimSpace(b.String())
}

func appleContainerNotFoundMessage(line, message, target, command string) bool {
	message = strings.TrimSpace(strings.Trim(message, `"'`))
	generic, hasGeneric := strings.CutPrefix(message, "container not found:")
	lowerLine := strings.ToLower(line)
	switch command {
	case "inspect":
		// Inspect emits the generic list/inspect spelling. A lifecycle
		// ID error or an exec-specific get failure is not an inspect
		// absence result.
		return hasGeneric && cliTargetListMatches(generic, target)
	case "exec":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(lowerLine, "failed to get container") ||
				strings.Contains(lowerLine, "operation failed")
		}
		return appleIDMessageMatches(message, target) || appleExecMissingLine(message, target)
	case "stop":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return true
		}
		// Bare notFound ID spelling is a real Apple 1.3 stop form.
		return appleIDMessageMatches(message, target)
	case "delete", "rm":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(lowerLine, "failed to delete container") ||
				strings.Contains(lowerLine, "operation failed")
		}
		// Bare notFound ID envelopes without a delete root are application
		// output, not a backend delete absence.
		return strings.Contains(lowerLine, "failed to delete container") &&
			appleIDMessageMatches(message, target)
	case "logs":
		// A get-logs root that names another container must not be accepted
		// just because a nested open-logs cause mentions the argv target.
		if idx := strings.Index(lowerLine, appleLogsGetPrefix); idx >= 0 {
			rest := lowerLine[idx+len(appleLogsGetPrefix):]
			if end := strings.IndexAny(rest, `"'`); end >= 0 {
				rest = rest[:end]
			}
			rootID := strings.TrimSpace(rest)
			if rootID != "" && !sameCLITarget(rootID, target) {
				return false
			}
		}
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(lowerLine, "failed to get logs") ||
				strings.Contains(lowerLine, "operation failed") ||
				strings.Contains(lowerLine, appleLogsOpenPrefix)
		}
		if appleLogsEnvelopeRoot(message, target) ||
			appleLogsOpenMissingLine(message, target) ||
			appleLogsMissingLine(message, target) {
			return true
		}
		if appleIDMessageMatches(message, target) || appleExecMissingLine(message, target) {
			return strings.Contains(lowerLine, "failed to get logs") ||
				strings.Contains(lowerLine, "operation failed") ||
				strings.Contains(lowerLine, appleLogsOpenPrefix)
		}
		return false
	default:
		return false
	}
}

func appleIDMessageMatches(message, target string) bool {
	return appleIDMissingLine(strings.TrimSpace(strings.Trim(message, `"'`)), target)
}

func appleExecMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "get failed:")
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	rest, ok = strings.CutPrefix(rest, "container ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || strings.EqualFold(id, target))
}

func appleIDMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "container with id ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || strings.EqualFold(id, target))
}

func appleStateMissingLine(line, target, wrapper string) bool {
	current := line
	for range 3 {
		if appleIDMissingLine(current, target) {
			return true
		}
		rest, ok := strings.CutPrefix(current, wrapper)
		if !ok {
			return false
		}
		current = strings.TrimSpace(rest)
	}
	return false
}

func appleLogsEnvelopeRoot(message, target string) bool {
	// The 1.2/1.3 logs envelope roots at "failed to get logs for container
	// <id>" with nothing after the ID, so an explanatory suffix (an
	// application's own sentence) cannot satisfy the anchor.
	rest, ok := strings.CutPrefix(message, appleLogsGetPrefix)
	return ok && sameCLITarget(rest, target)
}

// appleLogsOpenMissingLine accepts the notFound cause of that envelope: the
// absence reported behind the same "failed to open container logs: " marker
// the flat spelling uses, followed by one of the known missing-ID leaves.
func appleLogsOpenMissingLine(message, target string) bool {
	rest, ok := strings.CutPrefix(message, appleLogsOpenPrefix)
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	return appleIDMissingLine(rest, target) || appleExecMissingLine(rest, target)
}

func appleLogsMissingLine(line, target string) bool {
	current := line
	// Flat logs absence roots at a logs-specific wrapper. A bare notFound
	// ID leaf without get-logs/open-logs anchoring is application output.
	if rest, ok := strings.CutPrefix(current, appleLogsGetPrefix); ok {
		rest = strings.TrimSpace(rest)
		separator := strings.Index(rest, ":")
		if separator < 0 {
			return false
		}
		logID := strings.TrimSpace(rest[:separator])
		if logID == "" || (target != "" && !sameCLITarget(logID, target)) {
			return false
		}
		current = strings.TrimSpace(rest[separator+1:])
	} else if rest, ok := strings.CutPrefix(current, appleLogsOpenPrefix); ok {
		current = strings.TrimSpace(rest)
	} else {
		return false
	}

	// Client and API versions wrap the same absence in a small, known set
	// of prefixes. Bound the depth so arbitrary nested diagnostics cannot
	// grow the classifier without limit.
	for range 3 {
		if appleIDMissingLine(current, target) || appleExecMissingLine(current, target) {
			return true
		}
		if rest, ok := strings.CutPrefix(current, appleLogsOpenPrefix); ok {
			current = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(current, "get failed:"); ok {
			current = strings.TrimSpace(rest)
			rest, ok = strings.CutPrefix(current, "container ")
			if ok && strings.HasSuffix(rest, " not found") {
				id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
				if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
					(target == "" || sameCLITarget(id, target)) {
					return true
				}
			}
			continue
		}
		return false
	}
	return false
}
