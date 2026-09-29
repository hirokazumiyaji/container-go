//go:build integration

package bench

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
	tc "github.com/testcontainers/testcontainers-go"
)

func configureTestcontainersDockerEndpoint(tb testing.TB, backend ibench.Backend) ibench.BackendProvenance {
	tb.Helper()
	provenance, err := backend.CaptureProvenance()
	if err != nil {
		tb.Fatalf("capture Docker endpoint for Testcontainers: %v", err)
	}
	if err := validateTestcontainersDockerEndpoint(provenance.Endpoint); err != nil {
		tb.Fatal(err)
	}
	// DOCKER_HOST is the transport understood by the Docker SDK used by
	// testcontainers-go. DOCKER_CONTEXT is set to the same captured context
	// for clients that honor it; neither value is allowed to drift afterward.
	tb.Setenv("DOCKER_HOST", provenance.Endpoint)
	tb.Setenv("DOCKER_CONTEXT", provenance.Context)
	return provenance
}

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
	// later config read. The reaper image is forced to the pinned immutable
	// reference: the mutable tag the dependency requests by default is only
	// accepted later because the harness maps it to the same content.
	for name, value := range canonicalTestcontainersEnvironment() {
		tb.Setenv(name, value)
	}

	// The forced environment is held to the same policy as the inherited
	// one, and the image is checked against the pin directly, so a value that
	// does not survive forcing fails before anything is measured.
	if err := validateTestcontainersConfiguration(); err != nil {
		tb.Fatalf("forced testcontainers configuration was rejected: %v", err)
	}
	if err := validateCanonicalRyukImageEnvironment(os.Getenv); err != nil {
		tb.Fatal(err)
	}
	//nolint:staticcheck // verify the dependency's default before starting a reaper.
	if tc.ReaperDefaultImage != ibench.TestcontainersRyukTag {
		tb.Fatalf("testcontainers Ryuk default = %q, want pinned tag %q", tc.ReaperDefaultImage, ibench.TestcontainersRyukTag)
	}

	// ReadConfig memoizes, so it is called once, after the canonical values
	// are in place, and its result is the configuration the reaper will use.
	config := tc.ReadConfig()
	if err := canonicalTestcontainersConfigError(config); err != nil {
		tb.Fatal(err)
	}
	sessionID := strings.TrimSpace(config.Config.SessionID)
	if !validBenchmarkSessionID(sessionID) {
		tb.Fatalf("testcontainers generated an invalid reaper session ID %q", sessionID)
	}
	return sessionID
}

// canonicalTestcontainersConfigError reports every resolved setting that does
// not match the configuration the benchmark records. The deprecated mirror
// fields are checked alongside the current ones so a release that stops
// mirroring them cannot let a non-canonical value through.
func canonicalTestcontainersConfigError(config tc.TestcontainersConfig) error {
	var problems []string
	if config.RyukDisabled || config.Config.RyukDisabled {
		problems = append(problems, "Ryuk must be enabled")
	}
	if config.RyukPrivileged || config.Config.RyukPrivileged {
		problems = append(problems, "Ryuk must be non-privileged")
	}
	if config.Config.RyukVerbose {
		problems = append(problems, "Ryuk verbose logging must be disabled")
	}
	if config.Config.HubImageNamePrefix != "" {
		problems = append(problems, fmt.Sprintf("image name prefix %q must be empty", config.Config.HubImageNamePrefix))
	}
	if config.Config.RyukReconnectionTimeout != 10*time.Second {
		problems = append(problems, fmt.Sprintf("Ryuk reconnection timeout %s must be %s", config.Config.RyukReconnectionTimeout, 10*time.Second))
	}
	if config.Config.RyukConnectionTimeout != time.Minute {
		problems = append(problems, fmt.Sprintf("Ryuk connection timeout %s must be %s", config.Config.RyukConnectionTimeout, time.Minute))
	}
	if config.Host != "" || config.Config.Host != "" {
		problems = append(problems, fmt.Sprintf("Docker host override host=%q config=%q must be empty", config.Host, config.Config.Host))
	}
	if config.Config.TestcontainersHost != "" {
		problems = append(problems, fmt.Sprintf("Docker socket override %q must be empty", config.Config.TestcontainersHost))
	}
	if config.TLSVerify != 0 || config.Config.TLSVerify != 0 {
		problems = append(problems, fmt.Sprintf("Docker TLS verification tls=%d config=%d must be 0", config.TLSVerify, config.Config.TLSVerify))
	}
	if config.CertPath != "" || config.Config.CertPath != "" {
		problems = append(problems, fmt.Sprintf("Docker certificate path cert=%q config=%q must be empty", config.CertPath, config.Config.CertPath))
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
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
