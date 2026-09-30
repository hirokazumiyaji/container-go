package wait

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
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

const (
	logTerminalObservationWindow = 10 * time.Millisecond
	logScanDrainTimeout          = 100 * time.Millisecond
)

type logScanResult struct {
	matched  bool
	finished bool
	err      error
}

type logTerminalStream interface {
	TerminalError() error
}

type logDoneStream interface {
	Done() <-chan struct{}
}

type logObservation struct {
	terminalErr  error
	scanErr      error
	finished     bool
	terminalDone bool
}

func (o *logObservation) addScanResult(result logScanResult) {
	if !result.finished {
		return
	}
	o.finished = true
	if err := normalizeLogScanError(result.err); err != nil && o.scanErr == nil {
		o.scanErr = err
	}
}

func (o *logObservation) addTerminalError(err error) {
	if err = normalizeLogScanError(err); err != nil && o.terminalErr == nil {
		o.terminalErr = err
	}
}

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	timeout, _ := s.effective()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

	stream, err := target.FollowLogs(ctx)
	if err != nil {
		wrapped := fmt.Errorf("wait for log: %w", err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(wrapped, ctxErr)
		}
		return wrapped
	}
	closed := false
	closeStream := func() error {
		if closed {
			return nil
		}
		closed = true
		return stream.Close()
	}
	defer func() { _ = closeStream() }()

	results := make(chan logScanResult, 2)
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		count := 0
		matched := false
		for scanner.Scan() {
			if !matched {
				count += match(scanner.Text())
				if count >= s.occurrences {
					// Keep draining after reporting a match. A process-backed
					// stream can have a terminal stderr diagnostic immediately
					// after the matching line; Close will unblock this loop.
					matched = true
					results <- logScanResult{matched: true}
				}
			}
		}
		results <- logScanResult{matched: matched, finished: true, err: scanner.Err()}
	}()

	closeAndInspect := func(settleResults bool, observation *logObservation) error {
		observeLogBeforeClose(stream, results, settleResults, observation)
		closeErr := closeStream()
		observeLogAfterClose(stream, results, observation)
		return closeErr
	}

	select {
	case result := <-results:
		observation := &logObservation{}
		observation.addScanResult(result)
		if result.matched {
			closeErr := closeAndInspect(true, observation)
			contextErr := ctx.Err()
			if contextErr != nil {
				return errors.Join(
					fmt.Errorf("wait for log %q: %w", s.pattern, contextErr),
					observation.terminalErr,
					observation.scanErr,
					closeErr,
				)
			}
			if observation.terminalErr != nil || observation.scanErr != nil {
				return errors.Join(
					fmt.Errorf("wait for log %q: log stream failed after match: %w", s.pattern, errors.Join(observation.terminalErr, observation.scanErr)),
					closeErr,
				)
			}
			if closeErr != nil {
				return fmt.Errorf("wait for log %q: close log stream after match: %w", s.pattern, closeErr)
			}
			return nil
		}

		closeErr := closeAndInspect(false, observation)
		contextErr := ctx.Err()
		if contextErr != nil {
			return errors.Join(
				fmt.Errorf("wait for log %q: %w", s.pattern, contextErr),
				observation.terminalErr,
				observation.scanErr,
				closeErr,
			)
		}
		if observation.terminalErr != nil {
			return errors.Join(
				fmt.Errorf("wait for log %q: log stream failed before pattern appeared: %w", s.pattern, errors.Join(observation.terminalErr, observation.scanErr)),
				closeErr,
			)
		}
		if closeErr != nil {
			return errors.Join(
				fmt.Errorf("wait for log %q: close ended log stream: %w", s.pattern, closeErr),
				observation.scanErr,
			)
		}
		if readErr := normalizeLogScanError(observation.scanErr); readErr != nil {
			return errors.Join(
				fmt.Errorf("wait for log %q: log stream read failed: %w", s.pattern, readErr),
			)
		}
		streamErr := observation.scanErr
		if streamErr == nil || errors.Is(streamErr, io.ErrClosedPipe) {
			streamErr = io.EOF
		}
		// Do not detach this probe from ctx. In particular, a canceled
		// ForAny loser must not start a fresh five-second Running call.
		if contextErr != nil {
			return errors.Join(
				fmt.Errorf("wait for log %q: %w", s.pattern, contextErr),
				streamErr,
				closeErr,
			)
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		running, runningErr := target.Running(probeCtx)
		probeCancel()
		contextErr = ctx.Err()
		probeParts := make([]error, 0, 3)
		if runningErr != nil {
			probeParts = append(probeParts, fmt.Errorf("wait for log %q: check container state: %w", s.pattern, runningErr))
		}
		if runningErr == nil && !running {
			probeParts = append(probeParts, fmt.Errorf("wait for log %q: container stopped before pattern appeared", s.pattern))
		}
		if contextErr != nil {
			probeParts = append(probeParts, fmt.Errorf("wait for log %q: %w", s.pattern, contextErr))
		}
		if len(probeParts) > 0 {
			return errors.Join(append(probeParts, streamErr)...)
		}
		return errors.Join(
			fmt.Errorf("wait for log %q: log stream ended before pattern appeared: %w", s.pattern, streamErr),
		)
	case <-ctx.Done():
		observation := &logObservation{}
		closeErr := closeAndInspect(false, observation)
		return errors.Join(
			logContextError(ctx, s.pattern, timeout),
			observation.terminalErr,
			observation.scanErr,
			closeErr,
		)
	}
}

