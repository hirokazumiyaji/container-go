package container

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

func applyOptions(t *testing.T, cfg *config, opts ...Option) {
	t.Helper()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
}

func TestReuseEnvFingerprintValue(t *testing.T) {
	first := newConfig()
	applyOptions(t, first, WithReuseEnvFingerprint(), WithEnv(map[string]string{"A": "1", "B": "2"}))
	second := newConfig()
	applyOptions(t, second, WithReuseEnvFingerprint(), WithEnv(map[string]string{"B": "2", "A": "1"}))
	if first.reuseEnvFingerprintValue() == "" {
		t.Fatal("environment fingerprint is empty with the option set")
	}
	if first.reuseEnvFingerprintValue() != second.reuseEnvFingerprintValue() {
		t.Fatal("equal environments in different insertion order produced different fingerprints")
	}

	for name, env := range map[string]map[string]string{
		"changed value": {"A": "2", "B": "2"},
		"changed key":   {"A": "1", "C": "2"},
		"added pair":    {"A": "1", "B": "2", "C": "3"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := newConfig()
			applyOptions(t, changed, WithReuseEnvFingerprint(), WithEnv(env))
			if changed.reuseEnvFingerprintValue() == first.reuseEnvFingerprintValue() {
				t.Fatal("different environments produced the same fingerprint")
			}
		})
	}

	withoutOption := newConfig()
	applyOptions(t, withoutOption, WithEnv(map[string]string{"A": "1"}))
	if got := withoutOption.reuseEnvFingerprintValue(); got != "" {
		t.Fatalf("unrequested fingerprint = %q, want empty", got)
	}

	// The existing creation fingerprint intentionally ignores the
	// environment; only the opt-in label may distinguish it.
	legacyA := newConfig()
	applyOptions(t, legacyA, WithReuseEnvFingerprint(), WithEnv(map[string]string{"A": "1"}))
	legacyB := newConfig()
	applyOptions(t, legacyB, WithReuseEnvFingerprint(), WithEnv(map[string]string{"A": "2"}))
	if legacyA.reuseConfigHash() != legacyB.reuseConfigHash() {
		t.Fatal("environment values leaked into the creation configuration fingerprint")
	}
}

func TestReuseEnvFingerprintRequiresReuse(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuseEnvFingerprint(),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "WithReuseEnvFingerprint requires WithReuse") {
		t.Fatalf("error = %v, want WithReuseEnvFingerprint requires WithReuse", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called for an invalid reuse option: %v", f.calls)
	}
}

func TestReuseEnvFingerprintFlightKey(t *testing.T) {
	base := newConfig()
	base.eng = dockerEngine{}
	base.name = "shared"
	base.reuse = true
	applyOptions(t, base, WithReuseEnvFingerprint(), WithEnv(map[string]string{"A": "1"}))

	changed := newConfig()
	changed.eng = dockerEngine{}
	changed.name = "shared"
	changed.reuse = true
	applyOptions(t, changed, WithReuseEnvFingerprint(), WithEnv(map[string]string{"A": "2"}))
	if reuseFlightKey(base, "image") == reuseFlightKey(changed, "image") {
		t.Fatal("different fingerprinted environments shared one flight")
	}

	// Without the option the environment is deliberately ignored, exactly
	// as before this feature.
	offA := newConfig()
	offA.eng = dockerEngine{}
	offA.name = "shared"
	applyOptions(t, offA, WithEnv(map[string]string{"A": "1"}))
	offB := newConfig()
	offB.eng = dockerEngine{}
	offB.name = "shared"
	applyOptions(t, offB, WithEnv(map[string]string{"A": "2"}))
	if reuseFlightKey(offA, "image") != reuseFlightKey(offB, "image") {
		t.Fatal("environment changed the flight key without WithReuseEnvFingerprint")
	}
}

func TestReuseEnvFingerprintLabelsHideRawValues(t *testing.T) {
	const secret = "super-secret-env-value"
	f := newReuseCreateRunner()
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithReuseEnvFingerprint(),
		WithEnv(map[string]string{"TOKEN": secret}),
		withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	runArgs := f.callWith("run")
	if strings.Contains(strings.Join(runArgs, " "), secret) {
		t.Fatalf("raw environment value reached argv: %v", runArgs)
	}
	f.mu.Lock()
	labels := maps.Clone(f.labels)
	f.mu.Unlock()
	fingerprint, ok := labels[reuseEnvLabel]
	if !ok {
		t.Fatalf("missing environment fingerprint label: %v", labels)
	}
	if len(fingerprint) != 64 || !domain.IsHex(fingerprint) {
		t.Fatalf("fingerprint = %q, want a SHA-256 hex digest", fingerprint)
	}
	for key, value := range labels {
		if strings.Contains(key+value, secret) {
			t.Fatalf("raw environment value reached label %q", key)
		}
	}

	expected := newConfig()
	applyOptions(t, expected, WithReuseEnvFingerprint(), WithEnv(map[string]string{"TOKEN": secret}))
	if fingerprint != expected.reuseEnvFingerprintValue() {
		t.Fatalf("label fingerprint = %q, want %q", fingerprint, expected.reuseEnvFingerprintValue())
	}

	withoutOption := newReuseCreateRunner()
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithEnv(map[string]string{"TOKEN": secret}),
		withRunner(withoutOption), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run without option: %v", err)
	}
	withoutOption.mu.Lock()
	labels = maps.Clone(withoutOption.labels)
	withoutOption.mu.Unlock()
	if _, ok := labels[reuseEnvLabel]; ok {
		t.Fatalf("environment fingerprint emitted without the option: %v", labels)
	}
}

