package bench

import "testing"

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
}
