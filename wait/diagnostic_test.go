package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestForLogDiagnosticRedactsSecretsAndControlCharacters(t *testing.T) {
	const (
		password = "wait-password-value"
		token    = "wait-token-value"
	)
	for _, pattern := range []string{
		"password=" + password + "\x1b[31m\r\n",
		"token=" + token + "\x1b[0m\r\n",
	} {
		t.Run(pattern, func(t *testing.T) {
			target := newFakeTarget()
			err := ForLog(pattern).WithStartupTimeout(50*time.Millisecond).WithPollInterval(5*time.Millisecond).WaitUntilReady(context.Background(), target)
			if err == nil {
				t.Fatal("want timeout error")
			}
			got := err.Error()
			if strings.Contains(got, password) || strings.Contains(got, token) {
				t.Errorf("error = %q, contains a secret", got)
			}
			if strings.ContainsAny(got, "\x1b\r\n") {
				t.Errorf("error = %q, contains terminal controls", got)
			}
		})
	}
}

func TestForExecDiagnosticRedactsBareCommandSecrets(t *testing.T) {
	const secret = "bare-command-token-value"
	target := newFakeTarget()
	target.execErr = errors.New("backend rejected command")
	err := ForExec([]string{"tool", secret}).WithStartupTimeout(50*time.Millisecond).WithPollInterval(5*time.Millisecond).WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want exec timeout error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains command secret", err)
	}
}

func TestForLogDiagnosticSanitizesFollowError(t *testing.T) {
	target := &errorTarget{err: errors.New("stderr=raw-secret\x1b[31m\r\n")}
	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want follow error")
	}
	if strings.Contains(err.Error(), "raw-secret") || strings.ContainsAny(err.Error(), "\x1b\r\n") {
		t.Fatalf("error = %q, want sanitized follow error", err)
	}
}

type errorTarget struct {
	err error
}

func (t *errorTarget) Endpoint(context.Context, string) (string, error) { return "", t.err }
func (t *errorTarget) Running(context.Context) (bool, error)            { return true, t.err }
func (t *errorTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return nil, t.err
}
func (t *errorTarget) ExecCommand(context.Context, []string) (int, error) { return 0, t.err }

func TestDiagnosticValuesCarryCompleteStrategyContext(t *testing.T) {
	const (
		path     = "/private-health"
		port     = "8443/tcp"
		header   = "private-header-value"
		user     = "private-user"
		password = "private-password"
		pattern  = "private-log-pattern"
		command  = "private-command-value"
	)
	strategy := ForAll(
		ForHTTP(path).WithPort(port).WithHeader("X-Private", header).WithBasicAuth(user, password),
		ForLog(pattern),
		ForExec([]string{"tool", command}),
		ForListeningPort("9443/tcp"),
	)
	values := DiagnosticValues(strategy)
	joined := strings.Join(values, "\x00")
	for _, value := range []string{path, port, header, user, password, pattern, command, "9443/tcp", "cHJpdmF0ZS11c2VyOnByaXZhdGUtcGFzc3dvcmQ"} {
		if !strings.Contains(joined, value) {
			t.Errorf("DiagnosticValues() = %v, missing %q", values, value)
		}
	}
}

func TestHTTPWaitErrorRedactsCLIErrorFromErrorsAs(t *testing.T) {
	const secret = "wait-cli-error-secret"
	raw := &cli.CLIError{Args: []string{"probe", secret}, Stderr: secret, ExitCode: 1}
	err := ForHTTP("/health").WithStartupTimeout(20*time.Millisecond).WithPollInterval(time.Millisecond).WaitUntilReady(context.Background(), &errorTarget{err: raw})
	if err == nil {
		t.Fatal("want wait error")
	}
	var got *cli.CLIError
	if !errors.As(err, &got) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(got.Error(), secret) || strings.Contains(got.Stderr, secret) {
		t.Fatalf("errors.As CLIError is unsafe: %q", got.Error())
	}
	if !strings.Contains(got.RawError(), secret) {
		t.Fatalf("RawError() = %q, want raw value", got.RawError())
	}
}

func TestHTTPWaitErrorUsesHeaderAndBasicAuthContext(t *testing.T) {
	const (
		path     = "/private-health"
		header   = "private-header-value"
		password = "private-password"
	)
	target := newFakeTarget()
	target.endpoint = "bad endpoint"
	err := ForHTTP(path).
		WithHeader("X-Private", header).
		WithBasicAuth("user", password).
		WithStartupTimeout(20*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want HTTP wait error")
	}
	got := err.Error()
	for _, secret := range []string{path, header, password} {
		if strings.Contains(got, secret) {
			t.Errorf("error = %q, contains %q", got, secret)
		}
	}
}
