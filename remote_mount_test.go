package container

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerBindMountPolicy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "data")
	tests := []struct {
		name       string
		dockerHost string
		wantErr    bool
	}{
		{name: "local", dockerHost: "", wantErr: false},
		{name: "remote", dockerHost: "tcp://10.0.0.5:2375", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(backendEnv, "docker")
			t.Setenv("DOCKER_HOST", tt.dockerHost)
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), withRunner(f),
				WithMounts(Mount{Type: MountBind, Source: source, Target: "/data"}))

			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedCapability) {
					t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
				}
				if !strings.Contains(err.Error(), "remote Docker daemon") {
					t.Errorf("error = %q, want remote Docker explanation", err)
				}
				if len(f.calls) != 0 {
					t.Errorf("remote configuration must fail before backend work: %v", f.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("local bind mount: %v", err)
			}
			if f.callWith("run") == nil {
				t.Error("local bind mount did not issue a run command")
			}
		})
	}
}
