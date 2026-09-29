package bench

import (
	"os"
	"path/filepath"
	"testing"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
)

func TestValidateTestcontainersEnvironmentOverrides(t *testing.T) {
	for _, name := range []string{
		"TESTCONTAINERS_RYUK_DISABLED",
		"TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX",
		"TESTCONTAINERS_SESSION_ID",
	} {
		if err := validateTestcontainersEnvironmentOverride(name, ""); err != nil {
			t.Errorf("empty %s: %v", name, err)
		}
	}
	if err := validateTestcontainersEnvironmentOverride("TESTCONTAINERS_RYUK_DISABLED", "true"); err == nil {
		t.Fatal("disabled Ryuk override was accepted")
	}
	if err := validateTestcontainersEnvironmentOverride("TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX", "mirror.example/"); err == nil {
		t.Fatal("image prefix override was accepted")
	}
	if err := validateTestcontainersEnvironmentOverride("TESTCONTAINERS_SESSION_ID", "fixed-session"); err == nil {
		t.Fatal("session override was accepted")
	}
	for _, name := range []string{"TESTCONTAINERS_RYUK_IMAGE", "RYUK_IMAGE"} {
		if err := validateTestcontainersEnvironmentOverride(name, ""); err != nil {
			t.Errorf("empty %s: %v", name, err)
		}
		if err := validateTestcontainersEnvironmentOverride(name, ibench.PinnedTestcontainersRyukImage); err != nil {
			t.Errorf("%s pinned image: %v", name, err)
		}
		if err := validateTestcontainersEnvironmentOverride(name, "evil/ryuk:latest"); err == nil {
			t.Errorf("%s image override was accepted", name)
		}
	}
	if err := validateTestcontainersEnvironmentOverride("TESTCONTAINERS_RYUK_DISABLED", "false"); err != nil {
		t.Fatalf("explicit false disabled flag should be forceable: %v", err)
	}
}

// Every setting the preflight must refuse has to appear in the policy table.
// A name that is missing from the table is silently accepted, so the table
// itself is part of the contract.
func TestBenchmarkOverridesCoverBehaviorChangingSettings(t *testing.T) {
	for _, name := range []string{
		"TESTCONTAINERS_RYUK_DISABLED",
		"TESTCONTAINERS_RYUK_IMAGE",
		"TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED",
		"RYUK_IMAGE",
		"RYUK_VERBOSE",
		"TESTCONTAINERS_RYUK_VERBOSE",
		"RYUK_RECONNECTION_TIMEOUT",
		"TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT",
		"RYUK_CONNECTION_TIMEOUT",
		"TESTCONTAINERS_RYUK_CONNECTION_TIMEOUT",
		"RYUK_PORT",
		"TESTCONTAINERS_RYUK_PORT",
		"TESTCONTAINERS_ALWAYS_PULL_IMAGE",
		"TESTCONTAINERS_CHECKS_DISABLE",
		"TESTCONTAINERS_DOCKER_SOCKET_PATH",
		"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE",
		"TESTCONTAINERS_HOST_OVERRIDE",
		"TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX",
		"TESTCONTAINERS_SESSION_ID",
		"DOCKER_TLS_VERIFY",
		"DOCKER_CERT_PATH",
		"DOCKER_API_VERSION",
		"DOCKER_AUTH_CONFIG",
		"DOCKER_CONFIG",
	} {
		if _, ok := testcontainersBenchmarkOverrides[name]; !ok {
			t.Errorf("%s is not covered by the preflight policy", name)
		}
	}
}

