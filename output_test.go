package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type outputTestRunner struct {
	*fakeRunner
	output     string
	err        error
	inspectErr error
	runCalls   int
	runToCalls int
}

func (r *outputTestRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.runCalls++
	if len(args) > 0 && r.inspectErr != nil && args[0] == "inspect" {
		return nil, nil, r.inspectErr
	}
	if len(args) > 0 && (args[0] == "exec" || args[0] == "logs") {
		return []byte(r.output), []byte(r.output), r.err
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *outputTestRunner) RunTo(_ context.Context, stdout, stderr io.Writer, _ ...string) (cli.OutputStats, error) {
	r.runToCalls++
	data := []byte(r.output)
	if _, err := stdout.Write(data); err != nil {
		return cli.OutputStats{}, err
	}
	if _, err := stderr.Write(data); err != nil {
		return cli.OutputStats{}, err
	}
	return cli.OutputStats{
		StdoutBytes:     int64(len(data)),
		StderrBytes:     int64(len(data)),
		StdoutTruncated: testTruncation(stdout),
		StderrTruncated: testTruncation(stderr),
	}, r.err
}

func testTruncation(w io.Writer) bool {
	type reporter interface{ Truncated() bool }
	r, ok := w.(reporter)
	return ok && r.Truncated()
}

type joinedOutputRunner struct {
	*fakeRunner
	stdout       string
	stderr       string
	terminal     error
	delivery     error
	hideDelivery bool
}

func (r *joinedOutputRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return r.fakeRunner.Run(ctx, args...)
}

func (r *joinedOutputRunner) RunTo(_ context.Context, stdout, stderr io.Writer, _ ...string) (cli.OutputStats, error) {
	stdoutData := []byte(r.stdout)
	stderrData := []byte(r.stderr)
	_, _ = stdout.Write(stdoutData)
	_, _ = stderr.Write(stderrData)
	delivery := r.delivery
	if r.hideDelivery {
		delivery = nil
	}
	return cli.OutputStats{
		StdoutBytes: int64(len(stdoutData)),
		StderrBytes: int64(len(stderrData)),
	}, errors.Join(r.terminal, delivery)
}

type failingOutputWriter struct {
	err error
}

func (w *failingOutputWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func resetOutputTestCalls(r *outputTestRunner) {
	r.runCalls = 0
	r.runToCalls = 0
}

func TestLogsMaxBytesCapsAndReportsTruncation(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("x", 4096) + "END",
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{MaxBytes: 8})
	if err != nil {
		t.Fatalf("LogsWithOptions: %v", err)
	}
	defer rc.Close()
	data, readErr := io.ReadAll(rc)
	if len(data) != 8 {
		t.Fatalf("retained bytes = %d, want 8", len(data))
	}
	if !errors.Is(readErr, ErrOutputTruncated) {
		t.Fatalf("read error = %v, want ErrOutputTruncated", readErr)
	}
	reporter, ok := rc.(TruncationReporter)
	if !ok || !reporter.Truncated() {
		t.Fatalf("reader does not report truncation: %T", rc)
	}
	if f.runCalls != 0 || f.runToCalls != 1 {
		t.Fatalf("calls = Run:%d RunTo:%d, want 0/1", f.runCalls, f.runToCalls)
	}
}

func TestBoundedCaptureRetainsPrefixNotTail(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     "HEAD-marker-TAIL-marker",
	}
	ctr := runTestContainer(t, f)

	rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{MaxBytes: 4})
	if err != nil {
		t.Fatalf("LogsWithOptions: %v", err)
	}
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "HEAD" {
		t.Fatalf("retained data = %q, want prefix HEAD", data)
	}
	if !errors.Is(readErr, ErrOutputTruncated) {
		t.Fatalf("read error = %v, want ErrOutputTruncated", readErr)
	}
}

func TestExecMaxBytesCapsAndReportsTruncation(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("y", 4096) + "END",
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	code, output, err := ctr.Exec(context.Background(), []string{"true"}, WithExecMaxBytes(8))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	data, readErr := io.ReadAll(output)
	if len(data) != 8 {
		t.Fatalf("retained bytes = %d, want 8", len(data))
	}
	if !errors.Is(readErr, ErrOutputTruncated) {
		t.Fatalf("read error = %v, want ErrOutputTruncated", readErr)
	}
	reporter, ok := output.(TruncationReporter)
	if !ok || !reporter.Truncated() {
		t.Fatalf("reader does not report truncation: %T", output)
	}
	if f.runCalls != 0 || f.runToCalls != 1 {
		t.Fatalf("calls = Run:%d RunTo:%d, want 0/1", f.runCalls, f.runToCalls)
	}
}

