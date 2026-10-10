package container

import (
	"context"
	"github.com/hirokazumiyaji/container-go/internal/cli"
	"os"
	"strings"
	"testing"
)

func TestDetectEngineForDefaultsByOS(t *testing.T) {
	cases := []struct {
		goos, env string
		want      string
		wantErr   bool
	}{
		{"darwin", "", "apple", false},
		{"linux", "", "docker", false},
		{"windows", "", "docker", false},
		{"darwin", "docker", "docker", false},
		{"linux", "docker", "docker", false},
		{"darwin", "apple", "apple", false},
		{"linux", "apple", "", true},
		{"windows", "apple", "", true},
		{"darwin", "podman", "", true},
	}
	for _, tc := range cases {
		eng, err := detectEngineFor(tc.goos, tc.env)
		if tc.wantErr {
			if err == nil {
				t.Errorf("detectEngineFor(%q, %q): want error", tc.goos, tc.env)
			}
			continue
		}
		if err != nil {
			t.Errorf("detectEngineFor(%q, %q): %v", tc.goos, tc.env, err)
			continue
		}
		if eng.name() != tc.want {
			t.Errorf("detectEngineFor(%q, %q) = %q, want %q", tc.goos, tc.env, eng.name(), tc.want)
		}
	}
}

func TestValidateBackendEnvPreservesValidSelections(t *testing.T) {
	for _, value := range []string{"", "apple", "docker"} {
		if err := validateBackendEnv(value); err != nil {
			t.Errorf("validateBackendEnv(%q): %v", value, err)
		}
	}
	if err := validateBackendEnv("podman"); err == nil {
		t.Fatal("validateBackendEnv accepted an invalid backend")
	}
}

func TestRunSelectsBackendFromEnv(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "docker")

	d := &dockerRunner{fakeRunner: newTestRunner()}
	data, err := os.ReadFile("testdata/docker_inspect_v29.json")
	if err != nil {
		t.Fatal(err)
	}
	d.inspectJSON = data

	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(d))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.eng.name() != "docker" {
		t.Errorf("engine = %q, want docker", ctr.eng.name())
	}
}

func TestDefaultRunnerBinaryFollowsEngine(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	applyEngineBinary(cfg)
	if got := cfg.runner.(*cli.ExecRunner).Binary; got != "docker" {
		t.Errorf("Binary = %q, want docker", got)
	}

	// An explicitly configured binary is left alone.
	cfg2 := newConfig()
	cfg2.eng = dockerEngine{}
	cfg2.runner.(*cli.ExecRunner).Binary = "/opt/docker"
	applyEngineBinary(cfg2)
	if got := cfg2.runner.(*cli.ExecRunner).Binary; got != "/opt/docker" {
		t.Errorf("Binary = %q, want /opt/docker preserved", got)
	}
}

func TestRunRejectsInvalidBackendEnv(t *testing.T) {
	t.Setenv("CONTAINERGO_BACKEND", "podman")

	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine", WithName("myctr"), withRunner(f))
	if err == nil {
		t.Fatal("want error for invalid CONTAINERGO_BACKEND")
	}
	if !strings.Contains(err.Error(), "CONTAINERGO_BACKEND") {
		t.Errorf("error = %v, want mention of CONTAINERGO_BACKEND", err)
	}
}
