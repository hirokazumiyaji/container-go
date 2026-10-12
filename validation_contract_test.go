package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunValidatesImageBeforeApplyingOptions(t *testing.T) {
	called := false
	optionErr := errors.New("option must not run")
	_, err := Run(context.Background(), "-invalid", Option(func(*config) error {
		called = true
		return optionErr
	}))
	if called {
		t.Fatal("invalid image applied an option")
	}
	if err == nil || errors.Is(err, optionErr) {
		t.Fatalf("error = %v, want image validation error", err)
	}
	assertImageValidationError(t, err, "-invalid")
}

func TestRunRejectsNilOptionBeforeBackend(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine", nil, withRunner(f), withEngine(appleEngine{}))
	assertPublicValidationError(t, err, "Run", "Run", 0, "option 0 is nil")
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for a nil option: %v", f.calls)
	}
}

func TestConfigValidateChecksCrossOptionReuseInvariant(t *testing.T) {
	cfg := newConfig()
	if err := (WithReuse())(cfg); err != nil {
		t.Fatal(err)
	}
	err := cfg.validate()
	assertPublicValidationError(t, err, "WithReuse", "WithReuse", nil, "WithReuse requires WithName")
}

func TestWithFilesRejectsUnusableHostPathBeforeBackend(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithFiles(File{HostPath: missing, ContainerPath: "/tmp/input"}),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("missing host path was accepted")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if validationErr.Option != "WithFiles" || validationErr.Field != "hostPath" || validationErr.Value != missing {
		t.Fatalf("validation metadata = %#v, want WithFiles/hostPath/missing", validationErr)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for an unusable host path: %v", f.calls)
	}
}

func TestWithFilesRejectsEmptyHostPathBeforeBackend(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithFiles(File{HostPath: "", ContainerPath: "/tmp/input"}),
		withRunner(f), withEngine(appleEngine{}))
	assertPublicValidationError(t, err, "WithFiles", "hostPath", "", "host path must not be empty")
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for an empty host path: %v", f.calls)
	}
}

func TestWithFilesResolvesHostPathToAbsolute(t *testing.T) {
	cfg := newConfig()
	if err := (WithFiles(File{HostPath: "testdata/docker_inspect_v29.json", ContainerPath: "/tmp/input"}))(cfg); err != nil {
		t.Fatalf("WithFiles: %v", err)
	}
	if got := cfg.files[0].HostPath; !filepath.IsAbs(got) {
		t.Fatalf("stored host path = %q, want absolute path", got)
	}
}

func TestWithFilesReportsContainerPathField(t *testing.T) {
	const containerPath = "relative/input"
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithFiles(File{HostPath: "testdata/docker_inspect_v29.json", ContainerPath: containerPath}),
		withRunner(f), withEngine(appleEngine{}))
	assertPublicValidationError(t, err, "WithFiles", "containerPath", containerPath,
		fmt.Sprintf("container path %q must be absolute", containerPath))
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for an invalid container path: %v", f.calls)
	}
}

func TestCopyFileFromContainerUsesPOSIXPathNormalization(t *testing.T) {
	paths := []string{
		"/",
		"//",
		"/.",
		"/tmp/..",
		"/tmp/../",
		"/foo/",
		"/foo//",
	}
	for _, containerPath := range paths {
		t.Run(containerPath, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil

			rc, err := ctr.CopyFileFromContainer(context.Background(), containerPath)
			if rc != nil {
				_ = rc.Close()
			}
			want := fmt.Sprintf("copy file from container %q: cannot copy directory or root as a single file", containerPath)
			assertPublicValidationError(t, err, "CopyFileFromContainer", "containerPath", containerPath, want)
			if len(f.calls) != 0 {
				t.Fatalf("backend was called for root-equivalent path %q: %v", containerPath, f.calls)
			}
		})
	}
}