func TestExecToStreamsAndReportsTotalBytes(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("z", 4096),
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	var output bytes.Buffer
	code, stats, err := ctr.ExecTo(context.Background(), []string{"true"}, &output, WithExecMaxBytes(8))
	if err != nil {
		t.Fatalf("ExecTo: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if output.Len() != 8 {
		t.Fatalf("forwarded bytes = %d, want 8", output.Len())
	}
	if stats.Bytes != 8192 || !stats.Truncated {
		t.Fatalf("stats = %+v, want 8192 bytes and truncation", stats)
	}
	if f.runCalls != 0 || f.runToCalls != 1 {
		t.Fatalf("calls = Run:%d RunTo:%d, want 0/1", f.runCalls, f.runToCalls)
	}
}

func TestExecToPreservesCommandExitResult(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     "command output",
		err:        &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "command failed"},
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	var output bytes.Buffer
	code, _, err := ctr.ExecTo(context.Background(), []string{"false"}, &output)
	if err != nil {
		t.Fatalf("ExecTo: %v", err)
	}
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
	if output.String() != "command outputcommand output" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestExecToPreservesWriterAndTerminalErrors(t *testing.T) {
	deliveryErr := errors.New("writer rejected output")
	terminal := &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "application failed"}
	f := &joinedOutputRunner{
		fakeRunner: newTestRunner(),
		stdout:     "stdout",
		stderr:     "stderr",
		terminal:   terminal,
		delivery:   &cli.OutputError{Stream: "stdout", Err: deliveryErr},
	}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.ExecTo(context.Background(), []string{"false"}, &failingOutputWriter{err: deliveryErr})
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}
	if !errors.Is(err, deliveryErr) || !errors.Is(err, ErrOutputDelivery) {
		t.Fatalf("error = %v, want writer and output-delivery errors", err)
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want terminal CLI exit 7", err)
	}
}

func TestExecToPreservesExitCodeOnInfrastructureError(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     "diagnostic",
		err: &cli.CLIError{
			Args:     []string{"exec"},
			ExitCode: 19,
			Stderr:   "daemon unavailable",
		},
		inspectErr: errors.New("inspect failed"),
	}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.ExecTo(context.Background(), []string{"true"}, io.Discard)
	if code != 19 {
		t.Fatalf("code = %d, want 19", code)
	}
	if err == nil {
		t.Fatal("want infrastructure error")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 19 {
		t.Fatalf("error = %v, want CLI exit 19 in classified chain", err)
	}
}

func TestExecToDoesNotHideWriterErrorOnSuccess(t *testing.T) {
	deliveryErr := errors.New("writer rejected successful output")
	f := &joinedOutputRunner{
		fakeRunner:   newTestRunner(),
		stdout:       "stdout",
		stderr:       "stderr",
		delivery:     &cli.OutputError{Stream: "stdout", Err: deliveryErr},
		hideDelivery: true,
	}
	ctr := runTestContainer(t, f)

	code, stats, err := ctr.ExecTo(context.Background(), []string{"true"}, &failingOutputWriter{err: deliveryErr}, WithExecMaxBytes(8))
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !errors.Is(err, deliveryErr) {
		t.Fatalf("error = %v, want writer error", err)
	}
	if stats.Truncated {
		t.Fatalf("stats = %+v, sink failure must not masquerade as limit truncation", stats)
	}
}

func TestLogsToStreamsAndReportsTotalBytes(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("l", 4096),
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	var output bytes.Buffer
	stats, err := ctr.LogsTo(context.Background(), &output, LogsOptions{MaxBytes: 8})
	if err != nil {
		t.Fatalf("LogsTo: %v", err)
	}
	if output.Len() != 8 {
		t.Fatalf("forwarded bytes = %d, want 8", output.Len())
	}
	if stats.Bytes != 8192 || !stats.Truncated {
		t.Fatalf("stats = %+v, want 8192 bytes and truncation", stats)
	}
	if f.runCalls != 0 || f.runToCalls != 1 {
		t.Fatalf("calls = Run:%d RunTo:%d, want 0/1", f.runCalls, f.runToCalls)
	}
}

