package container

import (
	"context"
	"errors"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type mockRunner struct {
	calls [][]string
	runFn func(ctx context.Context, args ...string) ([]byte, []byte, error)
}

func (m *mockRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	m.calls = append(m.calls, append([]string(nil), args...))
	if m.runFn != nil {
		return m.runFn(ctx, args...)
	}
	return nil, nil, nil
}

func TestWithRunnerValidation(t *testing.T) {
	cfg := newConfig()
	err := WithRunner(nil)(cfg)
	if err == nil {
		t.Fatal("WithRunner(nil) succeeded, want error")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "WithRunner" {
		t.Fatalf("error = %v, want ValidationError for WithRunner", err)
	}
}

func TestWithRunnerInjected(t *testing.T) {
	r := &mockRunner{}
	cfg := newConfig()
	if err := WithRunner(r)(cfg); err != nil {
		t.Fatalf("WithRunner: %v", err)
	}
	if cfg.runner != r {
		t.Fatalf("cfg.runner = %v, want %v", cfg.runner, r)
	}
}

func TestWithBinaryValidation(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"nul byte", "/usr/bin/docker\x00"},
		{"newline", "/usr/bin/docker\n"},
		{"carriage return", "/usr/bin/docker\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newConfig()
			err := WithBinary(tt.path)(cfg)
			if err == nil {
				t.Fatalf("WithBinary(%q) succeeded, want error", tt.path)
			}
			var valErr *ValidationError
			if !errors.As(err, &valErr) || valErr.Option != "WithBinary" {
				t.Fatalf("error = %v, want ValidationError for WithBinary", err)
			}
		})
	}
}

func TestWithBinaryOverridesDefaultEngineBinary(t *testing.T) {
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	if err := WithBinary("/custom/bin/docker")(cfg); err != nil {
		t.Fatalf("WithBinary: %v", err)
	}
	applyEngineBinary(cfg)
	er, ok := cfg.runner.(*ExecRunner)
	if !ok {
		t.Fatalf("cfg.runner is %T, want *ExecRunner", cfg.runner)
	}
	if er.Binary != "/custom/bin/docker" {
		t.Errorf("er.Binary = %q, want /custom/bin/docker", er.Binary)
	}
}

func TestWithBinaryCombinedWithWithRunner(t *testing.T) {
	// Case 1: WithRunner before WithBinary
	cfg1 := newConfig()
	cfg1.eng = dockerEngine{}
	execR1 := &ExecRunner{}
	if err := WithRunner(execR1)(cfg1); err != nil {
		t.Fatal(err)
	}
	if err := WithBinary("/override/bin")(cfg1); err != nil {
		t.Fatal(err)
	}
	applyEngineBinary(cfg1)
	if execR1.Binary != "/override/bin" {
		t.Errorf("execR1.Binary = %q, want /override/bin", execR1.Binary)
	}

	// Case 2: WithBinary before WithRunner
	cfg2 := newConfig()
	cfg2.eng = dockerEngine{}
	if err := WithBinary("/override/bin")(cfg2); err != nil {
		t.Fatal(err)
	}
	execR2 := &ExecRunner{}
	if err := WithRunner(execR2)(cfg2); err != nil {
		t.Fatal(err)
	}
	applyEngineBinary(cfg2)
	if execR2.Binary != "/override/bin" {
		t.Errorf("execR2.Binary = %q, want /override/bin", execR2.Binary)
	}

	// Case 3: Custom runner implementing SetBinary
	cfg3 := newConfig()
	cfg3.eng = dockerEngine{}
	customR := &customBinaryRunner{}
	if err := WithBinary("/custom/binary")(cfg3); err != nil {
		t.Fatal(err)
	}
	if err := WithRunner(customR)(cfg3); err != nil {
		t.Fatal(err)
	}
	applyEngineBinary(cfg3)
	if customR.bin != "/custom/binary" {
		t.Errorf("customR.bin = %q, want /custom/binary", customR.bin)
	}
}

