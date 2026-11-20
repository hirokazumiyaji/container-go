package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func boundaryStderrScript(secret string, exit int) string {
	return "head -c " + strconv.Itoa(maxStderr) + " /dev/zero | tr '\\000' ' ' >&2; " +
		"printf 'password=" + secret + "' >&2; " +
		"head -c " + strconv.Itoa(maxStderr-len(secret)) + " /dev/zero | tr '\\000' ' ' >&2; " +
		"exit " + strconv.Itoa(exit)
}

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
	const secret = "zq-run-boundary-934"
	r := &ExecRunner{Binary: writeStub(t, boundaryStderrScript(secret, 9))}

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
	const secret = "zq-runto-boundary-934"
	r := &ExecRunner{Binary: writeStub(t, boundaryStderrScript(secret, 7))}

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
	const secret = "zq-pem-runto-body-934"
	r := &ExecRunner{Binary: writeStub(t, `printf -- '-----BEGIN RSA PRIVATE KEY-----\n' >&2; head -c 102400 /dev/zero | tr '\000' 'A' >&2; printf '\n`+secret+`\n-----END RSA PRIVATE KEY-----\n' >&2; exit 5`)}

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
	const secret = "zq-stream-boundary-934"
	r := &ExecRunner{Binary: writeStub(t, boundaryStderrScript(secret, 3))}

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

func TestSafeErrorRedactsCallerValueSplitAtRenderedTailBoundary(t *testing.T) {
	const secret = "caller-boundary-secret"
	stderr := strings.Repeat("p", maxStderr-len(secret)/2) + secret + strings.Repeat("q", 1024)
	raw := &CLIError{Stderr: stderr, ExitCode: 1}

	safe := WithRedactor(raw, NewContextRedactor(secret))
	if strings.Contains(safe.Error(), secret) {
		t.Fatalf("safe error = %q, contains a caller value split at the rendered boundary", tailOf(safe.Error(), 200))
	}

	joined := errors.Join(fmt.Errorf("context: %w", raw), errors.New("sibling diagnostic"))
	safeJoin := WithRedactor(joined, NewContextRedactor(secret))
	if strings.Contains(safeJoin.Error(), secret) {
		t.Fatalf("joined safe error = %q, contains a caller value split at the rendered boundary", tailOf(safeJoin.Error(), 200))
	}
	if !strings.Contains(safeJoin.Error(), "sibling diagnostic") {
		t.Fatalf("joined safe error lost a non-CLI diagnostic: %q", tailOf(safeJoin.Error(), 200))
	}

	wrapped := fmt.Errorf("runner: %w", raw)
	safeWrapped := WithRedactor(wrapped, NewContextRedactor(secret))
	if strings.Contains(safeWrapped.Error(), secret) {
		t.Fatalf("wrapped safe error = %q, contains a caller value split at the rendered boundary", tailOf(safeWrapped.Error(), 200))
	}
	if !strings.Contains(safeWrapped.Error(), "runner:") {
		t.Fatalf("wrapped safe error lost the wrapper context: %q", tailOf(safeWrapped.Error(), 200))
	}
}

func TestCommandErrorDropsPartialLeadingLineAtRetainedWindowBoundary(t *testing.T) {
	const secret = "partial-leading-secret"
	// Put the window cut inside the password value so the prefix is gone.
	stderr := strings.Repeat("f", maxStderrContext-5) + "password=" + secret + "\nError response from daemon: No such container: myctr\n"
	if len(stderr) <= maxStderrContext {
		t.Fatalf("test stderr = %d bytes, want more than %d", len(stderr), maxStderrContext)
	}

	err := commandError(context.Background(), "docker", "docker", []string{"inspect", "myctr"}, nil, []byte(stderr), &exec.ExitError{})
	if err == nil {
		t.Fatal("commandError returned nil for a failed command")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, contains a value with a discarded prefix", err)
	}
	if !strings.Contains(err.Error(), "No such container: myctr") {
		t.Fatalf("error = %q, lost the diagnostic line after the discarded prefix", err)
	}
}

func TestFormatCLIErrorRedactsOrphanedPEMEnd(t *testing.T) {
	const secret = "orphaned-key-body"
	stderr := secret + "\n" + strings.Repeat("A", maxStderr) + "\n-----END RSA PRIVATE KEY-----\nError response from daemon: No such container: myctr\n"

	got := formatCLIError(&CLIError{Stderr: stderr}, NewRedactor())
	for _, leaked := range []string{secret, strings.Repeat("A", 32), "END RSA PRIVATE KEY"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("formatted error contains %q", leaked)
		}
	}
	if !strings.Contains(got, "No such container: myctr") {
		t.Fatalf("formatted error lost the diagnostic after the orphaned key: %q", tailOf(got, 200))
	}
}

func TestTailBufferDropsPartialLeadingLineAfterOverflow(t *testing.T) {
	buffer := newTailBuffer(maxStderr)
	if _, err := buffer.Write([]byte("partial-secret" + strings.Repeat("x", maxStderr+10))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := buffer.Write([]byte("\nnext diagnostic\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buffer.String()
	if strings.Contains(got, "partial-secret") {
		t.Fatalf("tail buffer = %q, retained a value with a discarded prefix", tailOf(got, 200))
	}
	if !strings.Contains(got, "next diagnostic") {
		t.Fatalf("tail buffer = %q, lost the diagnostic line after the discarded prefix", tailOf(got, 200))
	}
}

func TestCommandErrorRedactsPEMBodySplitAtRetainedWindowBoundary(t *testing.T) {
	const secret = "pem-split-body-marker"
	body := strings.Repeat(secret+"\n", 2*maxStderrContext/len(secret+"\n")+1)
	stderr := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "-----END RSA PRIVATE KEY-----\nError response from daemon: No such container: myctr\n"
	if len(stderr) <= maxStderrContext {
		t.Fatalf("test stderr = %d bytes, want more than %d", len(stderr), maxStderrContext)
	}

	err := commandError(context.Background(), "docker", "docker", []string{"inspect", "myctr"}, nil, []byte(stderr), &exec.ExitError{})
	if err == nil {
		t.Fatal("commandError returned nil for a failed command")
	}
	for _, leaked := range []string{secret, "BEGIN RSA PRIVATE KEY", "END RSA PRIVATE KEY"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error = %q, contains %q", err, leaked)
		}
	}
	if !strings.Contains(err.Error(), "No such container: myctr") {
		t.Fatalf("error = %q, lost the diagnostic after the split key", err)
	}
}

func TestErrorsAsDoesNotExposeOrphanedPEMBody(t *testing.T) {
	const body = "orphan-pem-body-line-934"
	raw := &CLIError{
		Binary:   "docker",
		Args:     []string{"inspect", "myctr"},
		ExitCode: 1,
		Stderr:   body + "\n" + strings.Repeat("A", maxStderr) + "\n-----END RSA PRIVATE KEY-----\nError response from daemon: No such container: myctr\n",
	}
	safe := WithRedactor(raw, NewRedactor())

	var got *CLIError
	if !errors.As(safe, &got) {
		t.Fatal("errors.As did not find *CLIError")
	}
	if strings.Contains(got.Stderr, body) {
		t.Fatalf("errors.As CLIError.Stderr = %q, contains an orphaned PEM body", tailOf(got.Stderr, 200))
	}
	if !strings.Contains(got.Stderr, "No such container: myctr") {
		t.Fatalf("errors.As CLIError.Stderr = %q, lost the diagnostic after the orphaned key", tailOf(got.Stderr, 200))
	}
}
