package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRunRejectsInvalidPublicOptionsBeforeBackend(t *testing.T) {
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

func TestPreviouslyValidLargeCollectionsRemainAccepted(t *testing.T) {
	const n = 129 // just above the removed universal cap

	env := make(map[string]string, n)
	labels := make(map[string]string, n)
	mounts := make([]Mount, 0, n)
	ports := make([]string, 0, n)
	files := make([]File, 0, n)
	cmd := make([]string, 0, n)
	options := make([]Option, 0, n)
	for i := 0; i < n; i++ {
		env[fmt.Sprintf("VAR_%03d", i)] = "value"
		labels[fmt.Sprintf("label-%03d", i)] = "value"
		mounts = append(mounts, Mount{Type: MountTmpfs, Target: fmt.Sprintf("/mount-%03d", i)})
		ports = append(ports, "1/tcp")
		files = append(files, File{
			HostPath:      "testdata/docker_inspect_v29.json",
			ContainerPath: fmt.Sprintf("/file-%03d", i),
		})
		cmd = append(cmd, fmt.Sprintf("arg-%03d", i))
		options = append(options, WithCmd("ok"))
	}

	tests := []struct {
		name string
		opts []Option
	}{
		{name: "commands", opts: []Option{WithCmd(cmd...)}},
		{name: "environment", opts: []Option{WithEnv(env)}},
		{name: "labels", opts: []Option{WithLabels(labels)}},
		{name: "mounts", opts: []Option{WithMounts(mounts...)}},
		{name: "ports", opts: []Option{WithExposedPorts(ports...)}},
		{name: "files", opts: []Option{WithFiles(files...)}},
		{name: "options", opts: options},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			opts := append([]Option{}, tt.opts...)
			opts = append(opts, WithName("myctr"), withRunner(f), withEngine(appleEngine{}))
			if _, err := Run(context.Background(), "redis:7-alpine", opts...); err != nil {
				t.Fatalf("Run rejected previously valid %s input: %v", tt.name, err)
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

func TestExecRejectsNilOptionBeforeBackend(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	code, output, err := ctr.Exec(context.Background(), []string{"true"}, nil)
	if code != 0 || output != nil {
		t.Fatalf("Exec result = (%d, %v), want zero values", code, output)
	}
	if err == nil {
		t.Fatal("Exec accepted a nil option")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for a nil Exec option: %v", f.calls)
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