type customBinaryRunner struct {
	bin   string
	calls [][]string
}

func (r *customBinaryRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return nil, nil, nil
}

func (r *customBinaryRunner) SetBinary(binary string) {
	r.bin = binary
}

func TestPullOptionsValidation(t *testing.T) {
	ctx := context.Background()

	// Nil option
	err := Pull(ctx, "redis:7-alpine", nil)
	if err == nil {
		t.Fatal("Pull(nil option) succeeded, want error")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "Pull" {
		t.Fatalf("error = %v, want ValidationError for Pull", err)
	}

	// Nil runner
	err = Pull(ctx, "redis:7-alpine", WithPullRunner(nil))
	if err == nil {
		t.Fatal("Pull(WithPullRunner(nil)) succeeded, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithPullRunner" {
		t.Fatalf("error = %v, want ValidationError for WithPullRunner", err)
	}

	// Empty binary
	err = Pull(ctx, "redis:7-alpine", WithPullBinary(""))
	if err == nil {
		t.Fatal("Pull(WithPullBinary(\"\")) succeeded, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithPullBinary" {
		t.Fatalf("error = %v, want ValidationError for WithPullBinary", err)
	}

	// Invalid characters in binary
	err = Pull(ctx, "redis:7-alpine", WithPullBinary("bad\x00path"))
	if err == nil {
		t.Fatal("Pull(WithPullBinary(bad)) succeeded, want error")
	}
}

func TestPullWithCustomRunner(t *testing.T) {
	r := &mockRunner{}
	err := Pull(context.Background(), "redis:7-alpine", WithPullRunner(r))
	if err != nil {
		t.Fatalf("Pull with mock runner: %v", err)
	}
	if len(r.calls) == 0 {
		t.Fatal("expected runner to be called")
	}
}

func TestPrunePublicOptionValidation(t *testing.T) {
	ctx := context.Background()

	// Nil option
	_, err := Prune(ctx, nil)
	if err == nil {
		t.Fatal("Prune(nil) succeeded, want error")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "Prune" {
		t.Fatalf("error = %v, want ValidationError for Prune", err)
	}

	// Nil runner
	_, err = Prune(ctx, WithPruneRunner(nil))
	if err == nil {
		t.Fatal("Prune(WithPruneRunner(nil)) succeeded, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithPruneRunner" {
		t.Fatalf("error = %v, want ValidationError for WithPruneRunner", err)
	}

	// Invalid binary
	_, err = Prune(ctx, WithPruneBinary(""))
	if err == nil {
		t.Fatal("Prune(WithPruneBinary(\"\")) succeeded, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithPruneBinary" {
		t.Fatalf("error = %v, want ValidationError for WithPruneBinary", err)
	}
}

func TestPruneWithCustomRunner(t *testing.T) {
	r := &mockRunner{
		runFn: func(ctx context.Context, args ...string) ([]byte, []byte, error) {
			// Answer the list call in the shape the detected backend parses:
			// the Apple list is JSON, the Docker list is newline-separated IDs.
			// An empty result has to mean "nothing to remove" on both.
			if args[0] == "ls" {
				return []byte("[]"), nil, nil
			}
			return nil, nil, nil
		},
	}
	removed, err := Prune(context.Background(), WithPruneRunner(r))
	if err != nil {
		t.Fatalf("Prune with mock runner: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want empty", removed)
	}
	if len(r.calls) == 0 {
		t.Fatal("expected runner to be called")
	}
}

func TestPruneReuseGroupOptionsValidation(t *testing.T) {
	ctx := context.Background()

	// Nil option
	_, err := PruneReuseGroup(ctx, "integration", nil)
	if err == nil {
		t.Fatal("PruneReuseGroup(nil) succeeded, want error")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "PruneReuseGroup" {
		t.Fatalf("error = %v, want ValidationError for PruneReuseGroup", err)
	}

	// Nil runner
	_, err = PruneReuseGroup(ctx, "integration", WithPruneRunner(nil))
	if err == nil {
		t.Fatal("PruneReuseGroup(WithPruneRunner(nil)) succeeded, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithPruneRunner" {
		t.Fatalf("error = %v, want ValidationError for WithPruneRunner", err)
	}

	// Invalid binary
	_, err = PruneReuseGroup(ctx, "integration", WithPruneBinary(""))
	if err == nil {
		t.Fatal("PruneReuseGroup(WithPruneBinary(\"\")) succeeded, want error")
	}
	if !errors.As(err, &valErr) || valErr.Option != "WithPruneBinary" {
		t.Fatalf("error = %v, want ValidationError for WithPruneBinary", err)
	}
}

func TestPruneReuseGroupWithCustomRunner(t *testing.T) {
	r := &mockRunner{
		runFn: func(ctx context.Context, args ...string) ([]byte, []byte, error) {
			// See TestPruneWithCustomRunner: the list shape differs per backend.
			if args[0] == "ls" {
				return []byte("[]"), nil, nil
			}
			return nil, nil, nil
		},
	}
	removed, err := PruneReuseGroup(context.Background(), "integration", WithPruneRunner(r))
	if err != nil {
		t.Fatalf("PruneReuseGroup with mock runner: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want empty", removed)
	}
	if len(r.calls) == 0 {
		t.Fatal("expected runner to be called")
	}
}

func TestPullAndPruneOrderIndependence(t *testing.T) {
	// Pull order 1: WithPullRunner then WithPullBinary
	r1 := &customBinaryRunner{}
	_ = Pull(context.Background(), "redis:7-alpine", WithPullRunner(r1), WithPullBinary("/custom/pull1"))
	if r1.bin != "/custom/pull1" {
		t.Errorf("r1.bin = %q, want /custom/pull1", r1.bin)
	}

	// Pull order 2: WithPullBinary then WithPullRunner
	r2 := &customBinaryRunner{}
	_ = Pull(context.Background(), "redis:7-alpine", WithPullBinary("/custom/pull2"), WithPullRunner(r2))
	if r2.bin != "/custom/pull2" {
		t.Errorf("r2.bin = %q, want /custom/pull2", r2.bin)
	}

	// Prune order 1: WithPruneRunner then WithPruneBinary
	r3 := &customBinaryRunner{}
	_, _ = Prune(context.Background(), WithPruneRunner(r3), WithPruneBinary("/custom/prune1"))
	if r3.bin != "/custom/prune1" {
		t.Errorf("r3.bin = %q, want /custom/prune1", r3.bin)
	}

	// Prune order 2: WithPruneBinary then WithPruneRunner
	r4 := &customBinaryRunner{}
	_, _ = Prune(context.Background(), WithPruneBinary("/custom/prune2"), WithPruneRunner(r4))
	if r4.bin != "/custom/prune2" {
		t.Errorf("r4.bin = %q, want /custom/prune2", r4.bin)
	}
}

// A configured binary names the executable to run, while the backend identity
// error classification compares against stays the backend's own CLI. Without
// that split, a wrapper executable would stop the backend from recognizing
// errors its own CLI produced, silently degrading not-found, absence and
// conflict detection.
func TestConfiguredBinaryKeepsBackendIdentity(t *testing.T) {
	tests := []struct {
		name   string
		binary string
	}{
		{"renamed wrapper", "/opt/docker-wrapper"},
		{"same name in another directory", "/usr/local/bin/docker"},
		{"bare name", "docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, ok := effectiveRunner(&ExecRunner{Binary: tt.binary}, "", dockerEngine{}).(*ExecRunner)
			if !ok {
				t.Fatalf("runner = %T, want *ExecRunner", runner)
			}
			if runner.Binary != tt.binary {
				t.Fatalf("runner binary = %q, want the configured %q", runner.Binary, tt.binary)
			}
			if runner.Identity != "docker" {
				t.Fatalf("runner identity = %q, want docker", runner.Identity)
			}
			// The identity is what classification compares, so the backend must
			// keep recognizing errors its CLI produced under the override.
			cliErr := &cli.CLIError{Binary: runner.Identity, Args: []string{"inspect", "abc"}, ExitCode: 1}
			if !cliBinaryMatches(cliErr.Binary, (dockerEngine{}).binary()) {
				t.Fatalf("CLIError binary %q did not match the backend", cliErr.Binary)
			}
		})
	}
}

func TestUnconfiguredEnginesKeepDefaultBinary(t *testing.T) {
	if got := (dockerEngine{}).binary(); got != "docker" {
		t.Fatalf("docker binary = %q, want docker", got)
	}
	if got := (dockerEngine{}).probe().Binary; got != "docker" {
		t.Fatalf("docker probe binary = %q, want docker", got)
	}
	if got := (appleEngine{}).binary(); got != "container" {
		t.Fatalf("apple binary = %q, want container", got)
	}
	if got := (appleEngine{}).probe().Binary; got != "container" {
		t.Fatalf("apple probe binary = %q, want container", got)
	}
}

// An ExecRunner the caller supplied without naming an executable inherits the
// backend's CLI, which is what Run does for its default runner. Leaving it
// empty would make ExecRunner fall back to "container" on every backend.
func TestEffectiveRunnerAppliesEngineBinary(t *testing.T) {
	inherited := effectiveRunner(&ExecRunner{}, "", dockerEngine{})
	er, ok := inherited.(*ExecRunner)
	if !ok {
		t.Fatalf("runner = %T, want *ExecRunner", inherited)
	}
	if er.Binary != "docker" {
		t.Fatalf("runner binary = %q, want docker", er.Binary)
	}

	configured := effectiveRunner(&ExecRunner{}, "/opt/docker-wrapper", dockerEngine{})
	if er := configured.(*ExecRunner); er.Binary != "/opt/docker-wrapper" {
		t.Fatalf("runner binary = %q, want the configured path", er.Binary)
	}
}

// A typed-nil pointer stored in the runner interface passes an == nil test and
// then panics when the runner is used, so every runner option has to reject it
// by value.
func TestRunnerOptionsRejectTypedNil(t *testing.T) {
	var typedNil *ExecRunner
	options := []struct {
		name string
		err  error
	}{
		{"WithRunner", WithRunner(typedNil)(newConfig())},
		{"WithPruneRunner", WithPruneRunner(typedNil)(&pruneConfig{})},
		{"WithPullRunner", WithPullRunner(typedNil)(&pullConfig{})},
	}
	for _, tt := range options {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatal("typed-nil runner was accepted, want a validation error")
			}
			var valErr *ValidationError
			if !errors.As(tt.err, &valErr) {
				t.Fatalf("error = %v, want ValidationError", tt.err)
			}
		})
	}
}

func TestTypeAliasesUsable(t *testing.T) {
	var _ Runner = &ExecRunner{}
	var _ ExternalRunner = &ExecRunner{}
	var _ Streamer = &ExecRunner{}
}

// The Run path stamps the identity on the runner it resolves, so a wrapper
// configured through WithBinary still classifies as its backend. The caller's
// own runner is left untouched: the library supports concurrent callers sharing
// one runner.
func TestRunStampsEngineIdentity(t *testing.T) {
	configured := &ExecRunner{Binary: "/opt/docker-wrapper"}
	cfg := newConfig()
	cfg.eng = dockerEngine{}
	cfg.runner = configured
	applyEngineBinary(cfg)
	er, ok := cfg.runner.(*ExecRunner)
	if !ok {
		t.Fatalf("runner = %T, want *ExecRunner", cfg.runner)
	}
	if er.Identity != "docker" {
		t.Fatalf("identity = %q, want docker", er.Identity)
	}
	if er.Binary != "/opt/docker-wrapper" {
		t.Fatalf("binary = %q, want the configured wrapper", er.Binary)
	}
	if configured.Identity != "" {
		t.Fatalf("caller runner was mutated: identity = %q", configured.Identity)
	}
}
