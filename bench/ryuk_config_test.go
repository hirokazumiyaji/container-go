package bench

import (
	"os"
	"path/filepath"
	"testing"
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
		if err := validateTestcontainersEnvironmentOverride(name, "evil/ryuk:latest"); err == nil {
			t.Errorf("%s image override was accepted", name)
		}
	}
	if err := validateTestcontainersEnvironmentOverride("TESTCONTAINERS_RYUK_DISABLED", "false"); err != nil {
		t.Fatalf("explicit false disabled flag should be forceable: %v", err)
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
	} {
		if err := validateTestcontainersProperties(data); err != nil {
			t.Errorf("canonical properties %q rejected: %v", data, err)
		}
	}

	for name, data := range map[string]string{
		"whitespace separator":  "session.id fixed-session\n",
		"continuation":          "session.id=fixed-\\\nsession\n",
		"key continuation":      "session.\\\nid=fixed\n",
		"escaped separator":     `ryuk\.disabled=true` + "\n",
		"escaped colon":         `ryuk\:disabled=true` + "\n",
		"unicode escaped key":   `ryuk\u002edisabled=true` + "\n",
		"unicode escaped value": `ryuk.disabled=tr\u0075e` + "\n",
		"ryuk image property":   "ryuk.container.image=evil/ryuk:latest\n",
		"malformed escape":      "session.id=bad\\uZZZZ\n",
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
