package wait

import (
	"context"
	"errors"
	"io"
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
	t.runningCalls.Add(1)
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
		false,
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
	if got := target.runningCalls.Load(); got != 1 {
		t.Fatalf("Running calls = %d, want 1 before reconnect", got)
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
