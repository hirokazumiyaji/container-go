package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunRejectsInvalidPublicOptionsBeforeBackend(t *testing.T) {
	ports := make([]string, maxOptionCount+1)
	for i := range ports {
		ports[i] = "1/tcp"
	}
	mounts := make([]Mount, maxOptionCount+1)
	for i := range mounts {
		mounts[i] = Mount{Type: MountTmpfs, Target: "/tmp"}
	}
	options := make([]Option, maxOptionCount+1)
	for i := range options {
		options[i] = WithReuse()
	}

	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "unknown mount type",
			opts: []Option{WithMounts(Mount{Type: MountType(99), Source: "volume", Target: "/data"})},
		},
		{
			name: "zero memory",
			opts: []Option{WithMemory("0")},
		},
		{
			name: "overflow memory",
			opts: []Option{WithMemory("18446744073709551615P")},
		},
		{
			name: "invalid volume name",
			opts: []Option{WithMounts(Mount{Type: MountVolume, Source: "bad=name", Target: "/data"})},
		},
		{
			name: "unknown pull policy",
			opts: []Option{WithPullPolicy(PullPolicy(99))},
		},
		{
			name: "zero cpus",
			opts: []Option{WithCPUs(0)},
		},
		{
			name: "too many exposed ports",
			opts: []Option{WithExposedPorts(ports...)},
		},
		{
			name: "too many mounts",
			opts: []Option{WithMounts(mounts...)},
		},
		{
			name: "too many options",
			opts: options,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			opts := append([]Option{}, tt.opts...)
			opts = append(opts, WithName("myctr"), withRunner(f), withEngine(appleEngine{}))
			_, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err == nil {
				t.Fatal("Run succeeded, want validation error")
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error = %v, want ErrInvalidOption", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called despite invalid options: %v", f.calls)
			}
		})
	}
}

func TestReuseGroupValidationIsSharedByCreateAndPrune(t *testing.T) {
	tests := []struct {
		name    string
		group   string
		wantErr bool
	}{
		{name: "simple", group: "integration"},
		{name: "oci segment", group: "team/integration"},
		{name: "empty", group: "", wantErr: true},
		{name: "equals", group: "bad=value", wantErr: true},
		{name: "comma", group: "bad,value", wantErr: true},
		{name: "uppercase", group: "Integration", wantErr: true},
		{name: "too long", group: strings.Repeat("a", maxReuseGroupBytes+1), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createRunner := newReuseCreateRunner()
			_, createErr := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithReuse(), WithReuseGroup(tt.group),
				withRunner(createRunner), withEngine(appleEngine{}))
			pruneRunner := &lsRunner{fakeRunner: newTestRunner(), lsJSON: "[]"}
			_, pruneErr := pruneReuseGroupWith(context.Background(), pruneRunner, appleEngine{}, tt.group)

			if (createErr != nil) != tt.wantErr {
				t.Fatalf("create error = %v, want error=%t", createErr, tt.wantErr)
			}
			if (pruneErr != nil) != tt.wantErr {
				t.Fatalf("prune error = %v, want error=%t", pruneErr, tt.wantErr)
			}
			if tt.wantErr {
				if createErr.Error() != pruneErr.Error() {
					t.Errorf("create error %q != prune error %q", createErr, pruneErr)
				}
				var validationErr *ValidationError
				if !errors.As(createErr, &validationErr) || !errors.As(pruneErr, &validationErr) {
					t.Fatalf("errors are not typed: create=%T prune=%T", createErr, pruneErr)
				}
				if len(createRunner.calls) != 0 || len(pruneRunner.calls) != 0 {
					t.Fatalf("backend was called for invalid group: create=%v prune=%v", createRunner.calls, pruneRunner.calls)
				}
			}
		})
	}
}

func TestLogsWithOptionsRejectsNegativeTailBeforeBackend(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{Tail: -1})
	if rc != nil {
		t.Fatalf("reader = %v, want nil", rc)
	}
	if err == nil {
		t.Fatal("LogsWithOptions succeeded, want validation error")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for negative tail: %v", f.calls)
	}
}

func TestLogsWithOptionsZeroTailMeansAll(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{Tail: 0})
	if err != nil {
		t.Fatalf("LogsWithOptions: %v", err)
	}
	_ = rc.Close()
	call := f.callWith("logs")
	if call == nil {
		t.Fatal("no logs call recorded")
	}
	for _, arg := range call {
		if arg == "--tail" {
			t.Fatalf("zero tail was sent as a backend limit: %v", call)
		}
	}
}

func TestPruneReuseGroupValidatesBeforeEngineDetection(t *testing.T) {
	t.Setenv(backendEnv, "not-a-backend")
	_, err := PruneReuseGroup(context.Background(), "bad=value")
	if err == nil {
		t.Fatal("PruneReuseGroup succeeded, want validation error")
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if strings.Contains(err.Error(), backendEnv) {
		t.Fatalf("error = %v, want validation before backend detection", err)
	}
}

func TestUnknownMountNeverBuildsPartialArg(t *testing.T) {
	m := Mount{Type: MountType(99), Source: "volume", Target: "/data"}
	if got := m.arg(); got != "" {
		t.Fatalf("unknown mount arg = %q, want empty", got)
	}
}