func TestValidateTestcontainersEnvironmentOverridesRejectBehaviorChanges(t *testing.T) {
	for _, override := range []struct{ name, value string }{
		{"TESTCONTAINERS_ALWAYS_PULL_IMAGE", "true"},
		// A case variant still parses as true in Testcontainers' own
		// boolean parser, so it changes behavior and must be refused too.
		{"TESTCONTAINERS_ALWAYS_PULL_IMAGE", "TRUE"},
		{"TESTCONTAINERS_CHECKS_DISABLE", "true"},
		{"TESTCONTAINERS_CHECKS_DISABLE", "True"},
		{"TESTCONTAINERS_DOCKER_SOCKET_PATH", "/tmp/other-docker.sock"},
		{"TESTCONTAINERS_RYUK_PORT", "8080"},
		{"RYUK_PORT", "8080"},
		{"DOCKER_TLS_VERIFY", "1"},
		{"DOCKER_CERT_PATH", "/tmp/certs"},
		{"DOCKER_API_VERSION", "1.41"},
		{"DOCKER_CONFIG", "/tmp/docker-config"},
		{"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/var/run/docker.sock"},
		{"TESTCONTAINERS_HOST_OVERRIDE", "tcp://127.0.0.1:2375"},
		{"RYUK_RECONNECTION_TIMEOUT", "30s"},
		{"TESTCONTAINERS_RYUK_CONNECTION_TIMEOUT", "5m"},
		{"RYUK_VERBOSE", "true"},
		{"TESTCONTAINERS_RYUK_CONTAINER_PRIVILEGED", "true"},
	} {
		if err := validateTestcontainersEnvironmentOverride(override.name, override.value); err == nil {
			t.Errorf("%s=%q was accepted", override.name, override.value)
		}
	}
	for _, override := range []struct{ name, value string }{
		{"TESTCONTAINERS_ALWAYS_PULL_IMAGE", "false"},
		{"TESTCONTAINERS_CHECKS_DISABLE", "false"},
		{"RYUK_PORT", ""},
		{"DOCKER_TLS_VERIFY", "0"},
		// Equivalent spellings of the canonical durations are still the
		// canonical duration.
		{"RYUK_RECONNECTION_TIMEOUT", "10000ms"},
		{"RYUK_CONNECTION_TIMEOUT", "60s"},
	} {
		if err := validateTestcontainersEnvironmentOverride(override.name, override.value); err != nil {
			t.Errorf("%s=%q rejected: %v", override.name, override.value, err)
		}
	}
}

func TestValidateTestcontainersPropertiesRejectsIdentityOverrides(t *testing.T) {
	data := "ryuk.disabled=true\nhub.image.name.prefix=mirror.example/\nsession.id=fixed-session\n"
	if err := validateTestcontainersProperties(data); err == nil {
		t.Fatal("properties identity overrides were accepted")
	}
	if err := validateTestcontainersProperties("ryuk.disabled=false\n"); err != nil {
		t.Fatalf("false disabled property should be accepted: %v", err)
	}
}

func TestValidateTestcontainersPropertiesUsesJavaGrammar(t *testing.T) {
	for _, data := range []string{
		"ryuk.disabled = false\n",
		`ryuk\.disabled=false` + "\n",
		"ryuk.container.privileged: false\n",
		"ryuk.verbose false\n",
		"ryuk.reconnection.timeout 10s\n",
		"ryuk.connection.timeout = 1m\n",
		"session.id =\n",
		"ryuk.container.image=\n",
		"always.pull.image=false\n",
		"checks.disable=false\n",
		"docker.socket.path=\n",
	} {
		if err := validateTestcontainersProperties(data); err != nil {
			t.Errorf("canonical properties %q rejected: %v", data, err)
		}
	}

	for name, data := range map[string]string{
		"whitespace separator":      "session.id fixed-session\n",
		"continuation":              "session.id=fixed-\\\nsession\n",
		"key continuation":          "session.\\\nid=fixed\n",
		"escaped separator":         `ryuk\.disabled=true` + "\n",
		"escaped colon":             `ryuk\:disabled=true` + "\n",
		"unicode escaped key":       `ryuk\u002edisabled=true` + "\n",
		"unicode escaped value":     `ryuk.disabled=tr\u0075e` + "\n",
		"ryuk image property":       "ryuk.container.image=evil/ryuk:latest\n",
		"malformed escape":          "session.id=bad\\uZZZZ\n",
		"always pull":               "always.pull.image=true\n",
		"always pull case variant":  "always.pull.image=TRUE\n",
		"checks disable":            "checks.disable=true\n",
		"docker socket path":        "docker.socket.path=/tmp/other-docker.sock\n",
		"escaped pull key":          `always\.pull.image=true` + "\n",
		"unicode escaped pull flag": `always.pull.image=tr\u0075e` + "\n",
	} {
		if err := validateTestcontainersProperties(data); err == nil {
			t.Errorf("%s override was accepted", name)
		}
	}
}

