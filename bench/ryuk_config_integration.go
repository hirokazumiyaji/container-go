//go:build integration

package bench

import (
	"errors"
	"strings"
	"testing"
	"time"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
	tc "github.com/testcontainers/testcontainers-go"
)

// requireCanonicalTestcontainersConfig makes the benchmark fail closed
// before testcontainers reads its process-wide configuration. A benchmark
// that accepted a disabled or rewritten reaper would not measure the pinned
// Ryuk image recorded in the result document.
func requireCanonicalTestcontainersConfig(tb testing.TB) string {
	tb.Helper()
	if err := validateTestcontainersConfiguration(); err != nil {
		tb.Fatal(err)
	}

	// Set the canonical values explicitly so an inherited false value (or a
	// property that testcontainers decodes as false) cannot be changed by a
	// later config read.
	tb.Setenv("TESTCONTAINERS_RYUK_DISABLED", "false")
	tb.Setenv("TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED", "false")
	tb.Setenv("RYUK_VERBOSE", "false")
	tb.Setenv("TESTCONTAINERS_RYUK_VERBOSE", "false")
	config := tc.ReadConfig()
	if config.RyukDisabled || config.Config.RyukDisabled || config.RyukPrivileged || config.Config.RyukPrivileged || config.Config.RyukVerbose {
		tb.Fatal("testcontainers Ryuk must be enabled and non-privileged with verbose logging disabled")
	}
	if config.Config.HubImageNamePrefix != "" || config.Config.SessionID == "" {
		tb.Fatalf("testcontainers image/session configuration was not canonical: prefix=%q session=%q", config.Config.HubImageNamePrefix, config.Config.SessionID)
	}
	if config.Config.RyukReconnectionTimeout != 10*time.Second || config.Config.RyukConnectionTimeout != time.Minute {
		tb.Fatalf("testcontainers Ryuk timeouts are not canonical: reconnection=%s connection=%s", config.Config.RyukReconnectionTimeout, config.Config.RyukConnectionTimeout)
	}
	if config.Config.TestcontainersHost != "" || config.Config.Host != "" || config.Config.TLSVerify != 0 || config.Config.CertPath != "" {
		tb.Fatalf("testcontainers Docker connection overrides are not canonical: host=%q socket=%q tls=%d cert=%q", config.Config.Host, config.Config.TestcontainersHost, config.Config.TLSVerify, config.Config.CertPath)
	}
	sessionID := strings.TrimSpace(config.Config.SessionID)
	if !validBenchmarkSessionID(sessionID) {
		tb.Fatalf("testcontainers generated an invalid reaper session ID %q", sessionID)
	}
	return sessionID
}

// requireFreshTestcontainersSession verifies that the generated session has
// no pre-existing reaper. Reusing a stale reaper would make the recorded
// session identity describe a container from an earlier process.
func requireFreshTestcontainersSession(tb testing.TB, backend ibench.Backend, sessionID string) {
	tb.Helper()
	if backend.ContainerInspect == nil {
		tb.Fatal("Docker container inspection is required to verify a fresh testcontainers session")
	}
	name := "reaper_" + sessionID
	identity, err := backend.ContainerInspect(name)
	if err == nil {
		tb.Fatalf("testcontainers session %q already has reaper %q (%s); refusing a reused session", sessionID, name, identity.ID)
	}
	if !errors.Is(err, ibench.ErrContainerNotFound) {
		tb.Fatalf("check testcontainers reaper freshness for %q: %v", name, err)
	}
}
