package wait

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type freshContinuousReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *freshContinuousReader) Read(p []byte) (int, error) {
	select {
	case <-r.closed:
		return 0, io.EOF
	default:
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func (r *freshContinuousReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestSettleLogMatchRemainsObservableWithContinuousReader(t *testing.T) {
	stream := &freshContinuousReader{closed: make(chan struct{})}
	t.Cleanup(func() { _ = stream.Close() })
	// The idle and max settle windows stay far beyond the cancellation, so
	// only the caller context can end the settlement. A continuously
	// readable stream must not starve the select that observes it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)

	started := time.Now()
	err := settleLogMatch(ctx, stream, bufio.NewReader(stream))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("settle error = %v, want caller cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("continuous reader settlement took %v, want prompt cancellation", elapsed)
	}
}

func TestFinalLifecycleProbeRetriesTransientError(t *testing.T) {
	transient := errors.New("temporary lifecycle probe failure")
	target := &issue90Target{runningErrs: []error{transient}}
	err := poll(
		context.Background(),
		options{startupTimeout: time.Second, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return nil },
		false,
	)
	if err != nil {
		t.Fatalf("poll returned %v, want success after transient final probe", err)
	}
	if calls := target.runningCalls.Load(); calls != 2 {
		t.Fatalf("Running calls = %d, want transient error followed by success", calls)
	}
}

func TestFinalLifecycleProbePreservesPermanentCause(t *testing.T) {
	target := &issue90Target{runningErrs: []error{ErrContainerNotFound}}
	err := poll(
		context.Background(),
		options{startupTimeout: time.Second, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return nil },
		false,
	)
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("poll error = %v, want permanent lifecycle cause", err)
	}
	if calls := target.runningCalls.Load(); calls != 1 {
		t.Fatalf("Running calls = %d, want no retry for permanent failure", calls)
	}
}

type freshAlwaysTransientTarget struct {
	*issue90Target
	cause error
}

func (t *freshAlwaysTransientTarget) Running(context.Context) (bool, error) {
	t.runningCalls.Add(1)
	return false, t.cause
}

func TestFinalLifecycleProbeRetainsTransientCauseAtDeadline(t *testing.T) {
	transient := errors.New("lifecycle stayed unavailable")
	target := &freshAlwaysTransientTarget{
		issue90Target: &issue90Target{},
		cause:         transient,
	}
	err := poll(
		context.Background(),
		options{startupTimeout: 35 * time.Millisecond, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return nil },
		false,
	)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, transient) {
		t.Fatalf("error = %v, want deadline and latest lifecycle cause", err)
	}
}

type freshReconnectTarget struct {
	calls atomic.Int32
}

func (*freshReconnectTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("unused endpoint")
}
func (*freshReconnectTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*freshReconnectTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("unused exec")
}
func (t *freshReconnectTarget) Running(context.Context) (bool, error) {
	return false, fmt.Errorf("probe-%d", t.calls.Add(1))
}

func TestLogReconnectRetainsOnlyLatestProbeError(t *testing.T) {
	target := &freshReconnectTarget{}
	err := ForLog("never").
		WithPollInterval(time.Microsecond).
		WithStartupTimeout(35*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
	if got := strings.Count(err.Error(), "probe-"); got != 1 {
		t.Fatalf("error contains %d probe causes, want one latest cause: %v", got, err)
	}
	if target.calls.Load() < 2 {
		t.Fatalf("probe calls = %d, want reconnects", target.calls.Load())
	}
}
