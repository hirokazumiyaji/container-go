package wait

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type execResult struct {
	code   int
	output string
	err    error
}

// scriptedExecTarget implements ExecOutputTarget and records which exec
// surface ForExec used.
type scriptedExecTarget struct {
	*fakeTarget
	mu          sync.Mutex
	execCalls   int
	outputCalls int
	results     []execResult
	fallback    execResult
}

func (t *scriptedExecTarget) next() execResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.results) == 0 {
		return t.fallback
	}
	result := t.results[0]
	t.results = t.results[1:]
	return result
}

func (t *scriptedExecTarget) ExecCommand(context.Context, []string) (int, error) {
	t.mu.Lock()
	t.execCalls++
	t.mu.Unlock()
	result := t.next()
	return result.code, result.err
}

func (t *scriptedExecTarget) ExecCommandOutput(context.Context, []string) (int, []byte, error) {
	t.mu.Lock()
	t.outputCalls++
	t.mu.Unlock()
	result := t.next()
	return result.code, []byte(result.output), result.err
}

func (t *scriptedExecTarget) counts() (execCalls, outputCalls int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.execCalls, t.outputCalls
}

func newScriptedExecTarget(results ...execResult) *scriptedExecTarget {
	return &scriptedExecTarget{fakeTarget: newFakeTarget(), results: results}
}

func TestForExecWithoutMatcherDiscardsOutput(t *testing.T) {
	target := newScriptedExecTarget(execResult{output: "PONG\n"})
	err := ForExec([]string{"redis-cli", "ping"}).
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	execCalls, outputCalls := target.counts()
	if execCalls != 1 || outputCalls != 0 {
		t.Errorf("exec calls = %d, output calls = %d; want the plain ExecCommand path", execCalls, outputCalls)
	}
}

func TestForExecOutputContainsAcceptsReadyOutput(t *testing.T) {
	target := newScriptedExecTarget(execResult{output: "PONG\n"})
	err := ForExec([]string{"redis-cli", "ping"}).
		WithOutputContains("PONG").
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	execCalls, outputCalls := target.counts()
	if outputCalls != 1 || execCalls != 0 {
		t.Errorf("exec calls = %d, output calls = %d; want the output probe", execCalls, outputCalls)
	}
}

func TestForExecOutputMatcherRetriesRejectedOutput(t *testing.T) {
	target := newScriptedExecTarget(
		execResult{output: "LOADING"},
		execResult{output: "PONG"},
	)
	err := ForExec([]string{"redis-cli", "ping"}).
		WithResponseMatcher(func(output []byte) bool { return bytes.Contains(output, []byte("PONG")) }).
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	_, outputCalls := target.counts()
	if outputCalls < 2 {
		t.Errorf("output calls = %d, want a retry after the rejected output", outputCalls)
	}
}

func TestForExecOutputMatcherRunsOnlyAfterAcceptedExitCode(t *testing.T) {
	var matcherCalls int
	target := newScriptedExecTarget(execResult{code: 1, output: "PONG"})
	target.fallback = execResult{code: 1, output: "PONG"}
	err := ForExec([]string{"redis-cli", "ping"}).
		WithResponseMatcher(func([]byte) bool {
			matcherCalls++
			return true
		}).
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if matcherCalls != 0 {
		t.Errorf("output matcher calls = %d, want none while the exit code is rejected", matcherCalls)
	}
}

func TestForExecOutputMatcherRequiresOutputTarget(t *testing.T) {
	start := time.Now()
	err := ForExec([]string{"redis-cli", "ping"}).
		WithOutputContains("PONG").
		WithStartupTimeout(5*time.Second).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("error = %v, want ErrInvalidConfiguration", err)
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Field != "target" {
		t.Errorf("error = %v, want the target output configuration error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
}

func TestForExecOutputMatcherTimeoutReportsRejection(t *testing.T) {
	target := newScriptedExecTarget(execResult{output: "LOADING"})
	target.fallback = execResult{output: "LOADING"}
	err := ForExec([]string{"probe"}).
		WithOutputContains("READY").
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if !strings.Contains(err.Error(), "command output rejected") {
		t.Errorf("error = %q, want the rejected output diagnostic", err)
	}
}

func TestForExecOutputProbeRetriesBoundedAttempts(t *testing.T) {
	target := &blockingExecOutputTarget{fakeTarget: newFakeTarget()}
	err := ForExec([]string{"probe"}).
		WithOutputContains("READY").
		WithExecTimeout(10*time.Millisecond).
		WithStartupTimeout(80*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if calls := target.callCount(); calls < 2 {
		t.Errorf("output probe calls = %d, want repeated bounded attempts", calls)
	}
}

type blockingExecOutputTarget struct {
	*fakeTarget
	mu    sync.Mutex
	calls int
}

func (t *blockingExecOutputTarget) ExecCommandOutput(ctx context.Context, _ []string) (int, []byte, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	<-ctx.Done()
	return 0, nil, ctx.Err()
}

func (t *blockingExecOutputTarget) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func TestExecOutputSetters(t *testing.T) {
	base := ForExec([]string{"probe"})
	withContains := base.WithOutputContains("READY")
	if withContains.outputMatcher == nil || withContains.outputContains != "READY" || !withContains.outputContainsSet {
		t.Fatal("WithOutputContains was not stored")
	}
	if base.outputMatcher != nil || base.outputContains != "" || base.outputContainsSet {
		t.Error("WithOutputContains mutated the receiver")
	}
	withMatcher := withContains.WithResponseMatcher(func([]byte) bool { return true })
	if withMatcher.outputContains != "" || withMatcher.outputContainsSet || withMatcher.outputMatcher == nil {
		t.Error("WithResponseMatcher did not replace the substring matcher")
	}
	if withContains.outputContains != "READY" || !withContains.outputContainsSet {
		t.Error("WithResponseMatcher mutated an earlier derived strategy")
	}

	err := ForExec([]string{"probe"}).WithOutputContains("").WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration for an empty substring", err)
	}
}
