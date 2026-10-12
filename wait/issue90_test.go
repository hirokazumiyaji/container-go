package wait

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type issue90Target struct {
	endpoint      string
	endpointErr   error
	endpointCalls atomic.Int32
	runningCalls  atomic.Int32
	runningErrs   []error
	runningStates []bool
	followCalls   atomic.Int32
	execCalls     atomic.Int32
	execErr       error
	logs          []io.ReadCloser
}

func (t *issue90Target) Endpoint(context.Context, string) (string, error) {
	t.endpointCalls.Add(1)
	return t.endpoint, t.endpointErr
}

func (t *issue90Target) Running(context.Context) (bool, error) {
	call := int(t.runningCalls.Add(1)) - 1
	if call < len(t.runningErrs) && t.runningErrs[call] != nil {
		return false, t.runningErrs[call]
	}
	if call < len(t.runningStates) {
		return t.runningStates[call], nil
	}
	return true, nil
}

func (t *issue90Target) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.followCalls.Add(1)
	if len(t.logs) == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	i := int(t.followCalls.Load()) - 1
	if i >= len(t.logs) {
		i = len(t.logs) - 1
	}
	return t.logs[i], nil
}

func (t *issue90Target) ExecCommand(context.Context, []string) (int, error) {
	t.execCalls.Add(1)
	return 1, t.execErr
}

type issue90SuccessfulExecTarget struct {
	*issue90Target
}

