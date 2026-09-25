package container

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunCLIErrorUsesRedactedArgsAndStderr(t *testing.T) {
	const (
		envSecret    = "run-env-password"
		commandToken = "run-command-token"
		labelSecret  = "run-label-password"
		stderrToken  = "run-stderr-token"
	)
	f := newTestRunner()
	f.failPrefix = "run"
	f.failStderr = "password=" + envSecret + " token=" + stderrToken + "\x1b[31m\r\n"

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(f), withEngine(appleEngine{}),
		WithEnv(map[string]string{"PASSWORD": envSecret}),
		WithCmd("server", commandToken),
		WithLabels(map[string]string{"com.example.password": labelSecret}),
	)
	if err == nil {
		t.Fatal("want CLI error")
	}
	got := err.Error()
	for _, secret := range []string{envSecret, commandToken, labelSecret, stderrToken} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains secret %q", got, secret)
		}
	}
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("error = %q, contains terminal control characters", got)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(cliErr.Error(), commandToken) {
		t.Errorf("errors.As CLIError.Error() = %q, contains command secret", cliErr.Error())
	}
	if !strings.Contains(cliErr.RawError(), commandToken) {
		t.Errorf("RawError() = %q, want explicit raw command", cliErr.RawError())
	}
}

func TestRunWaitDiagnosticsRedactCallerSecretsAndControlCharacters(t *testing.T) {
	const (
		envSecret    = "env-password-value"
		commandToken = "cmd-token-value"
		labelSecret  = "label-secret-value"
		stderrSecret = "stderr-secret-value"
	)
	f := newTestRunner()
	logs := &stdoutRunner{
		fakeRunner: f,
		stdout: "starting\n" +
			"PASSWORD=" + envSecret + "\n" +
			"token=" + commandToken + "\x1b[31mred\x1b[0m\r\n" +
			"stderr=" + stderrSecret + "\n" +
			"label=" + labelSecret,
	}
	strategy := &recordingStrategy{err: errors.New("not ready\x1b[2J\r\n")}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(logs), withEngine(appleEngine{}),
		WithEnv(map[string]string{"PASSWORD": envSecret}),
		WithCmd("server", commandToken),
		WithLabels(map[string]string{"com.example.token": labelSecret}),
		WithWaitStrategy(strategy),
	)
	if err == nil {
		t.Fatal("want wait error")
	}
	got := err.Error()
	for _, secret := range []string{envSecret, commandToken, labelSecret, stderrSecret} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains secret %q", got, secret)
		}
	}
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("error = %q, contains terminal control characters", got)
	}
	if !strings.Contains(got, "container logs") {
		t.Errorf("error = %q, want redacted log tail", got)
	}
}
