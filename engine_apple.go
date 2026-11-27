package container

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/hirokazumiyaji/container-go/wait"
)

// appleEngine drives Apple Container's `container` CLI.
type appleEngine struct{}

// These limits mirror the checks in Apple Container 1.2.2 and 1.3.0
// (Parser.resources, PublishPort, Utility, and ContainersService). Keeping
// them here makes a bad request fail before any image fetch or container
// create is attempted.
const (
	appleMinMemoryBytes     uint64 = 200 * 1024 * 1024
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

// checkConfig resolves environment-provided defaults, applies the pure Apple
// option contract, and normalizes the resulting selector. Apple's CLI contract
// is state-independent, so the imperative part is limited to reading the
// process default platform instead of probing the backend.
func (appleEngine) checkConfig(_ context.Context, cfg *config) error {
	// resolveEffectivePlatform falls back to CONTAINER_DEFAULT_PLATFORM, so
	// keep the origin to report an environment-variable failure as such.
	explicitPlatform := cfg.platform != ""
	if err := resolveEffectivePlatform(cfg); err != nil {
		if explicitPlatform {
			return appleValidationError("WithPlatform", cfg.platform, "%v", err)
		}
		return appleValidationError(defaultPlatformEnv, os.Getenv(defaultPlatformEnv), "%v", err)
	}
	if err := (appleEngine{}).validateConfig(cfg, explicitPlatform); err != nil {
		return err
	}
	if cfg.platform != "" {
		cfg.platform = canonicalizeApplePlatformSelector(cfg.platform)
	}
	return nil
}

// validateConfig applies the parts of Apple Container's CLI contract that are
// independent of the current service state. The Apple CLI performs these
// checks only after it has already resolved the image, so doing them here
// avoids surprising pulls and makes the backend limits visible to callers.
//
// It is the pure core of checkConfig: it reads only cfg, runs no subprocess,
// and never mutates cfg.
func (appleEngine) validateConfig(cfg *config, explicitPlatform bool) error {
	if wait.RequiresHealthcheck(cfg.waitStrategy) {
		return appleCapabilityError(ErrUnsupportedCapability, "WithWaitStrategy", "wait strategy", "ForHealthcheck",
			"Apple Container has no HEALTHCHECK; wait.ForHealthcheck requires the Docker backend")
	}
	if len(cfg.networkAliases) > 0 {
		return appleCapabilityError(ErrUnsupportedCapability, "WithNetworkAliases", "network aliases", len(cfg.networkAliases),
			"apple backend does not support network aliases")
	}
	if cfg.pullPolicy == PullNever {
		return appleCapabilityError(ErrPullNeverUnsupported, "WithPullPolicy", "pull policy", cfg.pullPolicy,
			"Apple Container's run command always resolves images; use PullMissing or PullAlways")
	}
	if cfg.privileged {
		return appleCapabilityError(ErrUnsupportedCapability, "WithPrivileged", "privileged", true,
			"apple backend does not support WithPrivileged")
	}
	if len(cfg.extraHosts) > 0 {
		return appleCapabilityError(ErrUnsupportedCapability, "WithExtraHost", "extra hosts", len(cfg.extraHosts),
			"apple backend does not support WithExtraHost")
	}
	if cfg.hostname != "" {
		return appleCapabilityError(ErrUnsupportedCapability, "WithHostname", "hostname", cfg.hostname,
			"apple backend does not support WithHostname")
	}
	if cfg.cpus > 0 && cfg.cpus != math.Trunc(cfg.cpus) {
		return appleCapabilityError(ErrUnsupportedCapability, "WithCPUs", "cpus", cfg.cpus,
			"apple backend requires a whole-number CPU count, got %s", formatCPUs(cfg.cpus))
	}
	if cfg.platform != "" {
		if err := validateApplePlatform(cfg.platform); err != nil {
			if explicitPlatform {
				return appleValidationError("WithPlatform", cfg.platform, "%v", err)
			}
			return appleValidationError(defaultPlatformEnv, cfg.platform, "%v", err)
		}
	}

	if cfg.name != "" && !appleContainerNameRE.MatchString(cfg.name) {
		return appleValidationError("WithName", cfg.name,
			"invalid Apple container name %q: must be 2-63 characters and match %s", cfg.name, appleContainerNameRE)
	}
	if cfg.network != "" {
		// Apple accepts comma-separated network properties, but this
		// public option intentionally represents a name only. Rejecting
		// the separator here avoids passing an option string as a name.
		if strings.Contains(cfg.network, ",") {
			return appleValidationError("WithNetwork", cfg.network,
				"invalid Apple network name %q: comma-separated network properties are not supported", cfg.network)
		}
		if !appleNetworkNameRE.MatchString(cfg.network) {
			return appleValidationError("WithNetwork", cfg.network,
				"invalid Apple network name %q: must match %s", cfg.network, appleNetworkNameRE)
		}
	}

	if len(cfg.published) > applePublishedPortLimit {
		return appleValidationError("WithPublishedPort", len(cfg.published),
			"apple backend supports at most %d published-port descriptors, got %d", applePublishedPortLimit, len(cfg.published))
	}
	seenPublished := make(map[string]struct{}, len(cfg.published))
	for _, p := range cfg.published {
		if p.HostPort <= 1 || p.ContainerPort <= 1 {
			return appleValidationError("WithPublishedPort", p.Raw,
				"apple backend requires published ports to be 2-65535, got %q", p.Raw)
		}
		// Match Apple Container's PublishPort.hasOverlaps key:
		// "\(hostPort)/\(proto)" without hostAddress. Distinct bind
		// addresses on the same host port and protocol still overlap
		// upstream, so include them here before the CLI is called.
		key := fmt.Sprintf("%d/%s", p.HostPort, p.Proto)
		if _, exists := seenPublished[key]; exists {
			return appleValidationError("WithPublishedPort", p.Raw,
				"apple backend published-port descriptors overlap at %q", p.Raw)
		}
		seenPublished[key] = struct{}{}
	}

	if !cfg.memory.IsZero() && cfg.memory.Bytes() < appleMinMemoryBytes {
		return appleValidationError("WithMemory", cfg.memory.String(),
			"apple backend requires at least 200 MiB of memory, got %q", cfg.memory.String())
	}
	return nil
}

// appleValidationError reports an Apple-specific option value the backend
// rejects. It returns a *ValidationError whose cause also matches
// ErrInvalidConfig, so callers classifying a backend configuration failure
// can use either contract.
func appleValidationError(option string, value any, format string, args ...any) error {
	return newValidationError(option, value, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...)))
}

// appleCapabilityError reports an Apple option the backend cannot honor. The
// capability sentinel stays first so errors.Is keeps working, and the option
// is exposed as a *ValidationError without classifying a capability gap as
// ErrInvalidConfig.
func appleCapabilityError(sentinel error, option, field string, value any, format string, args ...any) error {
	validationErr := newValidationErrorWithField(option, field, value, fmt.Errorf(format, args...))
	return fmt.Errorf("%w: %w", sentinel, validationErr)
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
	return platform == "" || platformMetadataCompatible(platform, identity.platform)
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

func (appleEngine) platformCompatible(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
}

// normalizePlatformSelector rewrites an Apple selector to the canonical
// os/arch/variant spelling Apple reports from container inspect, so
// architecture aliases (aarch64, x86_64) and default variants compare
// equal after a round trip through WithReuse.
func (appleEngine) normalizePlatformSelector(selector string) string {
	parsed, ok := parseApplePlatformSelector(selector)
	if !ok {
		return selector
	}
	return formatApplePlatform(parsed)
}
