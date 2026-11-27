package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestExecWithExecStderrSeparatesStreams(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "stdout-part\n"}
	ctr := runTestContainer(t, f)

	var stderr bytes.Buffer
	code, out, err := ctr.Exec(context.Background(), []string{"psql", "-c", "select 1"}, WithExecStderr(&stderr))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	data, _ := io.ReadAll(out)
	if string(data) != "stdout-part\n" {
		t.Errorf("stdout = %q, want only stdout-part", data)
	}
	if stderr.String() != "stderr-part" {
		t.Errorf("stderr = %q, want stderr-part", stderr.String())
	}
}

func TestExecWithoutExecStderrKeepsCombinedOutput(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "stdout-part\n"}
	ctr := runTestContainer(t, f)

	_, out, err := ctr.Exec(context.Background(), []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, _ := io.ReadAll(out)
	if string(data) != "stdout-part\nstderr-part" {
		t.Errorf("combined output = %q", data)
	}
}

func TestExecToWithExecStderrSeparatesStreams(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "stdout-part\n"}
	ctr := runTestContainer(t, f)

	var stdout, stderr bytes.Buffer
	code, stats, err := ctr.ExecTo(context.Background(), []string{"echo", "hello"}, &stdout, WithExecStderr(&stderr))
	if err != nil {
		t.Fatalf("ExecTo: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.String() != "stdout-part\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	if stderr.String() != "stderr-part" {
		t.Errorf("stderr = %q", stderr.String())
	}
	if want := int64(len("stdout-part\n") + len("stderr-part")); stats.Bytes != want {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, want)
	}
}

func TestExecWithExecStderrMaxBytesBoundsStdoutOnly(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "abcdef"}
	ctr := runTestContainer(t, f)

	var stderr bytes.Buffer
	_, out, err := ctr.Exec(context.Background(), []string{"echo", "hello"},
		WithExecMaxBytes(3), WithExecStderr(&stderr))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, readErr := io.ReadAll(out)
	if string(data) != "abc" {
		t.Errorf("stdout = %q, want abc", data)
	}
	if !errors.Is(readErr, ErrOutputTruncated) {
		t.Errorf("read error = %v, want ErrOutputTruncated", readErr)
	}
	if stderr.String() != "stderr-part" {
		t.Errorf("stderr = %q, want unbounded stderr", stderr.String())
	}
}

func TestExecWithExecStderrReportsWriterFailure(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "stdout-part\n"}
	ctr := runTestContainer(t, f)

	writeErr := errors.New("stderr sink failed")
	_, _, err := ctr.Exec(context.Background(), []string{"echo", "hello"},
		WithExecStderr(&failingOutputWriter{err: writeErr}))
	if !errors.Is(err, writeErr) || !errors.Is(err, ErrOutputDelivery) {
		t.Fatalf("error = %v, want writer failure and ErrOutputDelivery", err)
	}
}

func TestWithExecStderrRejectsNilWriter(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	_, _, err := ctr.Exec(context.Background(), []string{"echo", "hello"}, WithExecStderr(nil))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if !strings.Contains(err.Error(), "stderr writer cannot be nil") {
		t.Fatalf("error = %v, want nil-writer validation", err)
	}
}

type overlapDetectingWriter struct {
	mu      sync.Mutex
	active  int
	overlap bool
	data    bytes.Buffer
}

func (w *overlapDetectingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.active++
	if w.active > 1 {
		w.overlap = true
	}
	w.mu.Unlock()

	time.Sleep(time.Millisecond)

	w.mu.Lock()
	defer w.mu.Unlock()
	w.active--
	return w.data.Write(p)
}

func (w *overlapDetectingWriter) overlapped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overlap
}

func (w *overlapDetectingWriter) output() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

type concurrentExecWriterRunner struct {
	*fakeRunner
	stdout string
	stderr string
}

func (r *concurrentExecWriterRunner) RunTo(ctx context.Context, stdout, stderr io.Writer, args ...string) (cli.OutputStats, error) {
	if len(args) == 0 || args[0] != "exec" {
		return cli.RunTo(r.fakeRunner, ctx, stdout, stderr, args...)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = stdout.Write([]byte(r.stdout))
	}()
	go func() {
		defer wg.Done()
		_, _ = stderr.Write([]byte(r.stderr))
	}()
	wg.Wait()
	return cli.OutputStats{
		StdoutBytes: int64(len(r.stdout)),
		StderrBytes: int64(len(r.stderr)),
	}, nil
}

func TestExecToSameWriterForStdoutAndStderrIsSerialized(t *testing.T) {
	f := &concurrentExecWriterRunner{fakeRunner: newTestRunner(), stdout: "stdout-part", stderr: "stderr-part"}
	ctr := runTestContainer(t, f)

	shared := &overlapDetectingWriter{}
	code, _, err := ctr.ExecTo(context.Background(), []string{"echo", "hello"}, shared, WithExecStderr(shared))
	if err != nil {
		t.Fatalf("ExecTo: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if shared.overlapped() {
		t.Fatal("stdout and stderr wrote to the shared writer concurrently")
	}
	if got := shared.output(); got != "stdout-partstderr-part" && got != "stderr-partstdout-part" {
		t.Errorf("shared writer output = %q", got)
	}
}

func TestExecToRejectsNilWriter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output io.Writer
	}{
		{name: "nil", output: nil},
		{name: "typed nil", output: (*bytes.Buffer)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil
			_, _, err := ctr.ExecTo(context.Background(), []string{"true"}, tc.output)
			assertPublicValidationError(t, err, "ExecTo", "ExecTo", tc.output,
				"output writer cannot be nil")
			if len(f.calls) != 0 {
				t.Fatalf("backend was called for a nil writer: %v", f.calls)
			}
		})
	}
}
