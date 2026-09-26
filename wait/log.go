package wait

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LogStrategy waits until a pattern appears in the container's log
// stream. Patterns are matched per line.
type LogStrategy struct {
	options
	pattern     string
	isRegexp    bool
	occurrences int
}

// ForLog waits for a substring to appear in the logs.
func ForLog(pattern string) *LogStrategy {
	return &LogStrategy{pattern: pattern, occurrences: 1}
}

// AsRegexp interprets the pattern as a regular expression.
func (s *LogStrategy) AsRegexp() *LogStrategy {
	s.isRegexp = true
	return s
}

// WithOccurrence requires the pattern to appear n times.
func (s *LogStrategy) WithOccurrence(n int) *LogStrategy {
	s.occurrences = n
	return s
}

func (s *LogStrategy) WithStartupTimeout(d time.Duration) *LogStrategy {
	s.startupTimeout = d
	return s
}

func (s *LogStrategy) WithPollInterval(d time.Duration) *LogStrategy {
	s.pollInterval = d
	return s
}

type logScanResult struct {
	found bool
	err   error
}

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	timeout, _ := s.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	what := fmt.Sprintf("wait for log %q", s.pattern)

	var match func(line string) int
	if s.isRegexp {
		re, err := regexp.Compile(s.pattern)
		if err != nil {
			return fmt.Errorf("wait for log: %w", err)
		}
		match = func(line string) int { return len(re.FindAllString(line, -1)) }
	} else {
		match = func(line string) int { return strings.Count(line, s.pattern) }
	}

	stream, err := target.FollowLogs(waitCtx)
	if err != nil {
		if terminal := waitContextTerminationError(callerCtx, waitCtx, what, timeout, err); terminal != nil {
			return terminal
		}
		return wrapWaitCause(what, err)
	}
	// Closing the stream unblocks the scanner goroutine on timeout.
	defer stream.Close()

	resultCh := make(chan logScanResult, 1)
	state := &logScanState{}
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		count := 0
		for scanner.Scan() {
			count += match(scanner.Text())
			if count >= s.occurrences {
				result := logScanResult{found: true}
				state.publish(result)
				resultCh <- result
				return
			}
		}
		result := logScanResult{err: scanner.Err()}
		state.publish(result)
		resultCh <- result
	}()

	if result, ok := receiveLogScanResult(resultCh, state, waitCtx.Done()); ok {
		return handleLogScanResult(callerCtx, waitCtx, timeout, what, target, result)
	}
	return waitContextTerminationError(callerCtx, waitCtx, what, timeout, nil)
}

type logScanState struct {
	mu     sync.Mutex
	result logScanResult
	ready  bool
}

func (s *logScanState) publish(result logScanResult) {
	s.mu.Lock()
	s.result = result
	s.ready = true
	s.mu.Unlock()
}

func (s *logScanState) load() (logScanResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.ready
}

func receiveLogScanResult(results <-chan logScanResult, state *logScanState, done <-chan struct{}) (logScanResult, bool) {
	// Prefer a result queued before the context completion.
	select {
	case result := <-results:
		return result, true
	default:
	}
	if result, ok := state.load(); ok {
		return result, true
	}
	select {
	case result := <-results:
		return result, true
	case <-done:
		// Drain a result published immediately before the deadline.
		select {
		case result := <-results:
			return result, true
		default:
		}
		if result, ok := state.load(); ok {
			return result, true
		}
	}
	return logScanResult{}, false
}

func handleLogScanResult(callerCtx, waitCtx context.Context, timeout time.Duration, what string, target Target, result logScanResult) error {
	// receiveLogScanResult prefers a result published before the deadline, so
	// an observed pattern wins over a timer that fired afterwards. Checking
	// the context first would turn a satisfied wait into a rollback.
	if result.found {
		return nil
	}
	if terminal := waitContextTerminationError(callerCtx, waitCtx, what, timeout, result.err); terminal != nil {
		return terminal
	}

	// probeBudget bounds the diagnostic probe when the wait has no deadline.
	const probeBudget = 5 * time.Second
	// Keep the caller's context as the probe base so a later cancellation
	// or deadline also bounds diagnostics. Never extend the startup budget
	// when a stream ends near its deadline.
	var (
		probeCtx    context.Context
		probeCancel context.CancelFunc
	)
	if deadline, ok := waitCtx.Deadline(); ok {
		if time.Until(deadline) <= 0 {
			// The pattern was never observed, so this wait must fail. The
			// context timer callback is dispatched asynchronously, so
			// waitContextTerminationError can still report nil here; a
			// readiness gate must never return success on this path.
			return waitTimeoutError(what, timeout, result.err)
		}
		// Bound on the absolute deadline rather than a re-based duration,
		// so the probe cannot outlive the wait budget it is diagnosing.
		probeCtx, probeCancel = context.WithDeadline(callerCtx, deadline)
	} else {
		probeCtx, probeCancel = context.WithTimeout(callerCtx, probeBudget)
	}
	if probeErr := probeCtx.Err(); probeErr != nil {
		probeCancel()
		return waitContextError(what, probeErr, result.err)
	}
	running, probeErr := target.Running(probeCtx)
	probeCancel()
	if terminal := waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(result.err, probeErr)); terminal != nil {
		return terminal
	}
	if probeErr == nil && !running {
		return newWaitError(fmt.Sprintf("%s: container stopped before pattern appeared", what), result.err, probeErr)
	}
	message := fmt.Sprintf("%s: log stream ended before pattern appeared", what)
	if result.err != nil {
		message += fmt.Sprintf(" (read error: %v)", result.err)
	}
	return newWaitError(message, result.err, probeErr)
}