func TestTestcontainersPropertiesPathsIncludeConfigOverride(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "testcontainers.properties")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TESTCONTAINERS_CONFIG", custom)

	paths, err := testcontainersPropertiesPaths()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range paths {
		if path == custom {
			found = true
		}
	}
	if !found {
		t.Fatalf("configuration paths = %v, want custom path %q", paths, custom)
	}

	if err := os.WriteFile(custom, []byte("session.id=fixed-session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersProperties(string(data)); err == nil {
		t.Fatal("custom Testcontainers configuration bypassed identity validation")
	}
	if err := os.WriteFile(custom, []byte("ryuk.disabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersProperties(string(data)); err != nil {
		t.Fatalf("canonical custom Testcontainers configuration was rejected: %v", err)
	}

	t.Setenv("TESTCONTAINERS_CONFIG", " "+custom)
	if _, err := testcontainersPropertiesPaths(); err == nil {
		t.Fatal("whitespace-padded configuration path was accepted")
	}
}

// A configuration file must be validated once, by resolved identity. A
// symlink, a redundant path segment, or a repeated entry must not turn one
// file into two inputs, and a relative path must not resolve against the
// process working directory instead of the file Testcontainers reads.
func TestTestcontainersPropertiesPathsCanonicalizeAndDedupe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	defaultPath := filepath.Join(home, ".testcontainers.properties")
	if err := os.WriteFile(defaultPath, []byte("ryuk.disabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedDefault, err := filepath.EvalSymlinks(defaultPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("TESTCONTAINERS_CONFIG", defaultPath)
	paths, err := testcontainersPropertiesPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != resolvedDefault {
		t.Fatalf("default configuration paths = %v, want [%s]", paths, resolvedDefault)
	}

	link := filepath.Join(t.TempDir(), "link.properties")
	if err := os.Symlink(defaultPath, link); err != nil {
		t.Fatal(err)
	}
	for name, configured := range map[string]string{
		"symlinked default":  link,
		"redundant segment":  filepath.Join(home, ".", ".testcontainers.properties"),
		"resolved parent":    filepath.Join(filepath.Dir(home), filepath.Base(home), ".testcontainers.properties"),
		"repeated separator": home + "//.testcontainers.properties",
	} {
		t.Setenv("TESTCONTAINERS_CONFIG", configured)
		paths, err := testcontainersPropertiesPaths()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(paths) != 1 || paths[0] != resolvedDefault {
			t.Errorf("%s: configuration paths = %v, want [%s]", name, paths, resolvedDefault)
		}
	}

	for name, configured := range map[string]string{
		"relative path": "testcontainers.properties",
		"dot relative":  "./testcontainers.properties",
		"parent escape": "../testcontainers.properties",
		"home shortcut": "~/.testcontainers.properties",
	} {
		t.Setenv("TESTCONTAINERS_CONFIG", configured)
		if paths, err := testcontainersPropertiesPaths(); err == nil {
			t.Errorf("%s: configuration paths = %v, want a rejection", name, paths)
		}
	}

	// An absolute path that does not exist yet is still an input the
	// benchmark has to consider: a file can appear before the reaper starts.
	missing := filepath.Join(t.TempDir(), "absent.properties")
	t.Setenv("TESTCONTAINERS_CONFIG", missing)
	paths, err = testcontainersPropertiesPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != resolvedDefault {
		t.Fatalf("configuration paths = %v, want the default and the missing path", paths)
	}
	if filepath.Base(paths[1]) != "absent.properties" {
		t.Fatalf("configuration paths = %v, want the missing path second", paths)
	}
}

// The values the preflight forces are re-validated before the session starts,
// so they must satisfy the same policy as the inherited environment.
func TestCanonicalTestcontainersEnvironmentSatisfiesPolicy(t *testing.T) {
	forced := canonicalTestcontainersEnvironment()
	if len(forced) == 0 {
		t.Fatal("the preflight forces no environment")
	}
	for name, value := range forced {
		if err := validateTestcontainersEnvironmentOverride(name, value); err != nil {
			t.Errorf("forced %s=%q is rejected by the preflight policy: %v", name, value, err)
		}
	}
	if err := validateCanonicalRyukImageEnvironment(func(name string) string { return forced[name] }); err != nil {
		t.Errorf("forced environment does not pin the Ryuk image: %v", err)
	}
}

// The reaper image is supplied through the environment, so the preflight has
// to prove the effective value is the pinned reference rather than trusting
// that the forcing code ran.
func TestValidateCanonicalRyukImageEnvironment(t *testing.T) {
	environment := map[string]string{}
	getenv := func(name string) string { return environment[name] }
	for _, name := range testcontainersRyukImageEnvironment {
		environment[name] = ibench.PinnedTestcontainersRyukImage
	}
	if err := validateCanonicalRyukImageEnvironment(getenv); err != nil {
		t.Fatalf("pinned Ryuk image environment rejected: %v", err)
	}

	environment[testcontainersRyukImageEnvironment[0]] = ""
	if err := validateCanonicalRyukImageEnvironment(getenv); err == nil {
		t.Fatal("unpinned Ryuk image environment was accepted")
	}
	environment[testcontainersRyukImageEnvironment[0]] = "testcontainers/ryuk:0.14.0"
	if err := validateCanonicalRyukImageEnvironment(getenv); err == nil {
		t.Fatal("mutable Ryuk image environment was accepted")
	}
	environment[testcontainersRyukImageEnvironment[0]] = "evil/ryuk@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := validateCanonicalRyukImageEnvironment(getenv); err == nil {
		t.Fatal("unrelated Ryuk image environment was accepted")
	}
}

// The reaper may be started from the mutable tag the dependency requests or
// from the pinned reference, depending on the version. Either is acceptable
// only because its content is verified; any other reference is not.
func TestValidRyukImageReference(t *testing.T) {
	for _, reference := range testcontainersRyukImageReferences {
		if !validRyukImageReference(reference) {
			t.Errorf("%q was rejected", reference)
		}
	}
	for _, reference := range []string{
		"",
		"testcontainers/ryuk",
		"testcontainers/ryuk:0.13.0",
		"testcontainers/ryuk@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"evil/ryuk:0.14.0",
		"ryuk:0.14.0",
	} {
		if validRyukImageReference(reference) {
			t.Errorf("unpinned reference %q was accepted", reference)
		}
	}
}

func TestValidateTestcontainersDockerEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"unix:///var/run/docker.sock",
		"tcp://127.0.0.1:2375",
		"npipe:////./pipe/docker_engine",
	} {
		if err := validateTestcontainersDockerEndpoint(endpoint); err != nil {
			t.Errorf("canonical endpoint %q rejected: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"ssh://user@example.invalid/run/docker.sock",
		"http://127.0.0.1:2375",
		"tcp://",
		"unix://",
	} {
		if err := validateTestcontainersDockerEndpoint(endpoint); err == nil {
			t.Errorf("unsupported endpoint %q was accepted", endpoint)
		}
	}
}

func TestValidateTestcontainersConfigurationRejectsEffectiveFixedIdentity(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "testcontainers.properties")
	for name := range testcontainersBenchmarkOverrides {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TESTCONTAINERS_CONFIG", custom)
	if err := os.WriteFile(custom, []byte("session.id=fixed-session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("fixed session in effective TESTCONTAINERS_CONFIG was accepted")
	}
	if err := os.WriteFile(custom, []byte("ryuk.container.image=evil/ryuk:latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("reaper image in effective TESTCONTAINERS_CONFIG was accepted")
	}
	t.Setenv("TESTCONTAINERS_RYUK_IMAGE", "evil/ryuk:latest")
	if err := os.WriteFile(custom, []byte("ryuk.disabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("reaper image environment override was accepted")
	}

	// The always-pull switch changes what a scenario measures, so it is
	// rejected whether it arrives through the environment or a file.
	t.Setenv("TESTCONTAINERS_RYUK_IMAGE", "")
	if err := os.WriteFile(custom, []byte("always.pull.image=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("always-pull in effective TESTCONTAINERS_CONFIG was accepted")
	}
	if err := os.WriteFile(custom, []byte("checks.disable=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("disabled checks in effective TESTCONTAINERS_CONFIG was accepted")
	}
	t.Setenv("TESTCONTAINERS_CHECKS_DISABLE", "true")
	if err := os.WriteFile(custom, []byte("ryuk.disabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("disabled checks environment override was accepted")
	}
	t.Setenv("TESTCONTAINERS_CHECKS_DISABLE", "")

	// Deduplication must not drop the default file: it is the one
	// Testcontainers reads, and a symlinked TESTCONTAINERS_CONFIG names the
	// same file again.
	link := filepath.Join(t.TempDir(), "link.properties")
	defaultPath := filepath.Join(home, ".testcontainers.properties")
	if err := os.WriteFile(defaultPath, []byte("docker.socket.path=/tmp/other-docker.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(defaultPath, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESTCONTAINERS_CONFIG", link)
	if err := validateTestcontainersConfiguration(); err == nil {
		t.Fatal("socket override in the default configuration was accepted through a symlinked TESTCONTAINERS_CONFIG")
	}
}

func TestValidBenchmarkSessionID(t *testing.T) {
	if !validBenchmarkSessionID("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789") {
		t.Fatal("valid session ID rejected")
	}
	if !validBenchmarkSessionID("fallback-uuid-1234") {
		t.Fatal("valid fallback session ID rejected")
	}
	if validBenchmarkSessionID("bad session") {
		t.Fatal("invalid session ID accepted")
	}
	if validBenchmarkSessionID("bad:session") {
		t.Fatal("session ID with a container-name separator was accepted")
	}
}
