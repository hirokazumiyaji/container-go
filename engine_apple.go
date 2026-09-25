package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

// appleEngine drives Apple Container's `container` CLI.
type appleEngine struct{}

// Verified against Apple Container CLI source/help and inspect fixtures for
// 1.2.2 and 1.3.0. API-server versions are runtime-reported and are not
// inferred from the CLI version. Stderr substrings below are matched
// case-insensitively on CLIError.Stderr.
// Sources (apple/container):
//   - name conflict: ContainerRun.swift throws ContainerizationError(.exists,
//     message: "container with id \(id) already exists")
//   - image missing / container missing: ContainerizationError(.notFound)
//     surfaces as "image not found: …" / "container not found: …"
const (
	appleStderrAlready   = "already"
	appleStderrExist     = "exist"
	appleStderrInUse     = "in use"
	appleStderrTaken     = "taken"
	appleStderrNotFound  = "not found"
	appleStderrNoSuchObj = "no such object"    // defensive; not observed on 1.3.0
	appleStderrNoSuchCtr = "no such container" // defensive; not observed on 1.3.0

	// These limits mirror the checks in Apple Container 1.2.2 and
	// 1.3.0 (Parser.resources, PublishPort, Utility, and
	// ContainersService). Keeping them here makes a bad request fail before
	// any image fetch or container create is attempted.
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

// validateApplePlatform mirrors ContainerizationOCI.Platform's grammar while
// applying Apple Container's Linux-only run/create contract. Apple requires an
// architecture and only accepts variants defined for that architecture.
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

	arch := parts[1]
	if len(parts) == 2 {
		return nil
	}
	variant := parts[2]
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

// applePlatform is the normalized form used when matching an image
// descriptor to a requested platform. It follows the normalization in
// ContainerizationOCI.Platform: architecture aliases are folded to their OCI
// names and the default arm64 variant (nil) is equivalent to v8.
type applePlatform struct {
	os           string
	architecture string
	variant      string
}

// parseApplePlatformSelector parses a platform selector using the same
// defaults as ContainerizationOCI.Platform(from:). In particular, a bare
// arm/armhf/armel selector means v7, a bare aarch64/arm64 selector means v8,
// and x86 aliases default to no variant.
func parseApplePlatformSelector(platform string) (applePlatform, bool) {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return applePlatform{}, false
	}
	if len(parts) == 3 && parts[2] == "" {
		return applePlatform{}, false
	}

	variant := ""
	if len(parts) == 3 {
		variant = parts[2]
	} else {
		switch parts[1] {
		case "arm", "armhf", "armel":
			variant = "v7"
		case "aarch64", "arm64":
			variant = "v8"
		}
	}
	return canonicalApplePlatform(parts[0], parts[1], variant), true
}

// canonicalApplePlatform normalizes a selector or an inspect descriptor.
// Apple treats the architecture aliases as equivalent and treats a nil arm64
// variant as v8. The explicit v1 x86 spelling is equivalent to the canonical
// no-variant amd64 form; v8/8 is likewise normalized for arm64.
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
		// The aliases normalize to arm, but a descriptor's nil arm
		// variant remains unknown; Apple only defaults nil to v8 for
		// arm64.
		p.architecture = "arm"
	}
	return p
}

func applePlatformsEqual(want, have applePlatform) bool {
	return want.os == have.os &&
		want.architecture == have.architecture &&
		want.variant == have.variant
}

// checkConfig applies the parts of Apple Container's CLI contract that are
// independent of the current service state. The Apple CLI performs these
// checks only after it has already resolved the image, so doing them here
// avoids surprising pulls and makes the backend limits visible to callers.
func (appleEngine) checkConfig(cfg *config) error {
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
		// PublishPort.hasOverlaps in Apple Container keys on host port
		// and protocol, regardless of the host address.
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
		default:
			return 0, fmt.Errorf("invalid Apple memory unit in %q", size)
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

func (appleEngine) defaultHost() string { return "127.0.0.1" }

func (appleEngine) probe() cli.Probe {
	return cli.Probe{Args: []string{"system", "status"}, Hint: "run `container system start`"}
}

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	return append(args, cfg.commonRunArgs(image, envFile, nil)...)
}

func (appleEngine) parseRunID([]byte) string { return "" }

func (appleEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

func (appleEngine) parseInspect(data []byte, id string) (*engineInfo, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.ID != id {
			continue
		}
		info := &engineInfo{
			state:  State(c.Status.State),
			labels: c.Configuration.Labels,
			image:  c.Configuration.Image.Reference,
		}
		if ip, err := c.IPv4(); err == nil {
			info.ip = ip
		}
		for _, p := range c.Configuration.PublishedPorts {
			info.bound = append(info.bound, boundPort{
				containerPort: p.ContainerPort,
				proto:         p.Proto,
				hostAddr:      p.HostAddress,
				hostPort:      p.HostPort,
			})
		}
		return info, nil
	}
	return nil, fmt.Errorf("container %s not in inspect output", id)
}

func (appleEngine) stopArgs(id string, timeout *time.Duration) []string {
	args := []string{"stop"}
	if timeout != nil {
		args = append(args, "--time", strconv.Itoa(int(timeout.Seconds())))
	}
	return append(args, id)
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

func (appleEngine) reaperSubcommand() string { return "delete" }

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

func (appleEngine) logsArgs(id string, follow bool) []string {
	if follow {
		return []string{"logs", "--follow", id}
	}
	return []string{"logs", id}
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

func (appleEngine) imageInspectArgs(image, _ string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "pull", "--platform", platform, image}
	}
	return []string{"image", "pull", image}
}

// imageMissing matches the CLI's error for an absent image.
func (appleEngine) imageMissing(err error) bool {
	return appleStderrContains(err, appleStderrNotFound)
}

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	// Keep the caller's selector unchanged. It is also the value used for
	// pull/run argv and the pull-flight key; normalization here must only
	// affect comparison with inspect output.
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return false
	}
	if platform == "" {
		return true
	}
	want, ok := parseApplePlatformSelector(platform)
	if !ok {
		return false
	}
	var images []struct {
		Variants []struct {
			Platform struct {
				Os           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"variants"`
	}
	if err := json.Unmarshal(data, &images); err != nil {
		return true
	}
	for _, img := range images {
		if len(img.Variants) == 0 {
			return true
		}
		for _, v := range img.Variants {
			have := canonicalApplePlatform(v.Platform.Os, v.Platform.Architecture, v.Platform.Variant)
			if applePlatformsEqual(want, have) {
				return true
			}
		}
	}
	return false
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

// nameConflict matches Apple Container's duplicate-name wording.
func (appleEngine) nameConflict(err error) bool {
	s, ok := appleCLIStderr(err)
	if !ok {
		return false
	}
	return strings.Contains(s, appleStderrAlready) &&
		(strings.Contains(s, appleStderrExist) ||
			strings.Contains(s, appleStderrInUse) ||
			strings.Contains(s, appleStderrTaken))
}

// containerMissing matches a CLI failure for an absent container.
func (appleEngine) containerMissing(err error) bool {
	s, ok := appleCLIStderr(err)
	if !ok {
		return false
	}
	return strings.Contains(s, appleStderrNotFound) ||
		strings.Contains(s, appleStderrNoSuchObj) ||
		strings.Contains(s, appleStderrNoSuchCtr)
}

func appleCLIStderr(err error) (string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return "", false
	}
	return strings.ToLower(cliErr.Stderr), true
}

func appleStderrContains(err error, substr string) bool {
	s, ok := appleCLIStderr(err)
	return ok && strings.Contains(s, substr)
}