func TestDockerVolumeGrammarIsCompleteAndBackendNeutral(t *testing.T) {
	valid := []string{"ab", "a1", "a_b", "a.b", "a-b", "1a", strings.Repeat("a", maxVolumeNameBytes)}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			cfg := &config{mounts: []Mount{{Type: MountVolume, Source: name, Target: "/data"}}}
			if err := (dockerEngine{}).checkConfig(context.Background(), cfg); err != nil {
				t.Fatalf("Docker rejected valid volume name %q: %v", name, err)
			}
		})
	}

	invalid := []string{"", "a", "-a", "_a", "a/b", "a b", "a=b", strings.Repeat("a", maxVolumeNameBytes+1)}
	for _, name := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			cfg := &config{mounts: []Mount{{Type: MountVolume, Source: name, Target: "/data"}}}
			if err := (dockerEngine{}).checkConfig(context.Background(), cfg); !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Docker accepted invalid volume name %q: %v", name, err)
			}
		})
	}

	appleName := "x"
	appleCfg := newConfig()
	if err := (WithMounts(Mount{Type: MountVolume, Source: appleName, Target: "/data"}))(appleCfg); err != nil {
		t.Fatalf("shared mount validation rejected Apple volume name %q: %v", appleName, err)
	}
	if err := (appleEngine{}).checkConfig(context.Background(), appleCfg); err != nil {
		t.Fatalf("Apple rejected one-character volume name: %v", err)
	}
}

func TestDockerVolumeGrammarRunsBeforeBackend(t *testing.T) {
	for _, name := range []string{"a", "-a", "a/b", "a b", strings.Repeat("a", maxVolumeNameBytes+1)} {
		t.Run(name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"),
				WithMounts(Mount{Type: MountVolume, Source: name, Target: "/data"}),
				withRunner(f), withEngine(dockerEngine{}))
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Run error = %v, want validation error", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called before Docker volume validation: %v", f.calls)
			}
		})
	}
}

