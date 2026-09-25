package container

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	appleMinMemoryBytes     uint64 = 200 * 1024 * 1024
	appleMaxMemoryBytes     uint64 = 1<<63 - 1
	applePublishedPortLimit        = 64
)

var (
	appleContainerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`)
	appleNetworkNameRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)
)

func validateApplePlatform(platform string) error {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
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

func appleMemoryBytes(size string) (uint64, error) {
	if !memoryRE.MatchString(size) {
		return 0, fmt.Errorf("invalid Apple memory size %q", size)
	}
	digits, multiplier := size, uint64(1)
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
		if strings.Contains(cfg.network, ",") {
			return fmt.Errorf("invalid Apple network name %q: comma-separated properties are not supported", cfg.network)
		}
		if !appleNetworkNameRE.MatchString(cfg.network) {
			return fmt.Errorf("invalid Apple network name %q: must match %s", cfg.network, appleNetworkNameRE)
		}
	}
	if len(cfg.published) > applePublishedPortLimit {
		return fmt.Errorf("apple backend supports at most %d published-port descriptors, got %d", applePublishedPortLimit, len(cfg.published))
	}
	seen := make(map[string]struct{}, len(cfg.published))
	for _, p := range cfg.published {
		if p.hostPort <= 1 || p.containerPort <= 1 {
			return fmt.Errorf("apple backend requires published ports to be 2-65535, got %q", p.raw)
		}
		key := fmt.Sprintf("%d/%s", p.hostPort, p.proto)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("apple backend published-port descriptors overlap at %q", p.raw)
		}
		seen[key] = struct{}{}
	}
	if cfg.memory != "" {
		if _, err := appleMemoryBytes(cfg.memory); err != nil {
			return err
		}
	}
	return nil
}
