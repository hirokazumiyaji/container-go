package bench

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
	"github.com/magiconair/properties"
)

// testcontainersBenchmarkOverrides lists every Testcontainers/Ryuk setting
// that changes the reaper or Docker connection semantics used by this
// benchmark. Empty values are harmless; non-empty values must equal the
// canonical value below. The reaper image entries name the pinned reference
// the benchmark forces before the session starts, so a run cannot fall back
// to a mutable or substituted image. DOCKER_HOST and DOCKER_CONTEXT are
// deliberately not listed here: the Docker provenance capture records and
// validates those effective selections instead. TESTCONTAINERS_CONFIG is
// handled separately by testcontainersPropertiesPaths so its effective file
// is parsed too.
var testcontainersBenchmarkOverrides = map[string]string{
	"TESTCONTAINERS_RYUK_DISABLED":             "false",
	"TESTCONTAINERS_RYUK_IMAGE":                ibench.PinnedTestcontainersRyukImage,
	"TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED": "false",
	"RYUK_IMAGE":                               ibench.PinnedTestcontainersRyukImage,
	"RYUK_VERBOSE":                             "false",
	"TESTCONTAINERS_RYUK_VERBOSE":              "false",
	"RYUK_RECONNECTION_TIMEOUT":                "10s",
	"TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT": "10s",
	"RYUK_CONNECTION_TIMEOUT":                  "1m",
	"TESTCONTAINERS_RYUK_CONNECTION_TIMEOUT":   "1m",
	"TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX":     "",
	"TESTCONTAINERS_SESSION_ID":                "",
	"RYUK_PORT":                                "",
	"TESTCONTAINERS_RYUK_PORT":                 "",
	"TESTCONTAINERS_ALWAYS_PULL_IMAGE":         "false",
	"TESTCONTAINERS_CHECKS_DISABLE":            "false",
	"TESTCONTAINERS_DOCKER_SOCKET_PATH":        "",
	"TESTCONTAINERS_HOST_OVERRIDE":             "",
	"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE":    "",
	"DOCKER_TLS_VERIFY":                        "0",
	"DOCKER_CERT_PATH":                         "",
	"DOCKER_API_VERSION":                       "",
	"DOCKER_AUTH_CONFIG":                       "",
	"DOCKER_CONFIG":                            "",
}

// testcontainersRyukImageEnvironment names the variables that can select the
// reaper image. The benchmark forces both to the pinned reference.
var testcontainersRyukImageEnvironment = []string{"RYUK_IMAGE", "TESTCONTAINERS_RYUK_IMAGE"}

// canonicalTestcontainersEnvironment is the environment the benchmark forces
// before a Testcontainers session starts, so an inherited value or a
// property that testcontainers decodes differently cannot change the session
// that is measured. Every entry has to satisfy the override policy.
func canonicalTestcontainersEnvironment() map[string]string {
	forced := map[string]string{
		"TESTCONTAINERS_RYUK_DISABLED":             "false",
		"TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED": "false",
		"RYUK_VERBOSE":                     "false",
		"TESTCONTAINERS_RYUK_VERBOSE":      "false",
		"TESTCONTAINERS_ALWAYS_PULL_IMAGE": "false",
		"TESTCONTAINERS_CHECKS_DISABLE":    "false",
	}
	for _, name := range testcontainersRyukImageEnvironment {
		forced[name] = ibench.PinnedTestcontainersRyukImage
	}
	return forced
}

// testcontainersRyukImageReferences are the image references a reaper
// container is allowed to carry. Testcontainers 0.44.0 requests the mutable
// tag it ships with; releases that honor the image environment use the
// pinned reference. The content behind either reference is verified, so the
// reference itself only has to be one the benchmark prepared.
var testcontainersRyukImageReferences = []string{ibench.TestcontainersRyukTag, ibench.PinnedTestcontainersRyukImage}

