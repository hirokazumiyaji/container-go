package container

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTimeoutOptionsValidation(t *testing.T) {
	tests := []struct {
		name       string
		opt        Option
		wantOption string
		wantErrMsg string
	}{
		{
			name:       "WithRunTimeout zero",
			opt:        WithRunTimeout(0),
			wantOption: "WithRunTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithRunTimeout negative",
			opt:        WithRunTimeout(-time.Second),
			wantOption: "WithRunTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithQueryTimeout zero",
			opt:        WithQueryTimeout(0),
			wantOption: "WithQueryTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithQueryTimeout negative",
			opt:        WithQueryTimeout(-time.Second),
			wantOption: "WithQueryTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithProbeTimeout zero",
			opt:        WithProbeTimeout(0),
			wantOption: "WithProbeTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithProbeTimeout negative",
			opt:        WithProbeTimeout(-time.Second),
			wantOption: "WithProbeTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithReuseAttachTimeout zero",
			opt:        WithReuseAttachTimeout(0),
			wantOption: "WithReuseAttachTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithReuseAttachTimeout negative",
			opt:        WithReuseAttachTimeout(-time.Second),
			wantOption: "WithReuseAttachTimeout",
			wantErrMsg: "timeout must be positive",
		},
		{
			name:       "WithReusePollInterval zero",
			opt:        WithReusePollInterval(0),
			wantOption: "WithReusePollInterval",
			wantErrMsg: "poll interval must be positive",
		},
		{
			name:       "WithReusePollInterval negative",
			opt:        WithReusePollInterval(-time.Millisecond),
			wantOption: "WithReusePollInterval",
			wantErrMsg: "poll interval must be positive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfig()
			err := tc.opt(cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var valErr *ValidationError
			if !errors.As(err, &valErr) {
				t.Fatalf("expected *ValidationError, got %T: %v", err, err)
			}
			if valErr.Option != tc.wantOption {
				t.Errorf("Option = %q, want %q", valErr.Option, tc.wantOption)
			}
			if valErr.Message != tc.wantErrMsg {
				t.Errorf("Message = %q, want %q", valErr.Message, tc.wantErrMsg)
			}
		})
	}
}

func TestReuseTimeoutCrossOptionValidation(t *testing.T) {
	t.Run("WithReuseAttachTimeout requires WithReuse", func(t *testing.T) {
		cfg := newConfig()
		if err := WithReuseAttachTimeout(5 * time.Second)(cfg); err != nil {
			t.Fatalf("unexpected option error: %v", err)
		}
		err := cfg.validate()
		if err == nil {
			t.Fatal("expected validate error, got nil")
		}
		var valErr *ValidationError
		if !errors.As(err, &valErr) {
			t.Fatalf("expected *ValidationError, got %T: %v", err, err)
		}
		if valErr.Option != "WithReuseAttachTimeout" {
			t.Errorf("Option = %q, want WithReuseAttachTimeout", valErr.Option)
		}
	})

	t.Run("WithReusePollInterval requires WithReuse", func(t *testing.T) {
		cfg := newConfig()
		if err := WithReusePollInterval(50 * time.Millisecond)(cfg); err != nil {
			t.Fatalf("unexpected option error: %v", err)
		}
		err := cfg.validate()
		if err == nil {
			t.Fatal("expected validate error, got nil")
		}
		var valErr *ValidationError
		if !errors.As(err, &valErr) {
			t.Fatalf("expected *ValidationError, got %T: %v", err, err)
		}
		if valErr.Option != "WithReusePollInterval" {
			t.Errorf("Option = %q, want WithReusePollInterval", valErr.Option)
		}
	})

	t.Run("WithReuse allows reuse timeouts", func(t *testing.T) {
		cfg := newConfig()
		if err := WithName("test-ctr")(cfg); err != nil {
			t.Fatal(err)
		}
		if err := WithReuse()(cfg); err != nil {
			t.Fatal(err)
		}
		if err := WithReuseAttachTimeout(10 * time.Second)(cfg); err != nil {
			t.Fatal(err)
		}
		if err := WithReusePollInterval(20 * time.Millisecond)(cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("expected valid config, got %v", err)
		}
		if cfg.effectiveReuseAttachTimeout() != 10*time.Second {
			t.Errorf("reuseAttachTimeout = %v, want 10s", cfg.effectiveReuseAttachTimeout())
		}
		if cfg.effectiveReusePollInterval() != 20*time.Millisecond {
			t.Errorf("reusePollInterval = %v, want 20ms", cfg.effectiveReusePollInterval())
		}
	})
}

