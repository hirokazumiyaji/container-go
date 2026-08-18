package container

import (
	"context"
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
