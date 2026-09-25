package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/magiconair/properties"
)

// testcontainersBenchmarkOverrides lists every Testcontainers/Ryuk setting
// that changes the reaper or Docker connection semantics used by this
// benchmark. Empty values are harmless; non-empty values must equal the
// canonical value below. DOCKER_HOST and DOCKER_CONTEXT are deliberately not
// listed here: the Docker provenance capture records and validates those
// effective selections instead. TESTCONTAINERS_CONFIG is handled separately
// by testcontainersPropertiesPaths so its effective file is parsed too.
var testcontainersBenchmarkOverrides = map[string]string{
	"TESTCONTAINERS_RYUK_DISABLED":             "false",
	"TESTCONTAINERS_RYUK_IMAGE":                "",
	"TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED": "false",
	"RYUK_IMAGE":                               "",
	"RYUK_VERBOSE":                             "false",
	"TESTCONTAINERS_RYUK_VERBOSE":              "false",
	"RYUK_RECONNECTION_TIMEOUT":                "10s",
	"TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT": "10s",
	"RYUK_CONNECTION_TIMEOUT":                  "1m",
	"TESTCONTAINERS_RYUK_CONNECTION_TIMEOUT":   "1m",
	"TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX":     "",
	"TESTCONTAINERS_SESSION_ID":                "",
	"RYUK_PORT":                                "",
	"TESTCONTAINERS_HOST_OVERRIDE":             "",
	"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE":    "",
	"DOCKER_TLS_VERIFY":                        "0",
	"DOCKER_CERT_PATH":                         "",
	"DOCKER_API_VERSION":                       "",
	"DOCKER_AUTH_CONFIG":                       "",
	"DOCKER_CONFIG":                            "",
}

// testcontainersPropertiesPaths returns every file that can influence the
// benchmark's Testcontainers configuration. Older Testcontainers releases
// read only the home-directory file, while newer/configuration-aware builds
// may honor TESTCONTAINERS_CONFIG. Checking both paths is fail-closed across
// those behaviors and also prevents a custom path from bypassing the
// benchmark's identity checks.
func testcontainersPropertiesPaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve testcontainers home directory: %w", err)
	}
	defaultPath := filepath.Join(home, ".testcontainers.properties")
	paths := []string{defaultPath}
	if configured := os.Getenv("TESTCONTAINERS_CONFIG"); configured != "" {
		if strings.TrimSpace(configured) != configured {
			return nil, fmt.Errorf("TESTCONTAINERS_CONFIG must not contain surrounding whitespace")
		}
		if configured != defaultPath {
			paths = append(paths, configured)
		}
	}
	return paths, nil
}

func validateTestcontainersConfiguration() error {
	names := make([]string, 0, len(testcontainersBenchmarkOverrides))
	for name := range testcontainersBenchmarkOverrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateTestcontainersEnvironmentOverride(name, os.Getenv(name)); err != nil {
			return err
		}
	}
	paths, err := testcontainersPropertiesPaths()
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := validateTestcontainersProperties(string(data)); err != nil {
				return fmt.Errorf("validate %s: %w", path, err)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("read testcontainers properties %s: %w", path, err)
		}
	}
	return nil
}

func validateTestcontainersEnvironmentOverride(name, value string) error {
	canonical, checked := testcontainersBenchmarkOverrides[name]
	if !checked || value == "" {
		return nil
	}
	if canonical == "" {
		if name == "DOCKER_AUTH_CONFIG" {
			return fmt.Errorf("%s overrides the pinned testcontainers benchmark identity", name)
		}
		return fmt.Errorf("%s=%q overrides the pinned testcontainers benchmark identity", name, value)
	}
	if canonical == "false" && strings.EqualFold(value, canonical) {
		return nil
	}
	if canonical == "10s" || canonical == "1m" {
		want := 10 * time.Second
		if canonical == "1m" {
			want = time.Minute
		}
		if duration, err := time.ParseDuration(value); err == nil && duration == want {
			return nil
		}
	}
	if name == "DOCKER_TLS_VERIFY" && value == canonical {
		return nil
	}
	return fmt.Errorf("%s=%q overrides the pinned testcontainers benchmark identity (want %q)", name, value, canonical)
}

// validateTestcontainersProperties uses the same magiconair/properties parser
// as testcontainers-go. In particular, escaped separators and Unicode
// escapes are decoded before keys are inspected; a hand-written strings.Cut
// parser would miss behavior-affecting properties such as an escaped dot or
// Unicode escape in a recognized key.
func validateTestcontainersProperties(data string) error {
	props, err := properties.LoadString(data)
	if err != nil {
		return fmt.Errorf("parse .testcontainers.properties: %w", err)
	}
	for _, key := range props.Keys() {
		// A backslash-newline or an escaped separator in a key is not a
		// valid Testcontainers setting, but older line-oriented checks
		// could mistake the physical lines for separate records. Reject
		// it rather than relying on a parser-specific interpretation of
		// a malformed key.
		if strings.ContainsAny(key, "\\\r\n:=") {
			return fmt.Errorf(".testcontainers.properties key %q is not a canonical setting", key)
		}
		value, _ := props.Get(key)
		if err := validateTestcontainersProperty(strings.ToLower(key), value); err != nil {
			return err
		}
	}
	return nil
}

func validateTestcontainersProperty(key, value string) error {
	if value == "" {
		return nil
	}
	switch key {
	case "ryuk.disabled", "ryuk.container.privileged", "ryuk.verbose":
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil || parsed {
			return fmt.Errorf(".testcontainers.properties %s=%q overrides the pinned benchmark identity", key, value)
		}
	case "docker.tls.verify":
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed != 0 {
			return fmt.Errorf(".testcontainers.properties %s=%q overrides the pinned benchmark identity", key, value)
		}
	case "ryuk.reconnection.timeout", "ryuk.connection.timeout":
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		want := 10 * time.Second
		if key == "ryuk.connection.timeout" {
			want = time.Minute
		}
		if err != nil || duration != want {
			return fmt.Errorf(".testcontainers.properties %s=%q overrides the pinned benchmark identity", key, value)
		}
	case "docker.host", "docker.cert.path", "hub.image.name.prefix", "session.id", "tc.host", "ryuk.container.image":
		return fmt.Errorf(".testcontainers.properties %s=%q overrides the pinned benchmark identity", key, value)
	}
	return nil
}

func validBenchmarkSessionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}