// absHostBindSource builds a host-absolute bind source of exactly n bytes.
// filepath.IsAbs("/") is false on Windows, so POSIX root spellings cannot
// exercise bind-source length boundaries there.
func absHostBindSource(n int) string {
	prefix := "/"
	if runtime.GOOS == "windows" {
		prefix = `C:\`
	}
	if n < len(prefix) {
		return prefix[:n]
	}
	return prefix + strings.Repeat("a", n-len(prefix))
}

func TestMountValidationBoundaries(t *testing.T) {
	hostAtLimit := absHostBindSource(maxMountPathBytes)
	hostOverLimit := absHostBindSource(maxMountPathBytes + 1)
	hostShort := absHostBindSource(8)
	tests := []struct {
		name  string
		mount Mount
		valid bool
	}{
		{
			name:  "volume name at limit",
			mount: Mount{Type: MountVolume, Source: strings.Repeat("a", maxVolumeNameBytes), Target: "/data"},
			valid: true,
		},
		{
			name:  "volume name over limit",
			mount: Mount{Type: MountVolume, Source: strings.Repeat("a", maxVolumeNameBytes+1), Target: "/data"},
		},
		{
			name:  "bind source at limit",
			mount: Mount{Type: MountBind, Source: hostAtLimit, Target: "/data"},
			valid: true,
		},
		{
			name:  "bind source over limit",
			mount: Mount{Type: MountBind, Source: hostOverLimit, Target: "/data"},
		},
		{
			name:  "target at limit",
			mount: Mount{Type: MountBind, Source: hostShort, Target: "/" + strings.Repeat("a", maxMountPathBytes-1)},
			valid: true,
		},
		{
			name:  "target over limit",
			mount: Mount{Type: MountBind, Source: hostShort, Target: "/" + strings.Repeat("a", maxMountPathBytes)},
		},
		{
			name:  "invalid UTF-8 source",
			mount: Mount{Type: MountBind, Source: hostShort + string([]byte{0xff}), Target: "/data"},
		},
		{
			name:  "invalid UTF-8 target",
			mount: Mount{Type: MountBind, Source: hostShort, Target: "/data/" + string([]byte{0xff})},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithMounts(tt.mount), withRunner(f), withEngine(appleEngine{}))
			if tt.valid {
				if err != nil {
					t.Fatalf("Run rejected boundary-valid mount: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Run error = %v, want validation error", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called for invalid mount: %v", f.calls)
			}
		})
	}
}

func TestMemoryValidationBoundaries(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithMemory("1P"), withRunner(f), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("1P should be accepted: %v", err)
	}
	if len(f.calls) == 0 {
		t.Fatal("valid memory option did not reach backend")
	}

	overflow := strings.Repeat("9", 100)
	f = newTestRunner()
	_, err = Run(context.Background(), "redis:7-alpine",
		WithMemory(overflow), withRunner(f), withEngine(appleEngine{}))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("overflow memory error = %v, want validation error", err)
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("error = %v, want overflow diagnostic", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for overflowing memory: %v", f.calls)
	}
}

func TestCommonRunArgsIncludesReachableMountArgument(t *testing.T) {
	cfg := newConfig()
	m := Mount{Type: MountVolume, Source: "ab", Target: "/data", ReadOnly: true}
	if err := (WithMounts(m))(cfg); err != nil {
		t.Fatal(err)
	}
	args := cfg.commonRunArgs("redis:7-alpine", "", nil)
	want := "--mount type=volume,source=ab,target=/data,readonly"
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, want) {
		t.Fatalf("run args = %v, want %q", args, want)
	}
}

func TestStopRejectsNegativeTimeoutBeforeBackend(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil
	timeout := -time.Second
	err := ctr.Stop(context.Background(), &timeout)
	assertPublicValidationError(t, err, "Stop", "timeout", timeout,
		"stop timeout must be >= 0, got -1s")
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for a negative stop timeout: %v", f.calls)
	}
}

func TestValidationErrorFieldAndSecretContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		option string
		apply  func() error
	}{
		{
			name:   "env key",
			option: "WithEnv",
			apply:  func() error { return (WithEnv(map[string]string{"BAD=KEY": "value"}))(newConfig()) },
		},
		{
			name:   "label key",
			option: "WithLabels",
			apply:  func() error { return (WithLabels(map[string]string{"BAD=KEY": "value"}))(newConfig()) },
		},
		{
			name:   "exec key",
			option: "WithExecEnv",
			apply: func() error {
				return (WithExecEnv(map[string]string{"BAD=KEY": "value"}))(&execConfig{env: map[string]string{}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.apply()
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
			if validationErr.Option != tc.option || validationErr.Field != "key" || validationErr.Value != "BAD=KEY" {
				t.Fatalf("validation metadata = %#v, want %s/key/BAD=KEY", validationErr, tc.option)
			}
		})
	}

	secret := "super-secret\n"
	err := (WithEnv(map[string]string{"TOKEN": secret}))(newConfig())
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if validationErr.Option != "WithEnv" || validationErr.Field != "value" {
		t.Fatalf("validation option/field = %q/%q, want WithEnv/value", validationErr.Option, validationErr.Field)
	}
	if validationErr.Value != nil {
		t.Fatalf("secret value stored in ValidationError: %#v", validationErr.Value)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("secret leaked in error: %v", err)
	}

	err = (WithLabels(map[string]string{"token": "label-secret\x00"}))(newConfig())
	if !errors.As(err, &validationErr) {
		t.Fatalf("label error = %T %v, want *ValidationError", err, err)
	}
	if validationErr.Option != "WithLabels" || validationErr.Field != "value" || validationErr.Value != nil {
		t.Fatalf("label validation metadata = %#v, want WithLabels/value/nil", validationErr)
	}
	if strings.Contains(err.Error(), "label-secret") {
		t.Fatalf("label secret leaked in error: %v", err)
	}

	err = (WithExecEnv(map[string]string{"TOKEN": secret}))(&execConfig{env: map[string]string{}})
	if !errors.As(err, &validationErr) {
		t.Fatalf("exec error = %T %v, want *ValidationError", err, err)
	}
	if validationErr.Option != "WithExecEnv" || validationErr.Field != "value" || validationErr.Value != nil {
		t.Fatalf("exec validation metadata = %#v, want WithExecEnv/value/nil", validationErr)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("exec secret leaked in error: %v", err)
	}
}

func TestValidationErrorMessageTracksErr(t *testing.T) {
	cause := errors.New("canonical cause")
	err := &ValidationError{Option: "Test", Field: "value", Message: "stale message", Err: cause}
	err.Message = "mutated message"
	if got := err.Error(); got != "canonical cause" {
		t.Fatalf("Error() = %q, want Err-derived message", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("ValidationError lost its Err cause")
	}
	err.Err = errors.New("replacement cause")
	if got := err.Error(); got != "replacement cause" {
		t.Fatalf("Error() after Err mutation = %q, want replacement cause", got)
	}
}