func TestLogsToPreservesWriterAndTerminalErrors(t *testing.T) {
	deliveryErr := errors.New("writer rejected log output")
	terminal := &cli.CLIError{Args: []string{"logs"}, ExitCode: 9, Stderr: "logs failed"}
	f := &joinedOutputRunner{
		fakeRunner: newTestRunner(),
		stdout:     "logs",
		stderr:     "details",
		terminal:   terminal,
		delivery:   deliveryErr,
	}
	ctr := runTestContainer(t, f)

	_, err := ctr.LogsTo(context.Background(), &failingOutputWriter{err: deliveryErr}, LogsOptions{})
	if !errors.Is(err, deliveryErr) {
		t.Fatalf("error = %v, want writer error", err)
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 9 {
		t.Fatalf("error = %v, want terminal CLI exit 9", err)
	}
}

func TestLogsToDoesNotHideWriterErrorOnSuccess(t *testing.T) {
	deliveryErr := errors.New("writer rejected successful log output")
	f := &joinedOutputRunner{
		fakeRunner:   newTestRunner(),
		stdout:       "logs",
		stderr:       "details",
		delivery:     &cli.OutputError{Stream: "stderr", Err: deliveryErr},
		hideDelivery: true,
	}
	ctr := runTestContainer(t, f)

	_, err := ctr.LogsTo(context.Background(), &failingOutputWriter{err: deliveryErr}, LogsOptions{})
	if !errors.Is(err, deliveryErr) || !errors.Is(err, ErrOutputDelivery) {
		t.Fatalf("error = %v, want writer and output-delivery errors", err)
	}
}

func assertBoundedPartial(t *testing.T, reader io.Reader) {
	t.Helper()
	if reader == nil {
		t.Fatal("partial output reader is nil")
	}
	data, err := io.ReadAll(reader)
	if len(data) != 8 {
		t.Fatalf("retained bytes = %d, want 8", len(data))
	}
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("read error = %v, want ErrOutputTruncated", err)
	}
	reporter, ok := reader.(TruncationReporter)
	if !ok || !reporter.Truncated() {
		t.Fatalf("reader does not report truncation: %T", reader)
	}
}

func TestExecBoundedReturnsPartialOnInfrastructureError(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("i", 32),
		err:        errors.New("backend unavailable"),
	}
	ctr := runTestContainer(t, f)

	code, output, err := ctr.Exec(context.Background(), []string{"true"}, WithExecMaxBytes(8))
	if err == nil {
		t.Fatal("want infrastructure error")
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	assertBoundedPartial(t, output)
}

func TestExecReturnsPartialOnInfrastructureError(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     "partial diagnostic",
		err:        errors.New("backend unavailable"),
	}
	ctr := runTestContainer(t, f)

	_, output, err := ctr.Exec(context.Background(), []string{"true"})
	if err == nil || output == nil {
		t.Fatalf("error/output = %v/%v, want error and partial output", err, output)
	}
	data, readErr := io.ReadAll(output)
	if readErr != nil || string(data) != f.output+f.output {
		t.Fatalf("partial output = %q/%v, want returned stdout and stderr", data, readErr)
	}
}

func TestExecBoundedReturnsPartialOnLaunchError(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("p", 32),
		err:        &exec.Error{Name: "container", Err: errors.New("executable not found")},
	}
	ctr := runTestContainer(t, f)

	code, output, err := ctr.Exec(context.Background(), []string{"true"}, WithExecMaxBytes(8))
	if code != 0 || err == nil {
		t.Fatalf("code/error = %d/%v, want launch error", code, err)
	}
	assertBoundedPartial(t, output)
}

func TestExecBoundedReturnsPartialAndExitCodeOnMissingContainer(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("m", 32),
		err: &cli.CLIError{
			Args:     []string{"exec"},
			ExitCode: 23,
			Stderr:   `not found: "myctr"`,
		},
		inspectErr: &cli.CLIError{
			Args:     []string{"inspect", "myctr"},
			ExitCode: 1,
			Stderr:   `not found: "myctr"`,
		},
	}
	ctr := runTestContainer(t, f)

	code, output, err := ctr.Exec(context.Background(), []string{"true"}, WithExecMaxBytes(8))
	if err == nil || !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
	if code != 23 {
		t.Fatalf("code = %d, want 23", code)
	}
	assertBoundedPartial(t, output)
}

func TestExecBoundedReturnsPartialOnCancellation(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("c", 32),
		err:        context.Canceled,
	}
	ctr := runTestContainer(t, f)

	code, output, err := ctr.Exec(context.Background(), []string{"true"}, WithExecMaxBytes(8))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	assertBoundedPartial(t, output)
}

func TestLogsReturnsPartialReaderOnFailure(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     "partial logs",
		err:        errors.New("backend unavailable"),
	}
	ctr := runTestContainer(t, f)

	rc, err := ctr.Logs(context.Background())
	if err == nil {
		t.Fatal("want logs failure")
	}
	if rc == nil {
		t.Fatal("partial log reader is nil")
	}
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil || string(data) != f.output+f.output {
		t.Fatalf("partial output = %q/%v, want returned stdout and stderr", data, readErr)
	}
}