func TestConfigEffectiveTimeouts(t *testing.T) {
	cfg := newConfig()
	if err := WithRunTimeout(2 * time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithQueryTimeout(15 * time.Second)(cfg); err != nil {
		t.Fatal(err)
	}
	if err := WithProbeTimeout(2 * time.Second)(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.effectiveRunTimeout() != 2*time.Minute {
		t.Errorf("effectiveRunTimeout = %v, want 2m", cfg.effectiveRunTimeout())
	}
	if cfg.effectiveQueryTimeout() != 15*time.Second {
		t.Errorf("effectiveQueryTimeout = %v, want 15s", cfg.effectiveQueryTimeout())
	}
	if cfg.effectiveProbeTimeout() != 2*time.Second {
		t.Errorf("effectiveProbeTimeout = %v, want 2s", cfg.effectiveProbeTimeout())
	}
}

func TestContainerTimeoutsAndShared(t *testing.T) {
	ctr := &Container{
		id:           "myctr",
		queryTimeout: 12 * time.Second,
		probeTimeout: 3 * time.Second,
	}

	if ctr.effectiveQueryTimeout() != 12*time.Second {
		t.Errorf("effectiveQueryTimeout = %v, want 12s", ctr.effectiveQueryTimeout())
	}
	if ctr.effectiveProbeTimeout() != 3*time.Second {
		t.Errorf("effectiveProbeTimeout = %v, want 3s", ctr.effectiveProbeTimeout())
	}

	shared := ctr.Shared()
	if shared == nil {
		t.Fatal("shared container is nil")
	}
	if shared.queryTimeout != ctr.queryTimeout {
		t.Errorf("shared.queryTimeout = %v, want %v", shared.queryTimeout, ctr.queryTimeout)
	}
	if shared.probeTimeout != ctr.probeTimeout {
		t.Errorf("shared.probeTimeout = %v, want %v", shared.probeTimeout, ctr.probeTimeout)
	}
}

func TestWaitForReusePollCustomInterval(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	interval := 30 * time.Millisecond
	if err := waitForReusePoll(ctx, interval); err != nil {
		t.Fatalf("waitForReusePoll: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond {
		t.Errorf("elapsed %v, want >= 20ms", elapsed)
	}
}

type timeoutTestRunner func(context.Context, ...string) ([]byte, []byte, error)

func (f timeoutTestRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return f(ctx, args...)
}

func TestWithQueryTimeoutAppliedToImageInspection(t *testing.T) {
	var observedDeadline time.Time
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if d, ok := ctx.Deadline(); ok {
			observedDeadline = d
		}
		return nil, nil, errors.New("simulated inspect error")
	})

	cfg := newConfig()
	cfg.runner = runner
	cfg.eng = dockerEngine{}
	customTimeout := 45 * time.Second
	if err := WithQueryTimeout(customTimeout)(cfg); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _, _ = cfg.inspectImage(context.Background(), "redis:alpine", "")
	if observedDeadline.IsZero() {
		t.Fatal("expected deadline to be set on context")
	}
	remaining := observedDeadline.Sub(start)
	if remaining < 40*time.Second || remaining > 50*time.Second {
		t.Errorf("remaining deadline = %v, want ~45s", remaining)
	}
}

func TestWithQueryTimeoutAppliedToDockerDefaultNetwork(t *testing.T) {
	var observedDeadline time.Time
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if d, ok := ctx.Deadline(); ok {
			observedDeadline = d
		}
		return []byte("linux\n"), nil, nil
	})

	cfg := newConfig()
	cfg.runner = runner
	cfg.eng = dockerEngine{}
	customTimeout := 45 * time.Second
	if err := WithQueryTimeout(customTimeout)(cfg); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	name, err := dockerDefaultNetwork(context.Background(), cfg.runner, cfg.eng, opTimeouts{query: cfg.effectiveQueryTimeout(), probe: cfg.effectiveProbeTimeout()})
	if err != nil {
		t.Fatalf("dockerDefaultNetwork error: %v", err)
	}
	if name != "bridge" {
		t.Errorf("name = %q, want bridge", name)
	}
	if observedDeadline.IsZero() {
		t.Fatal("expected deadline to be set on context")
	}
	remaining := observedDeadline.Sub(start)
	if remaining < 40*time.Second || remaining > 50*time.Second {
		t.Errorf("remaining deadline = %v, want ~45s", remaining)
	}
}

func TestWithQueryTimeoutAppliedToCopyFileVersion(t *testing.T) {
	var observedDeadline time.Time
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if d, ok := ctx.Deadline(); ok {
			observedDeadline = d
		}
		return []byte(`{"Client":{"Version":"30.0.0"},"Server":{"Version":"30.0.0"}}`), nil, nil
	})

	customTimeout := 45 * time.Second
	start := time.Now()
	err := dockerEngine{}.checkCopyFileFromContainerVersion(context.Background(), runner, opTimeouts{query: customTimeout})
	if err != nil {
		t.Fatalf("checkCopyFileFromContainerVersion error: %v", err)
	}
	if observedDeadline.IsZero() {
		t.Fatal("expected deadline to be set on context")
	}
	remaining := observedDeadline.Sub(start)
	if remaining < 40*time.Second || remaining > 50*time.Second {
		t.Errorf("remaining deadline = %v, want ~45s", remaining)
	}
}
