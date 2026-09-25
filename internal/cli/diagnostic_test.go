package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

func TestCLIErrorRedactsSecretsAndControlCharacters(t *testing.T) {
	const (
		password = "s3cret-password"
		token    = "tok-abc-123"
	)
	err := &CLIError{
		Binary:   "docker",
		Args:     []string{"run", "--env-file", "/tmp/password.env", "--label", "app.password=" + password, "image", "--token", token, "password=" + password},
		ExitCode: 1,
		Stderr:   "password=" + password + " token=" + token + "\x1b[31mrequest failed\x1b[0m\r\nnext",
	}

	got := err.Error()
	for _, secret := range []string{password, token, "/tmp/password.env"} {
		if strings.Contains(got, secret) {
			t.Errorf("Error() = %q, contains secret %q", got, secret)
		}
	}
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("Error() = %q, contains terminal control characters", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("Error() = %q, want redaction marker", got)
	}

	raw := err.RawError()
	if !strings.Contains(raw, password) || !strings.Contains(raw, token) || !strings.Contains(raw, "\x1b") || !strings.Contains(raw, "\r\n") {
		t.Errorf("RawError() = %q, want explicit unredacted diagnostic", raw)
	}
	rawSeparator := (&CLIError{Stderr: "before\u2028after\u2029"}).RawError()
	if !strings.Contains(rawSeparator, "\u2028") || !strings.Contains(rawSeparator, "\u2029") {
		t.Errorf("RawError() = %q, want raw Unicode separators", rawSeparator)
	}
}

func TestCLIErrorRedactsBareRunAndExecPayloadValues(t *testing.T) {
	const (
		runSecret  = "run-bare-secret"
		execSecret = "exec-bare-secret"
	)
	runErr := &CLIError{Args: []string{"run", "image", runSecret}, ExitCode: 1}
	execErr := &CLIError{Args: []string{"exec", "container", execSecret}, ExitCode: 1}
	for _, tc := range []struct {
		err    *CLIError
		secret string
	}{{runErr, runSecret}, {execErr, execSecret}} {
		if strings.Contains(tc.err.Error(), tc.secret) {
			t.Errorf("Error() = %q, contains %q", tc.err.Error(), tc.secret)
		}
	}
}

func TestWithRedactorTreatsExplicitValuesAsContext(t *testing.T) {
	const secret = "xy"
	raw := &CLIError{Args: []string{"inspect", "prefix" + secret + "suffix"}}
	err := WithRedactor(raw, NewRedactor(secret))
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Error() = %q, contains adjacent explicit value", err)
	}
}

func TestWithRedactorKeepsRawDiagnosticExplicit(t *testing.T) {
	const secret = "command-secret-value"
	raw := &CLIError{Args: []string{"run", "image", secret}, Stderr: secret, ExitCode: 1}
	err := WithRedactor(raw, NewRedactor(secret))

	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Error() = %q, contains configured secret", err.Error())
	}
	if !errors.Is(err, raw) {
		t.Fatal("redacted error no longer matches the original CLIError")
	}
	var got *CLIError
	if !errors.As(err, &got) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(got.Error(), secret) || strings.Contains(strings.Join(got.Args, " "), secret) || strings.Contains(got.Stderr, secret) {
		t.Fatalf("errors.As returned unsafe CLIError: error=%q args=%q stderr=%q", got.Error(), got.Args, got.Stderr)
	}
	if !strings.Contains(got.RawError(), secret) {
		t.Fatalf("RawError() = %q, want raw value", got.RawError())
	}
	if !strings.Contains(raw.RawError(), secret) {
		t.Fatalf("original RawError() = %q, want raw value", raw.RawError())
	}
}

func TestWithRedactorComposesAcrossWrappedErrors(t *testing.T) {
	const (
		first  = "first-boundary-secret"
		second = "second-boundary-secret"
	)
	raw := &CLIError{Args: []string{"run", "image", first, second}, Stderr: first + " " + second}
	firstWrap := WithRedactor(fmt.Errorf("context: %w", raw), NewRedactor(first))
	combined := WithRedactor(firstWrap, NewRedactor(second))
	var got *CLIError
	if !errors.As(combined, &got) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(got.Error(), first) || strings.Contains(got.Error(), second) {
		t.Fatalf("composed CLIError is unsafe: %q", got.Error())
	}
	if !strings.Contains(got.RawError(), first) || !strings.Contains(got.RawError(), second) {
		t.Fatalf("RawError() = %q, want both original values", got.RawError())
	}
}

func TestDiagnosticAndCLIWrappersComposeRedactors(t *testing.T) {
	const (
		first  = "cross-wrapper-first-secret"
		second = "cross-wrapper-second-secret"
	)
	raw := &CLIError{Args: []string{"run", "image", first, second}, Stderr: first + " " + second}
	firstWrap := WithRedactor(raw, NewContextRedactor(first))
	outer := diagnostic.WithRedactor(fmt.Errorf("context: %w", firstWrap), diagnostic.NewContextRedactor(second))
	var got *CLIError
	if !errors.As(outer, &got) {
		t.Fatal("errors.As did not find *CLIError through composed wrappers")
	}
	if strings.Contains(got.Error(), first) || strings.Contains(got.Error(), second) {
		t.Fatalf("composed CLIError is unsafe: %q", got.Error())
	}
	if !strings.Contains(got.RawError(), first) || !strings.Contains(got.RawError(), second) {
		t.Fatalf("RawError() = %q, want both original values", got.RawError())
	}
}

func TestClassifyPreservesOriginalAndProbeChains(t *testing.T) {
	original := &CLIError{Args: []string{"run", "image"}, Stderr: "original-secret", ExitCode: 1}
	probe := &CLIError{Args: []string{"system", "status"}, Stderr: "probe-secret", ExitCode: 1}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probe},
	}}

	classified := Classify(context.Background(), r, original, appleProbe)
	if !errors.Is(classified, ErrSystemNotRunning) || !errors.Is(classified, original) || !errors.Is(classified, probe) {
		t.Fatalf("classified error lost a chain link: %v", classified)
	}
	var system *SystemNotRunningError
	if !errors.As(classified, &system) {
		t.Fatalf("classified error = %T, want *SystemNotRunningError", classified)
	}
	if system.OriginalError() != original || system.ProbeError() != probe {
		t.Fatalf("system error links = original %v probe %v", system.OriginalError(), system.ProbeError())
	}

	safe := WithRedactor(classified, NewRedactor("original-secret", "probe-secret"))
	if strings.Contains(safe.Error(), "original-secret") || strings.Contains(safe.Error(), "probe-secret") {
		t.Fatalf("safe classified error leaked a value: %q", safe)
	}
	var got *CLIError
	if !errors.As(safe, &got) {
		t.Fatal("errors.As did not find original CLIError through classification")
	}
	if strings.Contains(got.Error(), "original-secret") || strings.Contains(got.Stderr, "original-secret") {
		t.Fatalf("errors.As CLIError is unsafe: error=%q stderr=%q", got.Error(), got.Stderr)
	}
	if !strings.Contains(got.RawError(), "original-secret") {
		t.Fatalf("RawError() = %q, want original raw value", got.RawError())
	}
}
