package container

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// validateConfig is the pure core of checkConfig. It must decide the backend
// option contract without spawning a CLI process or normalizing the caller's
// config.
func TestAppleValidateConfigIsPure(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	cfg := appleCapabilityConfig(t, WithPlatform("linux/armel"))
	cfg.runner = runner
	before := *cfg

	if err := (appleEngine{}).validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("validateConfig spawned %d CLI calls", got)
	}
	if !reflect.DeepEqual(before, *cfg) {
		t.Fatalf("validateConfig mutated the config: before %+v, after %+v", before, *cfg)
	}
	if cfg.platform != "linux/armel" {
		t.Fatalf("validateConfig normalized platform to %q before the shell decided", cfg.platform)
	}

	// The shell applies the environment default and normalization that the
	// pure core deliberately leaves out.
	if err := (appleEngine{}).checkConfig(context.Background(), cfg); err != nil {
		t.Fatalf("checkConfig: %v", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("checkConfig spawned %d CLI calls for a state-independent backend", got)
	}
	if cfg.platform != "linux/armel/v6" {
		t.Fatalf("checkConfig platform = %q, want canonical linux/armel/v6", cfg.platform)
	}
}

func TestAppleValidateConfigRejectsWithoutBackendIO(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	cfg := appleCapabilityConfig(t, WithName("a"))
	cfg.runner = runner

	err := (appleEngine{}).validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "2-63") {
		t.Fatalf("validateConfig error = %v, want Apple name contract", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("invalid validateConfig spawned %d CLI calls", got)
	}
}

func TestDockerValidateConfigIsPure(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	cfg := &config{runner: runner, eng: dockerEngine{}}
	before := *cfg

	if err := (dockerEngine{}).validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("validateConfig spawned %d CLI calls", got)
	}
	if !reflect.DeepEqual(before, *cfg) {
		t.Fatalf("validateConfig mutated the config: before %+v, after %+v", before, *cfg)
	}
}

func TestDockerValidateConfigRejectsBeforeRuntimeProbe(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config
		wantErr string
	}{
		{
			name:    "short volume name",
			cfg:     &config{mounts: []Mount{{Type: MountVolume, Source: "x"}}},
			wantErr: "too short",
		},
		{
			name:    "aliases require a user-defined network",
			cfg:     &config{network: dockerNetworkHost, networkExplicit: true, networkAliases: []string{"app"}},
			wantErr: "alias",
		},
		{
			name:    "host network cannot publish ports",
			cfg:     &config{network: dockerNetworkHost, networkExplicit: true, published: []publishSpec{{raw: "127.0.0.1:8080:80"}}},
			wantErr: "host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newCountingRunner(newTestRunner())
			tt.cfg.runner = runner
			tt.cfg.eng = dockerEngine{}

			err := (dockerEngine{}).checkConfig(context.Background(), tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkConfig error = %v, want %q", err, tt.wantErr)
			}
			if got := runner.count(); got != 0 {
				t.Fatalf("validation failure spawned %d runtime probes", got)
			}
		})
	}
}
