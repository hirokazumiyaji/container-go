package cli

import (
	"errors"
	"strings"
	"testing"
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
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("redacted CLIError.Error() = %q, contains secret", got.Error())
	}
	if !strings.Contains(got.RawError(), secret) {
		t.Fatalf("RawError() = %q, want raw value", got.RawError())
	}
	if !strings.Contains(raw.RawError(), secret) {
		t.Fatalf("original RawError() = %q, want raw value", raw.RawError())
	}
}