// testcontainersPropertiesPaths returns every file that can influence the
// benchmark's Testcontainers configuration. Older Testcontainers releases
// read only the home-directory file, while newer/configuration-aware builds
// may honor TESTCONTAINERS_CONFIG. Checking both paths is fail-closed across
// those behaviors and also prevents a custom path from bypassing the
// benchmark's identity checks.
//
// Paths are returned as resolved identities, deduplicated, so one file is
// validated once even when TESTCONTAINERS_CONFIG names it through a symlink
// or a redundant path. A relative TESTCONTAINERS_CONFIG is refused: it would
// resolve against the process working directory, which is neither comparable
// with the default path nor stable for the recorded run.
func testcontainersPropertiesPaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve testcontainers home directory: %w", err)
	}
	configured := os.Getenv("TESTCONTAINERS_CONFIG")
	if strings.TrimSpace(configured) != configured {
		return nil, fmt.Errorf("TESTCONTAINERS_CONFIG must not contain surrounding whitespace")
	}
	if configured != "" && !filepath.IsAbs(configured) {
		return nil, fmt.Errorf("TESTCONTAINERS_CONFIG must be an absolute path, got %q", configured)
	}
	paths := make([]string, 0, 2)
	resolved := make(map[string]struct{}, 2)
	for _, path := range []string{filepath.Join(home, ".testcontainers.properties"), configured} {
		if path == "" {
			continue
		}
		canonical, err := canonicalTestcontainersConfigPath(path)
		if err != nil {
			return nil, err
		}
		if _, duplicate := resolved[canonical]; duplicate {
			continue
		}
		resolved[canonical] = struct{}{}
		paths = append(paths, canonical)
	}
	return paths, nil
}

// canonicalTestcontainersConfigPath resolves one configuration path to the
// identity the daemon will read. A path that does not exist yet is
// canonicalized without link resolution: there is nothing to follow, and the
// file can still be created before the reaper starts.
func canonicalTestcontainersConfigPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve testcontainers configuration path %q: %w", path, err)
	}
	linked, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve testcontainers configuration path %q: %w", path, err)
		}
		return absolute, nil
	}
	return linked, nil
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

// validateTestcontainersDockerEndpoint limits the canonical DOCKER_HOST
// forms to transports that testcontainers-go can consume without additional
// context-specific material. The Docker CLI context remains recorded and is
// set alongside DOCKER_HOST so both clients select the same daemon.
func validateTestcontainersDockerEndpoint(endpoint string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" {
		return fmt.Errorf("unsupported Docker endpoint %q", endpoint)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("unsupported Docker endpoint %q", endpoint)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "unix", "npipe":
		if parsed.Path == "" {
			return fmt.Errorf("unsupported Docker endpoint %q", endpoint)
		}
	case "tcp":
		if parsed.Host == "" {
			return fmt.Errorf("unsupported Docker endpoint %q", endpoint)
		}
	default:
		return fmt.Errorf("unsupported Docker endpoint scheme %q", parsed.Scheme)
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
	if value == canonical {
		return nil
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
	return fmt.Errorf("%s=%q overrides the pinned testcontainers benchmark identity (want %q)", name, value, canonical)
}

// validateCanonicalRyukImageEnvironment proves the reaper image the process
// will hand to Testcontainers is the pinned reference. The preflight forces
// the values, and this check refuses to record a result if a forced value
// did not survive.
func validateCanonicalRyukImageEnvironment(getenv func(string) string) error {
	for _, name := range testcontainersRyukImageEnvironment {
		if value := getenv(name); value != ibench.PinnedTestcontainersRyukImage {
			return fmt.Errorf("%s=%q is not the pinned Ryuk image %q", name, value, ibench.PinnedTestcontainersRyukImage)
		}
	}
	return nil
}

// validRyukImageReference reports whether a reaper container may carry this
// image reference. The content is verified separately, by image ID and
// resolved digest, so the mutable tag is accepted only because the harness
// maps it to the pinned image first.
func validRyukImageReference(reference string) bool {
	return slices.Contains(testcontainersRyukImageReferences, reference)
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
	case "ryuk.disabled", "ryuk.container.privileged", "ryuk.verbose", "always.pull.image", "checks.disable":
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
	case "docker.host", "docker.cert.path", "docker.socket.path", "hub.image.name.prefix", "session.id", "tc.host", "ryuk.container.image":
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
