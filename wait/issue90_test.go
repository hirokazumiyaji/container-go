package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
