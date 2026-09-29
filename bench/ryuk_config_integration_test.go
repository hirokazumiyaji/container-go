//go:build integration

package bench

import (
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
)

// The preflight asserts the configuration testcontainers resolved, not the one
// it was asked for, so a property or environment value that survived the
// policy checks still fails before a session is measured.
func TestCanonicalTestcontainersConfigErrorCoversResolvedFields(t *testing.T) {
	canonical := tc.TestcontainersConfig{}
	canonical.Config.RyukReconnectionTimeout = 10 * time.Second
	canonical.Config.RyukConnectionTimeout = time.Minute
	if err := canonicalTestcontainersConfigError(canonical); err != nil {
		t.Fatalf("canonical resolved configuration rejected: %v", err)
	}

	for name, mutate := range map[string]func(*tc.TestcontainersConfig){
		"disabled reaper":        func(c *tc.TestcontainersConfig) { c.Config.RyukDisabled = true },
		"privileged reaper":      func(c *tc.TestcontainersConfig) { c.Config.RyukPrivileged = true },
		"verbose reaper":         func(c *tc.TestcontainersConfig) { c.Config.RyukVerbose = true },
		"image name prefix":      func(c *tc.TestcontainersConfig) { c.Config.HubImageNamePrefix = "mirror.example/" },
		"reconnection timeout":   func(c *tc.TestcontainersConfig) { c.Config.RyukReconnectionTimeout = 30 * time.Second },
		"connection timeout":     func(c *tc.TestcontainersConfig) { c.Config.RyukConnectionTimeout = 2 * time.Minute },
		"docker host":            func(c *tc.TestcontainersConfig) { c.Config.Host = "tcp://127.0.0.1:2375" },
		"docker socket override": func(c *tc.TestcontainersConfig) { c.Config.TestcontainersHost = "/var/run/docker.sock" },
		"tls verification":       func(c *tc.TestcontainersConfig) { c.Config.TLSVerify = 1 },
		"certificate path":       func(c *tc.TestcontainersConfig) { c.Config.CertPath = "/tmp/certs" },
		// The deprecated mirrors are checked too, so a release that stops
		// mirroring them cannot let a non-canonical value through.
		"deprecated host mirror":  func(c *tc.TestcontainersConfig) { c.Host = "tcp://127.0.0.1:2375" },
		"deprecated tls mirror":   func(c *tc.TestcontainersConfig) { c.TLSVerify = 1 },
		"deprecated cert mirror":  func(c *tc.TestcontainersConfig) { c.CertPath = "/tmp/certs" },
		"deprecated disable flag": func(c *tc.TestcontainersConfig) { c.RyukDisabled = true },
		"deprecated privilege":    func(c *tc.TestcontainersConfig) { c.RyukPrivileged = true },
	} {
		resolved := canonical
		mutate(&resolved)
		if err := canonicalTestcontainersConfigError(resolved); err == nil {
			t.Errorf("resolved configuration with %s was accepted", name)
		}
	}

	// The session ID is validated by the caller, which needs its value, so
	// it is deliberately not part of this comparison.
	session := canonical
	session.Config.SessionID = "not a session"
	if err := canonicalTestcontainersConfigError(session); err != nil {
		t.Errorf("session ID belongs to the caller, not this comparison: %v", err)
	}
}
