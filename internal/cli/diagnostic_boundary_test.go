package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandErrorRedactsSecretSplitAtRawTailBoundary(t *testing.T) {
	const secret = "private-boundary-value"
	stderr := []byte("password=" + secret + strings.Repeat(" ", maxStderr-len(secret)))
	if len(stderr) <= maxStderr {
		t.Fatalf("test stderr = %d bytes, want more than %d", len(stderr), maxStderr)
	}

	err := commandError(context.Background(), "stub", "stub", []string{"exec"}, nil, stderr, &exec.ExitError{})
	if err == nil {
		t.Fatal("commandError returned nil for a failed command")
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %T %v, want *CLIError", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains a secret split at the raw tail boundary", err)
	}
	if len(cliErr.Stderr) > maxStderrContext {
		t.Fatalf("len(CLIError.Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderrContext)
	}
	if got := len(err.Error()); got > maxStderr+512 {
		t.Fatalf("rendered error = %d bytes, want at most %d", got, maxStderr+512)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %q, want a redaction marker", err)
	}
}

func TestCommandErrorRedactsPrivateKeyHeaderOutsideRetainedTail(t *testing.T) {
	const secret = "private-key-body-must-not-survive"
	key := "-----BEGIN RSA PRIVATE KEY-----\n" +
		secret + "\n" +
		strings.Repeat("A", maxStderr) + "\n" +
		"-----END RSA PRIVATE KEY-----"

	err := commandError(context.Background(), "stub", "stub", []string{"exec"}, nil, []byte(key), &exec.ExitError{})
	if err == nil {
		t.Fatal("commandError returned nil for a failed command")
	}
	for _, leaked := range []string{secret, "BEGIN RSA PRIVATE KEY"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error = %q, contains %q", err, leaked)
		}
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %q, want a redaction marker", err)
	}
}

func TestCommandErrorStderrRetainsLineStructure(t *testing.T) {
	stderr := []byte("password=secret-value\nError response from daemon: No such container: myctr\n")
	err := commandError(context.Background(), "docker", "docker", []string{"inspect", "myctr"}, nil, stderr, &exec.ExitError{})
	if err == nil {
		t.Fatal("commandError returned nil for a failed command")
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %T %v, want *CLIError", err, err)
	}
	if !strings.Contains(cliErr.Stderr, "\n") {
		t.Fatalf("CLIError.Stderr = %q, want original line structure", cliErr.Stderr)
	}
	if !strings.Contains(cliErr.Stderr, "Error response from daemon: No such container: myctr") {
		t.Fatalf("CLIError.Stderr = %q, want the classifier-visible diagnostic", cliErr.Stderr)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error = %q, contains a secret", err)
	}
}

func TestExecRunnerRunRedactsStructuralSecretsBeforeTruncation(t *testing.T) {
	const secret = "private-run-value"
	r := &ExecRunner{Binary: writeStub(t, `printf 'password=`+secret+`' >&2; head -c 131072 /dev/zero | tr '\000' ' ' >&2; exit 9`)}

	_, stderr, err := r.Run(context.Background(), "exec")
	if err == nil {
		t.Fatal("Run returned nil for a failed command")
	}
	if !strings.Contains(string(stderr), secret) {
		t.Fatalf("raw stderr = %d bytes, want the complete child stderr", len(stderr))
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains a secret", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %T %v, want *CLIError", err, err)
	}
	if cliErr.ExitCode != 9 {
		t.Fatalf("ExitCode = %d, want 9", cliErr.ExitCode)
	}
	if len(cliErr.Stderr) > maxStderrContext {
		t.Fatalf("len(CLIError.Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderrContext)
	}
}

func TestExecRunnerRunToRedactsStructuralSecretsBeforeTruncation(t *testing.T) {
	const secret = "private-runto-value"
	r := &ExecRunner{Binary: writeStub(t, `printf 'password=`+secret+`' >&2; head -c 131072 /dev/zero | tr '\000' ' ' >&2; exit 7`)}

	var sink bytes.Buffer
	stats, err := r.RunTo(context.Background(), io.Discard, &sink, "exec")
	if err == nil {
		t.Fatal("RunTo returned nil for a failed command")
	}
	if !strings.Contains(sink.String(), secret) {
		t.Fatalf("stderr sink lost the raw child output")
	}
	if stats.StderrBytes <= maxStderr {
		t.Fatalf("StderrBytes = %d, want the complete child stderr", stats.StderrBytes)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains a secret", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %T %v, want *CLIError", err, err)
	}
	if cliErr.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", cliErr.ExitCode)
	}
	if len(cliErr.Stderr) > maxStderrContext {
		t.Fatalf("len(CLIError.Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderrContext)
	}
}

func TestExecRunnerRunToRedactsPrivateKeyHeaderOutsideRetainedTail(t *testing.T) {
	const secret = "private-key-runto-body"
	r := &ExecRunner{Binary: writeStub(t, `printf -- '-----BEGIN RSA PRIVATE KEY-----\n`+secret+`\n' >&2; head -c 131072 /dev/zero | tr '\000' 'A' >&2; printf '\n-----END RSA PRIVATE KEY-----\n' >&2; exit 5`)}

	var sink bytes.Buffer
	_, err := r.RunTo(context.Background(), io.Discard, &sink, "exec")
	if err == nil {
		t.Fatal("RunTo returned nil for a failed command")
	}
	if !strings.Contains(sink.String(), secret) {
		t.Fatalf("stderr sink lost the raw child output")
	}
	for _, leaked := range []string{secret, "BEGIN RSA PRIVATE KEY"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error = %q, contains %q", err, leaked)
		}
	}
}

func TestStreamTerminalErrorRedactsStructuralSecretsBeforeTruncation(t *testing.T) {
	const secret = "private-stream-value"
	r := &ExecRunner{Binary: writeStub(t, `printf 'password=`+secret+`' >&2; head -c 131072 /dev/zero | tr '\000' ' ' >&2; exit 3`)}

	stream, err := r.Stream(context.Background(), "logs")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	data, readErr := io.ReadAll(stream)
	if closeErr := stream.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	if !strings.Contains(string(data), secret) {
		t.Fatalf("public stream lost the raw child output")
	}
	if readErr == nil {
		t.Fatal("terminal read error = nil, want a *CLIError")
	}
	if strings.Contains(readErr.Error(), secret) {
		t.Fatalf("read error = %q, contains a secret", readErr)
	}
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %T %v, want *CLIError", readErr, readErr)
	}
	if cliErr.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", cliErr.ExitCode)
	}
	if len(cliErr.Stderr) > maxStderrContext {
		t.Fatalf("len(CLIError.Stderr) = %d, want <= %d", len(cliErr.Stderr), maxStderrContext)
	}
}

func TestFormatCLIErrorKeepsDiagnosticTailWhenRedactorValueIsOversized(t *testing.T) {
	huge := strings.Repeat("x", MaxStreamOverlap+1)
	stderr := "opening diagnostic\n" + huge + "\n" + strings.Repeat("p", maxStderr) + "\nclosing diagnostic"
	got := formatCLIError(&CLIError{Stderr: stderr}, NewRedactor(huge))
	if strings.Contains(got, huge) {
		t.Fatal("formatted error contains the oversized redactor value")
	}
	if !strings.Contains(got, "closing diagnostic") {
		t.Fatalf("formatted error lost the trailing diagnostic: %q", tailOf(got, 200))
	}
	if strings.Contains(got, "opening diagnostic") {
		t.Fatalf("formatted error retained more than the trailing window: %q", tailOf(got, 200))
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
