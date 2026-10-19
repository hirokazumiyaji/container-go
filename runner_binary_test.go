package container

import (
	"context"
	"errors"
	"testing"
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

func TestTypeAliasesUsable(t *testing.T) {
	var _ Runner = &ExecRunner{}
	var _ ExternalRunner = &ExecRunner{}
	var _ Streamer = &ExecRunner{}
}
