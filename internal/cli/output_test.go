package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type cappedTestWriter struct {
	data      bytes.Buffer
	max       int
	truncated bool
}

func (w *cappedTestWriter) Write(p []byte) (int, error) {
	if room := w.max - w.data.Len(); room > 0 {
		keep := len(p)
		if keep > room {
			keep = room
		}
		_, _ = w.data.Write(p[:keep])
		if len(p) > keep {
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *cappedTestWriter) Truncated() bool { return w.truncated }

type legacyOutputRunner struct {
	stdout []byte
	stderr []byte
}

func (r *legacyOutputRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	return r.stdout, r.stderr, nil
}

func TestRunToAdaptsLegacyRunner(t *testing.T) {
	runner := &legacyOutputRunner{stdout: []byte("out-data"), stderr: []byte("err-data")}
	stdout := &cappedTestWriter{max: 3}
	stderr := &cappedTestWriter{max: 3}

	stats, err := RunTo(runner, context.Background(), stdout, stderr, "logs")
	if err != nil {
		t.Fatalf("RunTo: %v", err)
	}
	if stdout.data.String() != "out" || stderr.data.String() != "err" {
		t.Fatalf("retained output = %q/%q, want out/err", stdout.data.String(), stderr.data.String())
	}
	if stats.StdoutBytes != 8 || stats.StderrBytes != 8 || !stats.Truncated() {
		t.Fatalf("stats = %+v, want full byte counts and truncation", stats)
	}
}

func TestExecRunnerRunToWritesLargeOutputDirectlyToBoundedSinks(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `head -c 4194304 /dev/zero; head -c 4194304 /dev/zero >&2`)}
	stdout := &cappedTestWriter{max: 8}
	stderr := &cappedTestWriter{max: 8}

	stats, err := r.RunTo(context.Background(), stdout, stderr, "logs", "id")
	if err != nil {
		t.Fatalf("RunTo: %v", err)
	}
	if stdout.data.Len() != 8 || stderr.data.Len() != 8 {
		t.Fatalf("retained stdout/stderr = %d/%d, want 8/8", stdout.data.Len(), stderr.data.Len())
	}
	if !stats.Truncated() || !stdout.truncated || !stderr.truncated {
		t.Fatalf("truncation not reported: stats=%+v stdout=%v stderr=%v", stats, stdout.truncated, stderr.truncated)
	}
	if stats.StdoutBytes != 4194304 || stats.StderrBytes != 4194304 {
		t.Fatalf("observed bytes = %d/%d, want 4194304/4194304", stats.StdoutBytes, stats.StderrBytes)
	}
}

func TestExecRunnerRunToKeepsFailureDiagnosticBounded(t *testing.T) {
	r := &ExecRunner{Binary: writeStub(t, `head -c 131072 /dev/zero >&2; exit 7`)}

	_, err := r.RunTo(context.Background(), io.Discard, io.Discard, "exec")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if cliErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cliErr.ExitCode)
	}
	if len(cliErr.Stderr) != maxStderr {
		t.Errorf("len(CLIError.Stderr) = %d, want %d", len(cliErr.Stderr), maxStderr)
	}
}
