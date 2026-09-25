package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
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
