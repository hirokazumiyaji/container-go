package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type legacyTarget struct {
	running  bool
	execCode int
}

func (t *legacyTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func (t *legacyTarget) Running(context.Context) (bool, error) { return t.running, nil }

func (t *legacyTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (t *legacyTarget) ExecCommand(context.Context, []string) (int, error) {
	return t.execCode, nil
}

type lifecycleTarget struct {
	mu          sync.Mutex
	state       State
	stateErr    error
	stateScript []struct {
		state State
		err   error
	}
	stateCalls int
	execCode   int
	logStreams []io.ReadCloser
	logErrors  []error
	logCalls   int
}

func newLifecycleTarget(state State) *lifecycleTarget {
	return &lifecycleTarget{state: state}
}

func (t *lifecycleTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func (t *lifecycleTarget) Running(context.Context) (bool, error) {
	state, err := t.State(context.Background())
	return state == StateRunning, err
}

func (t *lifecycleTarget) State(context.Context) (State, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stateCalls++
	if len(t.stateScript) == 0 {
		return t.state, t.stateErr
	}
	step := t.stateScript[0]
	if len(t.stateScript) > 1 {
		t.stateScript = t.stateScript[1:]
	}
	t.state = step.state
	t.stateErr = step.err
	return step.state, step.err
}

func (t *lifecycleTarget) currentState() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *lifecycleTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := t.logCalls
	t.logCalls++
	if i < len(t.logErrors) && t.logErrors[i] != nil {
		return nil, t.logErrors[i]
	}
	if i >= len(t.logStreams) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return t.logStreams[i], nil
}

func (t *lifecycleTarget) ExecCommand(context.Context, []string) (int, error) {
	return t.execCode, nil
}

func TestBuiltInStrategyAcceptsLegacyRunningTarget(t *testing.T) {
	target := &legacyTarget{running: true, execCode: 0}
	if err := ForExec([]string{"true"}).WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("legacy Target rejected: %v", err)
	}
}

func TestForExecFailsFastOnTerminalLifecycleState(t *testing.T) {
	for _, state := range []State{StateStopped, StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			target := newLifecycleTarget(state)
			start := time.Now()
			err := ForExec([]string{"true"}).
				WithStartupTimeout(2*time.Second).
				WithPollInterval(20*time.Millisecond).
				WaitUntilReady(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), string(state)) {
				t.Fatalf("error = %v, want terminal %s failure", err, state)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("terminal state took %v; want fail-fast", elapsed)
			}
		})
	}
}

func TestForLogFailsFastOnTerminalLifecycleState(t *testing.T) {
	for _, state := range []State{StateStopped, StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			pr, pw := io.Pipe()
			t.Cleanup(func() { _ = pw.Close() })
			target := newLifecycleTarget(state)
			target.logStreams = []io.ReadCloser{pr}

			start := time.Now()
			err := ForLog("ready").
				WithStartupTimeout(2*time.Second).
				WithPollInterval(20*time.Millisecond).
				WaitUntilReady(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), string(state)) {
				t.Fatalf("error = %v, want terminal %s failure", err, state)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("terminal state took %v; want fail-fast", elapsed)
			}
		})
	}
}

func TestForLogClassifiesTerminalStateAfterEOF(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateRunning},
		{state: StateStopped},
	}
	target.logStreams = []io.ReadCloser{io.NopCloser(strings.NewReader("boot\n"))}

	start := time.Now()
	err := ForLog("ready").
		WithStartupTimeout(400*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want stopped after EOF", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("EOF classification took %v; want fail-fast", elapsed)
	}
}

func TestForAnyFailsFastOnTerminalLifecycleState(t *testing.T) {
	target := newLifecycleTarget(StatePaused)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	target.logStreams = []io.ReadCloser{pr}

	start := time.Now()
	err := ForAny(
		ForLog("ready").WithStartupTimeout(2*time.Second).WithPollInterval(20*time.Millisecond),
		ForExec([]string{"true"}).WithStartupTimeout(2*time.Second).WithPollInterval(20*time.Millisecond),
	).WaitUntilReady(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("error = %v, want paused failure from ForAny", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("ForAny took %v; want fail-fast", elapsed)
	}
}

type blockingStrategy struct{}

func (blockingStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestForAnyMonitorsTerminalTransitionForCustomStrategies(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateRunning},
		{state: StateStopped},
	}

	start := time.Now()
	err := ForAny(blockingStrategy{}).
		WithStartupTimeout(2*time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want stopped transition", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("ForAny transition took %v; want bounded fail-fast", elapsed)
	}
}

func TestPollReportsLastTransientStateErrorAtTimeout(t *testing.T) {
	target := newLifecycleTarget(StateUnknown)
	target.stateErr = errors.New("temporary inspect failure")
	err := poll(
		context.Background(),
		options{startupTimeout: 80 * time.Millisecond, pollInterval: 20 * time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return errors.New("not ready") },
	)
	if err == nil || !strings.Contains(err.Error(), "temporary inspect failure") {
		t.Fatalf("error = %v, want last inspect failure", err)
	}
}

func TestPollFailsFastOnNotFoundStateError(t *testing.T) {
	target := newLifecycleTarget(StateUnknown)
	target.stateErr = fmt.Errorf("%w: inspect failed", ErrTargetNotFound)
	start := time.Now()
	err := poll(
		context.Background(),
		options{startupTimeout: 2 * time.Second, pollInterval: 20 * time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return errors.New("not ready") },
	)
	if err == nil || !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("not-found took %v; want fail-fast", elapsed)
	}
}

func TestPollFailsFastWhileContainerIsStopping(t *testing.T) {
	target := newLifecycleTarget(StateStopping)
	start := time.Now()
	err := poll(
		context.Background(),
		options{startupTimeout: 2 * time.Second, pollInterval: 20 * time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return errors.New("not ready") },
	)
	if err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("error = %v, want stopping failure", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopping took %v; want fail-fast", elapsed)
	}
}

func TestForLogRetriesStreamOpenFailure(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logErrors = []error{errors.New("temporary log open failure")}
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("boot\n")),
		io.NopCloser(strings.NewReader("ready\n")),
	}

	err := ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.logCalls != 2 {
		t.Fatalf("FollowLogs calls = %d, want retry after open failure", target.logCalls)
	}
}

func TestForLogRetriesEOF(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("boot\n")),
		io.NopCloser(strings.NewReader("ready\n")),
	}

	err := ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.logCalls != 2 {
		t.Fatalf("FollowLogs calls = %d, want retry after EOF", target.logCalls)
	}
}

func TestStateProbeTransitionsAndRecoversFromInspectError(t *testing.T) {
	target := newLifecycleTarget(StateUnknown)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateUnknown, err: errors.New("temporary inspect failure")},
		{state: StateCreated},
		{state: StateRunning},
	}
	err := poll(
		context.Background(),
		options{startupTimeout: 2500 * time.Millisecond, pollInterval: 10 * time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error {
			if target.currentState() == StateRunning {
				return nil
			}
			return errors.New("not ready")
		},
	)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if target.stateCalls < 3 {
		t.Fatalf("State calls = %d, want inspect-error/Created/Running transitions", target.stateCalls)
	}
}