func (*issue90SuccessfulExecTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

func TestEndpointLaunchErrorIsPermanentForPortAndHTTP(t *testing.T) {
	launchErr := &exec.Error{Name: "container", Err: errors.New("backend binary not found")}
	strategies := map[string]func(*issue90Target) error{
		"port": func(target *issue90Target) error {
			return ForListeningPort("6379/tcp").
				WithStartupTimeout(100*time.Millisecond).
				WithPollInterval(time.Millisecond).
				WaitUntilReady(context.Background(), target)
		},
		"http": func(target *issue90Target) error {
			return ForHTTP("/ready").
				WithStartupTimeout(100*time.Millisecond).
				WithPollInterval(time.Millisecond).
				WaitUntilReady(context.Background(), target)
		},
	}
	for name, wait := range strategies {
		t.Run(name, func(t *testing.T) {
			target := &issue90Target{endpointErr: launchErr}
			err := wait(target)
			var got *exec.Error
			if !errors.As(err, &got) || got != launchErr {
				t.Fatalf("error = %v, want permanent *exec.Error", err)
			}
			if got := target.endpointCalls.Load(); got != 1 {
				t.Fatalf("Endpoint calls = %d, want one fail-fast launch attempt", got)
			}
		})
	}
}

func TestStateLaunchErrorIsPermanentAfterSuccessfulCheck(t *testing.T) {
	launchErr := &exec.Error{Name: "container", Err: errors.New("backend binary not found")}
	target := &issue90SuccessfulExecTarget{issue90Target: &issue90Target{
		runningErrs: []error{launchErr},
	}}
	err := ForExec([]string{"true"}).
		WithStartupTimeout(100*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	var got *exec.Error
	if !errors.As(err, &got) || got != launchErr {
		t.Fatalf("error = %v, want permanent state launch error", err)
	}
	if got := target.runningCalls.Load(); got != 1 {
		t.Fatalf("Running calls = %d, want one final state launch attempt", got)
	}
}

func TestWaitRejectsInvalidConfigurationBeforeTargetCalls(t *testing.T) {
	tests := []struct {
		name     string
		strategy Strategy
		calls    func(*issue90Target) int32
	}{
		{
			name:     "port syntax",
			strategy: ForListeningPort("not-a-port"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "UDP port is not a TCP wait target",
			strategy: ForListeningPort("6379/udp"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "HTTP UDP port is not a TCP wait target",
			strategy: ForHTTP("/").WithPort("6379/udp"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "empty explicit port",
			strategy: ForListeningPort(""),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "http method",
			strategy: ForHTTP("/").WithMethod("GET\n"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "empty HTTP method",
			strategy: ForHTTP("/").WithMethod(""),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "http path",
			strategy: ForHTTP("health"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "http path with space",
			strategy: ForHTTP("/health check"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "http header",
			strategy: ForHTTP("/").WithHeader("X-Test\n", "value"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "http port",
			strategy: ForHTTP("/").WithPort("not-a-port"),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "empty explicit HTTP port",
			strategy: ForHTTP("/").WithPort(""),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "empty exec executable",
			strategy: ForExec([]string{""}),
			calls:    func(target *issue90Target) int32 { return target.execCalls.Load() },
		},
		{
			name:     "empty log pattern",
			strategy: ForLog(""),
			calls:    func(target *issue90Target) int32 { return target.followCalls.Load() },
		},
		{
			name:     "zero log occurrence",
			strategy: ForLog("ready").WithOccurrence(0),
			calls:    func(target *issue90Target) int32 { return target.followCalls.Load() },
		},
		{
			name:     "invalid regexp",
			strategy: ForLog("[").AsRegexp(),
			calls:    func(target *issue90Target) int32 { return target.followCalls.Load() },
		},
		{
			name:     "negative startup timeout",
			strategy: ForListeningPort("6379/tcp").WithStartupTimeout(-time.Second),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "negative port poll interval",
			strategy: ForListeningPort("6379/tcp").WithPollInterval(-time.Second),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "negative HTTP poll interval",
			strategy: ForHTTP("/").WithPollInterval(-time.Second),
			calls:    func(target *issue90Target) int32 { return target.endpointCalls.Load() },
		},
		{
			name:     "negative exec poll interval",
			strategy: ForExec([]string{"false"}).WithPollInterval(-time.Second),
			calls:    func(target *issue90Target) int32 { return target.execCalls.Load() },
		},
		{
			name:     "negative log poll interval",
			strategy: ForLog("ready").WithPollInterval(-time.Second),
			calls:    func(target *issue90Target) int32 { return target.followCalls.Load() },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := &issue90Target{}
			started := time.Now()
			err := tc.strategy.WaitUntilReady(context.Background(), target)
			if err == nil {
				t.Fatal("want invalid configuration error")
			}
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
			if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
				t.Fatalf("validation took %v", elapsed)
			}
			if got := tc.calls(target); got != 0 {
				t.Fatalf("target calls = %d, want 0", got)
			}
		})
	}
}

func TestUndeclaredPortFailsFastAndRemainsMatchable(t *testing.T) {
	for _, strategy := range []Strategy{
		ForListeningPort("6379/tcp"),
		ForExposedPort(),
	} {
		target := &issue90Target{endpointErr: ErrPortNotExposed}
		started := time.Now()
		err := strategy.WaitUntilReady(context.Background(), target)
		if err == nil {
			t.Fatal("want undeclared-port error")
		}
		if !errors.Is(err, ErrPortNotExposed) {
			t.Fatalf("error = %v, want ErrPortNotExposed", err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("wait took %v, want fail-fast", elapsed)
		}
		if got := target.endpointCalls.Load(); got != 1 {
			t.Fatalf("endpoint calls = %d, want 1", got)
		}
	}
}

func TestInvalidHTTPRequestFailsFast(t *testing.T) {
	target := &issue90Target{endpoint: "%"}
	started := time.Now()
	err := ForHTTP("/").WithStartupTimeout(time.Second).WithPollInterval(time.Millisecond).WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want invalid HTTP request", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("wait took %v, want fail-fast", elapsed)
	}
	if got := target.endpointCalls.Load(); got != 1 {
		t.Fatalf("endpoint calls = %d, want 1", got)
	}
}

func TestTimeoutRetainsLastErrorChain(t *testing.T) {
	cause := errors.New("temporary endpoint failure")
	target := &issue90Target{}
	err := poll(
		context.Background(),
		options{startupTimeout: 5 * time.Millisecond, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return cause },
	)
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want last error in chain", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded in chain", err)
	}
}

func TestContainerNotFoundFromExecFailsFast(t *testing.T) {
	target := &issue90Target{execErr: ErrContainerNotFound}
	started := time.Now()
	err := ForExec([]string{"false"}).WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("wait took %v, want fail-fast", elapsed)
	}
	if got := target.execCalls.Load(); got != 1 {
		t.Fatalf("exec calls = %d, want 1", got)
	}
}

func TestContainerNotFoundFailsFastDuringWait(t *testing.T) {
	target := &issue90Target{endpointErr: ErrContainerNotFound}
	started := time.Now()
	err := ForListeningPort("6379/tcp").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("wait took %v, want fail-fast", elapsed)
	}
	if got := target.endpointCalls.Load(); got != 1 {
		t.Fatalf("endpoint calls = %d, want 1", got)
	}
}

// nextReadyLogSettle is the priority pass used before idle/max timers.
func TestLogSettlePrefersReadyEvidenceBeforeTerminal(t *testing.T) {
	reads := make(chan logSettleRead, 1)
	done := make(chan struct{})
	want := logSettleRead{err: errLogLineTooLong}
	reads <- want

	got, hasRead, doneReady := nextReadyLogSettle(reads, done)
	if !hasRead || doneReady || !errors.Is(got.err, errLogLineTooLong) {
		t.Fatalf("ready evidence = (%+v, read=%v, done=%v), want queued read", got, hasRead, doneReady)
	}

	close(done)
	_, hasRead, doneReady = nextReadyLogSettle(make(chan logSettleRead), done)
	if hasRead || !doneReady {
		t.Fatalf("done evidence = (read=%v, done=%v), want done", hasRead, doneReady)
	}
}

func TestForLogTreatsOversizedLineAsPermanent(t *testing.T) {
	target := &issue90Target{logs: []io.ReadCloser{
		io.NopCloser(strings.NewReader(strings.Repeat("x", maxLogLineSize+1))),
	}}
	err := ForLog("never").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, errLogLineTooLong) {
		t.Fatalf("error = %v, want errLogLineTooLong", err)
	}
	if got := target.followCalls.Load(); got != 1 {
		t.Fatalf("FollowLogs calls = %d, want 1", got)
	}
	// Lifecycle probes may run while the oversized line is still being
	// read. Permanence is FollowLogs staying at 1 (no reconnect), not
	// that Running is never observed mid-scan.
}

func TestForLogPollIntervalReconnects(t *testing.T) {
	target := &issue90Target{logs: []io.ReadCloser{
		io.NopCloser(strings.NewReader("starting\n")),
		io.NopCloser(strings.NewReader("ready\n")),
	}}
	err := ForLog("ready").WithPollInterval(time.Millisecond).WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.followCalls.Load(); got != 2 {
		t.Fatalf("FollowLogs calls = %d, want 2", got)
	}
	if got := target.runningCalls.Load(); got != 2 {
		t.Fatalf("Running calls = %d, want reconnect and final lifecycle checks", got)
	}
}

func TestForLogReconnectsAfterTransientStateProbeError(t *testing.T) {
	stateErr := &cli.CLIError{
		Binary:   "container",
		Args:     []string{"inspect", "myctr"},
		ExitCode: 1,
		Stderr:   "temporary daemon failure",
	}
	target := &issue90Target{
		runningErrs: []error{stateErr},
		logs: []io.ReadCloser{
			io.NopCloser(strings.NewReader("starting\n")),
			io.NopCloser(strings.NewReader("ready\n")),
		},
	}
	if err := ForLog("ready").
		WithPollInterval(time.Millisecond).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.followCalls.Load(); got != 2 {
		t.Fatalf("FollowLogs calls = %d, want reconnect after transient state error", got)
	}
}

func TestScanLogStreamBoundsReplayState(t *testing.T) {
	const lineCount = 10_000
	stream := io.NopCloser(strings.NewReader(strings.Repeat("history\n", lineCount)))
	result := scanLogStream(
		context.Background(),
		stream,
		func(string) int { return 0 },
		1,
		logReplay{},
	)
	if result.err != nil {
		t.Fatalf("scanLogStream: %v", result.err)
	}
	if got := len(result.lines); got > 4096 {
		t.Fatalf("replay lines = %d, want bounded to at most 4096", got)
	}
	if result.lineCount != lineCount {
		t.Fatalf("line count = %d, want %d", result.lineCount, lineCount)
	}
}

func TestScanLogStreamDeduplicatesReplayBeyondFingerprintWindow(t *testing.T) {
	const lineCount = 10_000
	history := "ready\n" + strings.Repeat("history\n", lineCount-1)
	first := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader(history)),
		func(string) int { return 0 },
		1,
		logReplay{},
	)
	if first.err != nil {
		t.Fatalf("first scan: %v", first.err)
	}
	replay := logReplay{
		previous:      first.lines,
		previousStart: first.lineStart,
		previousLines: first.lineCount,
		count:         first.count,
	}
	matches := 0
	second := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader(history+"ready\nready\n")),
		func(line string) int {
			lineMatches := strings.Count(line, "ready")
			matches += lineMatches
			return lineMatches
		},
		2,
		replay,
	)
	if second.err != nil {
		t.Fatalf("second scan: %v", second.err)
	}
	if !second.found {
		t.Fatal("two new occurrences after the replayed prefix were not found")
	}
	if matches != 2 {
		t.Fatalf("ready matches = %d, want replayed occurrence excluded", matches)
	}
	if second.lineCount != lineCount+2 {
		t.Fatalf("line count = %d, want %d", second.lineCount, lineCount+2)
	}
}

