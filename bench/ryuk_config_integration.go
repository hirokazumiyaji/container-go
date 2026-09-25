//go:build integration

package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tc "github.com/testcontainers/testcontainers-go"
)

// requireCanonicalTestcontainersConfig makes the benchmark fail closed
// before testcontainers reads its process-wide configuration. A benchmark
// that accepted a disabled or rewritten reaper would not measure the pinned
// Ryuk image recorded in the result document.
func requireCanonicalTestcontainersConfig(tb testing.TB) string {
	tb.Helper()
	for name := range testcontainersBenchmarkOverrides {
		if err := validateTestcontainersEnvironmentOverride(name, os.Getenv(name)); err != nil {
			tb.Fatal(err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		tb.Fatalf("resolve testcontainers home directory: %v", err)
	}
	if properties, err := os.ReadFile(filepath.Join(home, ".testcontainers.properties")); err == nil {
		if err := validateTestcontainersProperties(string(properties)); err != nil {
			tb.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		tb.Fatalf("read testcontainers properties: %v", err)
	}

	// Override a disabled property value as well as an inherited false
	// value. Empty prefix/session values cannot override those properties,
	// which is why non-empty property values were rejected above.
	tb.Setenv("TESTCONTAINERS_RYUK_DISABLED", "false")
	config := tc.ReadConfig()
	if config.RyukDisabled || config.Config.RyukDisabled {
		tb.Fatal("testcontainers Ryuk must be enabled for the benchmark")
	}
	if config.Config.HubImageNamePrefix != "" {
		tb.Fatalf("testcontainers image prefix override is not allowed: %q", config.Config.HubImageNamePrefix)
	}
	sessionID := strings.TrimSpace(config.Config.SessionID)
	if !validBenchmarkSessionID(sessionID) {
		tb.Fatalf("testcontainers generated an invalid reaper session ID %q", sessionID)
	}
	return sessionID
}