func logContextError(ctx context.Context, pattern string, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("wait for log %q: timed out after %v: %w", pattern, timeout, context.DeadlineExceeded)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait for log %q: %w", pattern, err)
	}
	return fmt.Errorf("wait for log %q: timed out after %v", pattern, timeout)
}

func observeLogBeforeClose(stream io.ReadCloser, results <-chan logScanResult, waitForScan bool, observation *logObservation) {
	terminal, hasTerminal := stream.(logTerminalStream)
	done, hasDone := stream.(logDoneStream)
	var doneCh <-chan struct{}
	if hasTerminal && hasDone {
		doneCh = done.Done()
	}
	if !hasTerminal && !waitForScan {
		return
	}

	timer := time.NewTimer(logTerminalObservationWindow)
	defer timer.Stop()
	for {
		if hasTerminal && observation.terminalDone {
			observation.addTerminalError(terminal.TerminalError())
			if !waitForScan {
				return
			}
		}
		if !hasTerminal && observation.finished {
			return
		}
		if hasTerminal && waitForScan && observation.terminalDone && observation.finished {
			return
		}
		select {
		case <-doneCh:
			observation.terminalDone = true
			doneCh = nil
			observation.addTerminalError(terminal.TerminalError())
			if !waitForScan {
				return
			}
		case result := <-results:
			observation.addScanResult(result)
			if !hasTerminal && observation.finished {
				return
			}
		case <-timer.C:
			return
		}
	}
}

func observeLogAfterClose(stream io.ReadCloser, results <-chan logScanResult, observation *logObservation) {
	if !observation.finished {
		timer := time.NewTimer(logScanDrainTimeout)
	waitForScanner:
		for !observation.finished {
			select {
			case result := <-results:
				observation.addScanResult(result)
			case <-timer.C:
				break waitForScanner
			}
		}
		timer.Stop()
	}

	terminal, hasTerminal := stream.(logTerminalStream)
	if !hasTerminal {
		return
	}
	var doneCh <-chan struct{}
	if done, ok := stream.(logDoneStream); ok {
		doneCh = done.Done()
	}
	timer := time.NewTimer(logScanDrainTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if observation.terminalDone || doneCh == nil {
			observation.addTerminalError(terminal.TerminalError())
			if observation.terminalErr != nil {
				return
			}
		}
		select {
		case <-doneCh:
			observation.terminalDone = true
			doneCh = nil
		case <-ticker.C:
		case <-timer.C:
			return
		}
	}
}

func normalizeLogScanError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}