type issue90FragmentErrorReader struct {
	fragment string
	err      error
	sent     bool
}

func (r *issue90FragmentErrorReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, r.err
	}
	r.sent = true
	return copy(p, r.fragment), nil
}

func (*issue90FragmentErrorReader) Close() error { return nil }

func TestScanLogStreamRollsBackTransientState(t *testing.T) {
	baseline := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader("one\ntwo\n")),
		func(string) int { return 0 },
		1,
		logReplay{},
	)
	if baseline.err != nil {
		t.Fatalf("baseline scan: %v", baseline.err)
	}
	replay := logReplay{
		previous:      baseline.lines,
		previousStart: baseline.lineStart,
		previousLines: baseline.lineCount,
		count:         baseline.count,
		partial:       baseline.partial,
	}
	transient := errors.New("transport interrupted")
	result := scanLogStream(
		context.Background(),
		&issue90FragmentErrorReader{fragment: "ready", err: transient},
		func(line string) int { return strings.Count(line, "ready") },
		1,
		replay,
	)
	if !errors.Is(result.err, transient) {
		t.Fatalf("scan error = %v, want transient transport error", result.err)
	}
	if result.count != replay.count || result.lineCount != replay.previousLines || len(result.lines) != len(replay.previous) {
		t.Fatalf(
			"transient scan advanced committed state: count=%d lines=%d fingerprints=%d",
			result.count,
			result.lineCount,
			len(result.lines),
		)
	}
	if string(result.partial) != string(replay.partial) {
		t.Fatalf("transient partial = %q, want committed %q", result.partial, replay.partial)
	}
}