func TestLogsBoundedReturnsPartialReaderOnFailures(t *testing.T) {
	missing := &cli.CLIError{Args: []string{"logs"}, ExitCode: 31, Stderr: `not found: "myctr"`}
	cases := []struct {
		name       string
		err        error
		inspectErr error
		notFound   bool
	}{
		{name: "infrastructure", err: errors.New("backend unavailable")},
		{name: "cancellation", err: context.DeadlineExceeded},
		{name: "launch", err: &exec.Error{Name: "container", Err: errors.New("not found")}},
		{name: "missing-container", err: missing, inspectErr: &cli.CLIError{Args: []string{"inspect"}, ExitCode: 1, Stderr: "not found"}, notFound: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &outputTestRunner{
				fakeRunner: newTestRunner(),
				output:     strings.Repeat("l", 32),
				err:        tc.err,
				inspectErr: tc.inspectErr,
			}
			ctr := runTestContainer(t, f)
			rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{MaxBytes: 8})
			if err == nil {
				t.Fatal("want logs failure")
			}
			if tc.notFound && !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("error = %v, want ErrContainerNotFound", err)
			}
			if rc == nil {
				t.Fatal("partial log reader is nil")
			}
			defer rc.Close()
			assertBoundedPartial(t, rc)
		})
	}
}

func TestLegacyOutputRemainsUnboundedByDefault(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("legacy", 1024),
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	_, output, err := ctr.Exec(context.Background(), []string{"true"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	data, readErr := io.ReadAll(output)
	if readErr != nil {
		t.Fatalf("legacy read error = %v", readErr)
	}
	if len(data) != len(f.output)*2 {
		t.Fatalf("legacy output bytes = %d, want %d", len(data), len(f.output)*2)
	}
}

func TestLimitedWriterDoesNotCallSinkFailureTruncation(t *testing.T) {
	deliveryErr := errors.New("sink failed before limit")
	writer := newLimitedWriter(&failingOutputWriter{err: deliveryErr}, 8)
	if _, err := writer.Write([]byte("short")); !errors.Is(err, deliveryErr) {
		t.Fatalf("Write error = %v, want sink error", err)
	}
	if writer.Truncated() {
		t.Fatal("sink failure was reported as configured-limit truncation")
	}
}

func TestOutputLimitValidation(t *testing.T) {
	f := &outputTestRunner{fakeRunner: newTestRunner(), output: "output"}
	ctr := runTestContainer(t, f)

	if _, err := ctr.LogsWithOptions(context.Background(), LogsOptions{MaxBytes: -1}); err == nil {
		t.Error("LogsWithOptions accepted negative MaxBytes")
	}
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}, WithExecMaxBytes(0)); err == nil {
		t.Error("Exec accepted zero WithExecMaxBytes")
	}
}

func TestWaitExecDiscardsOutputThroughStreamingRunner(t *testing.T) {
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("q", 4*1024*1024),
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	code, err := (waitTarget{c: ctr}).ExecCommand(context.Background(), []string{"check"})
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if f.runCalls != 0 || f.runToCalls != 1 {
		t.Fatalf("calls = Run:%d RunTo:%d, want 0/1", f.runCalls, f.runToCalls)
	}
}

func TestLogTailUsesFixedStreamingSink(t *testing.T) {
	const marker = "LATEST_FATAL_MARKER"
	f := &outputTestRunner{
		fakeRunner: newTestRunner(),
		output:     strings.Repeat("A", 2*1024*1024) + marker,
	}
	ctr := runTestContainer(t, f)
	resetOutputTestCalls(f)

	tail := ctr.logTail(context.Background())
	if len(tail) != logTailLimit {
		t.Fatalf("tail len = %d, want %d", len(tail), logTailLimit)
	}
	if !strings.HasSuffix(tail, marker) {
		t.Fatalf("tail does not retain latest marker: %q", tail[len(tail)-len(marker):])
	}
	if f.runCalls != 0 || f.runToCalls != 1 {
		t.Fatalf("calls = Run:%d RunTo:%d, want 0/1", f.runCalls, f.runToCalls)
	}
}

func TestBoundedReaderReportsTerminalTruncation(t *testing.T) {
	buffer := newBoundedBuffer(4)
	if _, err := buffer.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	reader := buffer.reader()
	if !reader.Truncated() {
		t.Fatal("Truncated() = false, want true")
	}
	data, err := io.ReadAll(reader)
	if string(data) != "abcd" {
		t.Fatalf("data = %q, want abcd", data)
	}
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("error = %v, want ErrOutputTruncated", err)
	}
}

func TestBoundedReaderSpansFixedChunks(t *testing.T) {
	limit := int64(2*boundedChunkSize + 3)
	input := strings.Repeat("0123456789", 6000)
	buffer := newBoundedBuffer(limit)
	if _, err := buffer.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(buffer.reader())
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	if string(data) != input {
		t.Fatalf("data length/content = %d/%q, want %d/%q", len(data), data[:16], len(input), input[:16])
	}
	if buffer.Truncated() {
		t.Fatal("Truncated() = true for output shorter than limit")
	}
}
