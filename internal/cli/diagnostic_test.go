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
	if !strings.Contains(raw.RawError(), first) || !strings.Contains(raw.RawError(), second) {
		t.Fatalf("original RawError() = %q, want both original values", raw.RawError())
	}
}

func TestDiagnosticWrapperDoesNotExposeRawCLIError(t *testing.T) {
	const secret = "diagnostic-wrapper-raw-117"
	raw := &CLIError{Args: []string{"run", secret}, Stderr: secret, ExitCode: 1}
	safe := diagnostic.WithRedactor(fmt.Errorf("context: %w", raw), diagnostic.NewRedactor(secret))
	if got := errors.Unwrap(safe); got != nil {
		t.Fatalf("errors.Unwrap(diagnostic safe error) = %v, want nil", got)
	}
	var got *CLIError
	if !errors.As(safe, &got) {
		t.Fatal("errors.As did not find safe CLIError")
	}
	if strings.Contains(got.Error(), secret) || strings.Contains(got.Stderr, secret) {
		t.Fatalf("diagnostic errors.As exposed raw CLIError: %q", got.Error())
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
	if !strings.Contains(raw.RawError(), first) || !strings.Contains(raw.RawError(), second) {
		t.Fatalf("original RawError() = %q, want both original values", raw.RawError())
	}
}

func TestSafeWrappersDoNotExposeRawThroughUnwrapOrAs(t *testing.T) {
	const secret = "safe-wrapper-secret-117"
	raw := &CLIError{Args: []string{"run", "image", secret}, Stderr: secret, ExitCode: 1}
	safe := WithRedactor(fmt.Errorf("outer: %w", raw), NewRedactor(secret))

	if got := errors.Unwrap(safe); got != nil {
		t.Fatalf("errors.Unwrap(safe) = %v, want nil", got)
	}
	var cliErr *CLIError
	if !errors.As(safe, &cliErr) {
		t.Fatal("errors.As did not find safe CLIError")
	}
	if cliErr == raw || strings.Contains(cliErr.Error(), secret) || strings.Contains(strings.Join(cliErr.Args, " "), secret) || strings.Contains(cliErr.Stderr, secret) {
		t.Fatalf("errors.As exposed raw CLIError: %#v", cliErr)
	}
	if got := errors.Unwrap(cliErr); got != nil {
		t.Fatalf("errors.Unwrap(CLIError clone) = %v, want nil", got)
	}
	if !errors.Is(safe, raw) {
		t.Fatal("safe wrapper lost errors.Is identity")
	}
	rawProvider, ok := safe.(interface{ UnwrapRaw() error })
	if !ok || rawProvider.UnwrapRaw() == nil {
		t.Fatal("safe wrapper is missing explicit UnwrapRaw")
	}
}

func TestSafeSystemNotRunningCloneRedactsBothChildren(t *testing.T) {
	const originalSecret = "original-system-secret-117"
	const probeSecret = "probe-system-secret-117"
	original := &CLIError{Args: []string{"run", "image", originalSecret}, Stderr: originalSecret, ExitCode: 1}
	probe := &CLIError{Args: []string{"system", "status"}, Stderr: probeSecret, ExitCode: 1}
	classified := &SystemNotRunningError{hint: "start the service", original: original, probe: probe}
	safe := WithRedactor(classified, NewRedactor(originalSecret, probeSecret))

	var system *SystemNotRunningError
	if !errors.As(safe, &system) {
		t.Fatal("errors.As did not find safe SystemNotRunningError")
	}
	if system == classified || strings.Contains(system.Error(), originalSecret) || strings.Contains(system.Error(), probeSecret) {
		t.Fatalf("system clone is unsafe: %q", system.Error())
	}
	for _, child := range []error{system.OriginalError(), system.ProbeError()} {
		if child == original || child == probe { //nolint:errorlint // identity assertion; errors.Is would also match the wrapped child this check distinguishes
			t.Fatalf("accessor returned raw child: %T", child)
		}
		if got := errors.Unwrap(child); got != nil {
			t.Fatalf("errors.Unwrap(safe child) = %v, want nil", got)
		}
	}
	if !errors.Is(safe, original) || !errors.Is(safe, probe) || !errors.Is(safe, ErrSystemNotRunning) {
		t.Fatal("safe system clone lost intended errors.Is links")
	}
	var childCLI *CLIError
	if !errors.As(safe, &childCLI) || strings.Contains(childCLI.Error(), originalSecret) || strings.Contains(childCLI.Stderr, originalSecret) {
		t.Fatalf("errors.As through safe system exposed unsafe CLIError: %v", childCLI)
	}
}

func TestClassifyPreservesOriginalAndProbeChains(t *testing.T) {
	original := &CLIError{Args: []string{"run", "image"}, Stderr: "original-secret", ExitCode: 1}
	probe := &CLIError{Args: []string{"system", "status"}, Stderr: "probe-secret", ExitCode: 1}
	r := &fakeRunner{results: map[string]fakeResult{
		"system status": {err: probe},
	}}

	classified := Classify(context.Background(), r, original, appleProbe)
	if !errors.Is(classified, original) || !errors.Is(classified, probe) {
		t.Fatalf("classified error lost a chain link: %v", classified)
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
}

func TestClassificationTreePreservesWrapperShapeAndIdentity(t *testing.T) {
	leaf := errors.New("private-value")
	safe := WithRedactor(leaf, NewRedactor("private-value"))
	wrapper := fmt.Errorf("TLS certificate verification failed: %w", safe)
	tree := classificationErrorTree(wrapper)
	single, ok := tree.(interface{ Unwrap() error })
	if !ok || single.Unwrap() != leaf { //nolint:errorlint // identity assertion; errors.Is would also match the wrapped child this check distinguishes
		t.Fatalf("classification changed single wrapper shape: %T", tree)
	}
	if !errors.Is(tree, wrapper) || !errors.Is(tree, leaf) {
		t.Fatal("classification lost wrapper identity")
	}
	if !IsDefinitiveNonLivenessError(tree) {
		t.Fatal("classification lost wrapper TLS text")
	}
	if strings.Contains(wrapper.Error(), "private-value") {
		t.Fatal("classification changed safe diagnostics")
	}
}

func TestClassificationTreeClonesMutableJoinedChildren(t *testing.T) {
	leaf := errors.New("private-value")
	safe := WithRedactor(leaf, NewRedactor("private-value"))
	original := errors.Join(safe, errors.New("other"))
	children := original.(interface{ Unwrap() []error }).Unwrap()
	tree := classificationErrorTree(original)
	if children[0] != safe { //nolint:errorlint // identity assertion; errors.Is would also match the wrapped child this check distinguishes
		t.Fatal("classification mutated original children")
	}
	classified := tree.(interface{ Unwrap() []error }).Unwrap()
	if classified[0] != leaf || !errors.Is(tree, original) { //nolint:errorlint // identity assertion; errors.Is would also match the wrapped child this check distinguishes
		t.Fatal("classification lost joined identity")
	}
	classified[0] = errors.New("replacement")
	if children[0] != safe { //nolint:errorlint // identity assertion; errors.Is would also match the wrapped child this check distinguishes
		t.Fatal("classification aliases original child slice")
	}
}

type timeoutClassificationJoin struct{ children []error }

func (e *timeoutClassificationJoin) Error() string   { return errors.Join(e.children...).Error() }
func (e *timeoutClassificationJoin) Unwrap() []error { return e.children }
func (*timeoutClassificationJoin) Timeout() bool     { return true }

func TestClassificationTreePreservesJoinedWrapperType(t *testing.T) {
	safe := WithRedactor(errors.New("private-value"), NewRedactor("private-value"))
	wrapper := &timeoutClassificationJoin{children: []error{safe}}
	tree := classificationErrorTree(wrapper)
	var original *timeoutClassificationJoin
	if !errors.As(tree, &original) || original != wrapper {
		t.Fatal("classification lost joined wrapper type")
	}
	if !IsDefinitiveNonLivenessError(tree) {
		t.Fatal("classification lost joined wrapper timeout veto")
	}
}

type callerSinkError struct{ path string }

func (e *callerSinkError) Error() string { return "sink closed: " + e.path }

func TestSafeErrorExposesCallerSinkErrorThroughErrorsAs(t *testing.T) {
	sink := &callerSinkError{path: "/var/log/app.log"}
	raw := errors.Join(
		&CLIError{Binary: "docker", Args: []string{"logs"}, ExitCode: 1, Stderr: "failed"},
		fmt.Errorf("delivery: %w", &OutputError{Stream: "stdout", Err: sink}),
	)
	err := WithRedactor(raw, NewHashedContextRedactor("app"))

	var got *callerSinkError
	if !errors.As(err, &got) || got != sink {
		t.Fatalf("errors.As(sink) = %v, want the caller's writer error %v", got, sink)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 1 {
		t.Fatalf("errors.As(CLIError) = %v, want the redacted terminal clone", cliErr)
	}
}

func TestSafeErrorStillHidesNonSinkErrorsFromErrorsAs(t *testing.T) {
	err := WithRedactor(fmt.Errorf("probe: %w", &callerSinkError{path: "secret"}), NewRedactor())

	var got *callerSinkError
	if errors.As(err, &got) {
		t.Fatalf("errors.As exposed a raw error outside a sink failure: %v", got)
	}
}

func TestSafeErrorExposesOutputErrorThroughErrorsAs(t *testing.T) {
	sink := &callerSinkError{path: "/var/log/app.log"}
	raw := fmt.Errorf("delivery: %w", &OutputError{Stream: "stdout", Err: sink})
	err := WithRedactor(raw, NewHashedContextRedactor("app"))

	var outputErr *OutputError
	if !errors.As(err, &outputErr) {
		t.Fatal("errors.As(OutputError) = false, want the caller's sink wrapper")
	}
	if outputErr.Stream != "stdout" {
		t.Fatalf("Stream = %q, want stdout", outputErr.Stream)
	}
	var got *callerSinkError
	if !errors.As(outputErr, &got) || got != sink {
		t.Fatalf("errors.As(outputErr) = %v, want the raw caller sink error %v", got, sink)
	}
}