func TestScanLogStreamUsesScannerLineSemantics(t *testing.T) {
	tests := map[string]string{
		"final unterminated": "ready",
		"LF":                 "ready\n",
		"CRLF":               "ready\r\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			var matched string
			result := scanLogStream(
				context.Background(),
				io.NopCloser(strings.NewReader(input)),
				func(line string) int {
					matched = line
					return strings.Count(line, "ready")
				},
				1,
				logReplay{},
			)
			if result.err != nil {
				t.Fatalf("scan: %v", result.err)
			}
			if !result.found || result.count != 1 {
				t.Fatalf("scan = found:%v count:%d, want final logical line", result.found, result.count)
			}
			if matched != "ready" {
				t.Fatalf("matched line = %q, want Scanner text without terminator", matched)
			}
		})
	}
}

func TestScanLogStreamTreatsFinalAndTerminatedReadyAsSameFingerprint(t *testing.T) {
	first := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader("ready")),
		func(line string) int { return strings.Count(line, "ready") },
		1,
		logReplay{},
	)
	if first.err != nil || first.count != 1 || first.lineCount != 1 {
		t.Fatalf("first scan = err:%v count:%d lines:%d, want one final line", first.err, first.count, first.lineCount)
	}
	replay := logReplay{
		previous:      first.lines,
		previousStart: first.lineStart,
		previousLines: first.lineCount,
		count:         first.count,
		partial:       first.partial,
	}
	matches := 0
	second := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader("ready\nready\n")),
		func(line string) int {
			matches += strings.Count(line, "ready")
			return strings.Count(line, "ready")
		},
		2,
		replay,
	)
	if second.err != nil || !second.found {
		t.Fatalf("second scan = err:%v found:%v, want one new occurrence", second.err, second.found)
	}
	if matches != 1 {
		t.Fatalf("new ready matches = %d, want replayed final line excluded", matches)
	}
}

func TestForAllValidatesChildrenBeforeRunning(t *testing.T) {
	target := &issue90Target{}
	err := ForAll(
		ForListeningPort("6379/tcp"),
		ForHTTP("invalid-path"),
	).WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
	}
	if got := target.endpointCalls.Load(); got != 0 {
		t.Fatalf("endpoint calls = %d, want 0", got)
	}
}

type terminalLogReader struct {
	err  error
	sent bool
}

func (r *terminalLogReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(p, "ready\n"), r.err
}

func (r *terminalLogReader) Close() error { return nil }