func TestReuseEnvFingerprintAttach(t *testing.T) {
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			fingerprintedCfg := newConfig()
			fingerprintedCfg.reuse = true
			applyOptions(t, fingerprintedCfg, WithReuseEnvFingerprint(), WithEnv(map[string]string{"TOKEN": "one"}))
			fingerprinted := reuseInspectWithLabels(backend.inspect(backend.runningSpec()), fingerprintedCfg.allLabels())

			plainCfg := newConfig()
			plainCfg.reuse = true
			plain := reuseInspectWithLabels(backend.inspect(backend.runningSpec()), plainCfg.allLabels())

			for _, tc := range []struct {
				name    string
				data    []byte
				opts    []Option
				wantErr bool
			}{
				{
					name: "matching environment",
					data: fingerprinted,
					opts: []Option{WithReuseEnvFingerprint(), WithEnv(map[string]string{"TOKEN": "one"})},
				},
				{
					name:    "changed environment value",
					data:    fingerprinted,
					opts:    []Option{WithReuseEnvFingerprint(), WithEnv(map[string]string{"TOKEN": "two"})},
					wantErr: true,
				},
				{
					name:    "changed environment key",
					data:    fingerprinted,
					opts:    []Option{WithReuseEnvFingerprint(), WithEnv(map[string]string{"OTHER": "one"})},
					wantErr: true,
				},
				{
					name:    "missing fingerprint",
					data:    plain,
					opts:    []Option{WithReuseEnvFingerprint(), WithEnv(map[string]string{"TOKEN": "one"})},
					wantErr: true,
				},
				{
					name: "option omitted ignores environment",
					data: fingerprinted,
					opts: []Option{WithEnv(map[string]string{"TOKEN": "two"})},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					runner := newReuseTransitionRunner(tc.data, tc.data, backend.eng)
					opts := append([]Option{WithName("shared"), WithReuse(), withRunner(runner), withEngine(backend.eng)}, tc.opts...)
					ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
					if tc.wantErr {
						if !errors.Is(err, ErrReuseMismatch) {
							t.Fatalf("error = %v, want ErrReuseMismatch", err)
						}
						if ctr != nil {
							t.Fatal("mismatched environment returned a handle")
						}
						return
					}
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					if ctr == nil {
						t.Fatal("missing reused handle")
					}
				})
			}
		})
	}
}

func TestReuseEnvFingerprintReservedLabel(t *testing.T) {
	if !domain.IsReservedLabel(domain.LabelReuseEnvFingerprint) {
		t.Fatal("environment fingerprint label is not reserved")
	}
	labels := domain.BuildLabels(domain.LabelConfig{Custom: map[string]string{
		domain.LabelReuseEnvFingerprint: "forged",
	}})
	if _, ok := labels[domain.LabelReuseEnvFingerprint]; ok {
		t.Fatalf("caller-supplied fingerprint survived: %v", labels)
	}
	labels = domain.BuildLabels(domain.LabelConfig{
		Reuse:               true,
		ReuseEnvFingerprint: "digest",
	})
	if labels[domain.LabelReuseEnvFingerprint] != "digest" {
		t.Fatalf("fingerprint = %q, want digest", labels[domain.LabelReuseEnvFingerprint])
	}
	labels = domain.BuildLabels(domain.LabelConfig{ReuseEnvFingerprint: "digest"})
	if _, ok := labels[domain.LabelReuseEnvFingerprint]; ok {
		t.Fatal("fingerprint emitted without reuse")
	}
}

func TestReuseConfigHashIncludesExtendedOptions(t *testing.T) {
	base := newConfig()
	for name, opt := range map[string]Option{
		"read-only":   WithReadOnlyRootFS(),
		"init":        WithInit(),
		"dns":         WithDNS("1.1.1.1", "8.8.8.8"),
		"stop-signal": WithStopSignal("SIGTERM"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := newConfig()
			if err := opt(cfg); err != nil {
				t.Fatalf("apply option: %v", err)
			}
			if cfg.reuseConfigHash() == base.reuseConfigHash() {
				t.Fatal("extended run option is absent from the reuse creation fingerprint")
			}
		})
	}
}
