package container

import (
	"fmt"
	"os"
)

const defaultPlatformEnv = "CONTAINER_DEFAULT_PLATFORM"

// resolveEffectivePlatform applies Apple Container's default platform setting
// to the configuration once. An explicit WithPlatform value always wins, as
// it does in the Apple CLI.
func resolveEffectivePlatform(cfg *config) error {
	if cfg.platform != "" {
		if !platformRE.MatchString(cfg.platform) {
			return fmt.Errorf("invalid platform %q", cfg.platform)
		}
		return nil
	}

	platform := os.Getenv(defaultPlatformEnv)
	if platform == "" {
		return nil
	}
	if !platformRE.MatchString(platform) {
		return fmt.Errorf("invalid platform %q in %s", platform, defaultPlatformEnv)
	}
	cfg.platform = platform
	return nil
}