func TestForLogDoesNotAcceptTerminalCLIError(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", "myctr"},
		ExitCode: 17,
		Stderr:   "logs stream failed",
	}
	target := &issue90Target{logs: []io.ReadCloser{&terminalLogReader{err: terminal}}}
	err := ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("terminal CLI error unexpectedly satisfied the log pattern")
	}
	var got *cli.CLIError
	if !errors.As(err, &got) {
		t.Fatalf("error = %v, want *cli.CLIError", err)
	}
	if got.ExitCode != 17 {
		t.Fatalf("ExitCode = %d, want 17", got.ExitCode)
	}
	if target.followCalls.Load() != 1 {
		t.Fatalf("FollowLogs calls = %d, want no reconnect", target.followCalls.Load())
	}
	if target.runningCalls.Load() != 0 {
		t.Fatalf("Running calls = %d, want no probe after terminal error", target.runningCalls.Load())
	}
}

func TestForLogDeduplicatesReplayedHistoryAcrossReconnect(t *testing.T) {
	target := &issue90Target{logs: []io.ReadCloser{
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("ready\nstill starting\nready\n")),
	}}
	err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.followCalls.Load(); got != 3 {
		t.Fatalf("FollowLogs calls = %d, want 3 after two replayed snapshots", got)
	}
}

func TestForLogDoesNotCommitTransientOccurrence(t *testing.T) {
	transient := errors.New("transport interrupted")
	target := &issue90Target{logs: []io.ReadCloser{
		&issue90FragmentErrorReader{fragment: "ready", err: transient},
		io.NopCloser(strings.NewReader("")),
	}}
	err := ForLog("ready").
		WithStartupTimeout(30*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want timeout without committed transient occurrence", err)
	}
}

func TestForLogDeduplicatesFinalReadyWithReplayedTerminatedLine(t *testing.T) {
	target := &issue90Target{logs: []io.ReadCloser{
		io.NopCloser(strings.NewReader("ready")),
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("ready\nready\n")),
	}}
	if err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.followCalls.Load(); got != 3 {
		t.Fatalf("FollowLogs calls = %d, want 3 logical ready occurrences", got)
	}
}

type instantStrategy struct{}

func (instantStrategy) WaitUntilReady(context.Context, Target) error { return nil }

func TestCompositeNegativeTimeoutRemainsUnbounded(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(instantStrategy{}).WithStartupTimeout(-time.Second),
		"any": ForAny(instantStrategy{}).WithStartupTimeout(-time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(strategy); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if err := strategy.WaitUntilReady(context.Background(), &issue90Target{}); err != nil {
				t.Fatalf("WaitUntilReady: %v", err)
			}
		})
	}
}

func TestValidateRecursesAndRejectsTypedNil(t *testing.T) {
	var typedNil *LogStrategy
	for name, strategy := range map[string]Strategy{
		"typed nil leaf":  typedNil,
		"typed nil child": ForAll(ForAny(typedNil)),
		"nested invalid":  ForAll(ForAny(ForExec(nil))),
		"invalid port":    ForAll(ForListeningPort("not-a-port")),
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(strategy); err == nil || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("Validate error = %v, want ErrInvalidConfiguration", err)
			}
		})
	}
}

func TestValidateWithPortsChecksDeclarationsRecursively(t *testing.T) {
	if err := ValidateWithPorts(ForListeningPort("6379/tcp"), []string{"6379/tcp"}); err != nil {
		t.Fatalf("declared TCP port: %v", err)
	}
	if err := ValidateWithPorts(ForExposedPort(), []string{"53/udp"}); !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("UDP-only default error = %v, want ErrPortNotExposed", err)
	}
	if err := ValidateWithPorts(ForAll(ForAny(ForHTTP("/"))), []string{"80/tcp"}); err != nil {
		t.Fatalf("nested default TCP port: %v", err)
	}
	if err := ValidateWithPorts(ForHTTP("/").WithPort("8080/tcp"), []string{"80/tcp"}); !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("missing explicit port error = %v, want ErrPortNotExposed", err)
	}
}

func TestCanonicalPortSpecNormalizesNumericText(t *testing.T) {
	tests := map[string]string{
		"80":        "80/tcp",
		"080/tcp":   "80/tcp",
		"00053":     "53/tcp",
		"00053/udp": "53/udp",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := canonicalPortSpec(input)
			if err != nil {
				t.Fatalf("canonicalPortSpec(%q): %v", input, err)
			}
			if got != want {
				t.Fatalf("canonicalPortSpec(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestValidateWithPortsMatchesCanonicalPortText(t *testing.T) {
	if err := ValidateWithPorts(ForListeningPort("00080"), []string{"80/tcp"}); err != nil {
		t.Fatalf("leading-zero strategy port: %v", err)
	}
	if err := ValidateWithPorts(ForHTTP("/").WithPort("80"), []string{"00080/tcp"}); err != nil {
		t.Fatalf("leading-zero declaration: %v", err)
	}
}
