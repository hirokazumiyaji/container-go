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

// terminalSettleWindow is a short grace period for a process-backed stream
// that has just produced a matching line. It observes an already-finishing
// CLI without delaying readiness for a live follow stream.
const terminalSettleWindow = 10 * time.Millisecond

// LogStrategy waits for a pattern to appear in the container's log
// stream. Patterns are matched per line.
type LogStrategy struct {
	options
	pattern     string
	isRegexp    bool
	occurrences int
}

// ForLog waits for a pattern to appear in the logs.
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

type terminalStreamStatus interface {
	Done() <-chan struct{}
	TerminalError() error
}

func scanLogStream(stream io.ReadCloser, match func(string) int, occurrences int) <-chan logScanResult {
	results := make(chan logScanResult, 1)
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		count := 0
		matched := false
		for scanner.Scan() {
			if matched {
				continue
			}
			count += match(scanner.Text())
			if count >= occurrences {
				// Keep draining after reporting a match. A process-backed
				// stream can have a terminal stderr diagnostic immediately
				// after the matching line; draining lets the stream retain
				// that cause before Close shuts the reader down.
				matched = true
				results <- logScanResult{found: true}
			}
		}
		if !matched {
			results <- logScanResult{err: scanner.Err()}
		}
	}()
	return results
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
		return fmt.Errorf("wait for log: %w", err)
	}
	// Closing the stream unblocks the scanner goroutine on timeout.
	defer stream.Close()

	results := scanLogStream(stream, match, s.occurrences)
	select {
	case result := <-results:
		return s.finishScan(ctx, target, stream, result, timeout)
	case <-ctx.Done():
		// Capture a terminal process error before Close marks the stream as
		// intentionally closed. Then close and give the scanner a chance to
		// publish any remaining read-side cause. A terminal CLI error is more
		// useful than replacing it with a generic timeout.
		terminalErr := terminalStreamError(stream)
		_ = stream.Close()
		// Close is the stream contract that unblocks the scanner. Receive
		// its result before classifying the context so a terminal CLI error
		// is not lost to an arbitrary timing fallback.
		result := <-results
		if terminalErr != nil {
			return terminalLogError(ctx, s.pattern, terminalErr)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if result.err != nil && !errors.Is(result.err, io.EOF) && !errors.Is(result.err, io.ErrClosedPipe) && !errors.Is(result.err, context.Canceled) {
				return wrapLogError(s.pattern, errors.Join(ctxErr, result.err))
			}
			return logContextError(ctx, s.pattern, timeout)
		}
		return s.finishScan(ctx, target, stream, result, timeout)
	}
}

func (s *LogStrategy) finishScan(ctx context.Context, target Target, stream io.ReadCloser, result logScanResult, timeout time.Duration) error {
	if result.found {
		if terminalErr := settleTerminalStream(stream); terminalErr != nil {
			return terminalLogError(ctx, s.pattern, terminalErr)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return logContextError(ctx, s.pattern, timeout)
		}
		return nil
	}

	// A process-backed stream can report its terminal CLI failure through
	// status even when the matching merged stderr line was observed first.
	// Check that status before probing container state or formatting a timeout.
	if terminalErr := terminalStreamError(stream); terminalErr != nil {
		return terminalLogError(ctx, s.pattern, terminalErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return logContextError(ctx, s.pattern, timeout)
	}
	if result.err != nil {
		return wrapLogError(s.pattern, result.err)
	}

	// The wait deadline already fired or the stream ended; check container
	// state with a bounded probe so a hung backend cannot stall diagnostics.
	// WithoutCancel detaches from the expired wait deadline, WithTimeout
	// re-bounds the probe.
	probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	running, rErr := target.Running(probeCtx)
	probeCancel()
	if rErr != nil {
		return wrapLogError(s.pattern, rErr)
	}
	if !running {
		return fmt.Errorf("wait for log %q: container stopped before pattern appeared", s.pattern)
	}
	return fmt.Errorf("wait for log %q: log stream ended before pattern appeared", s.pattern)
}

func terminalStreamError(stream io.ReadCloser) error {
	status, ok := stream.(terminalStreamStatus)
	if !ok {
		return nil
	}
	done := status.Done()
	if done == nil {
		return nil
	}
	timer := time.NewTimer(terminalSettleWindow)
	defer timer.Stop()
	select {
	case <-done:
		return normalizeTerminalError(status.TerminalError())
	case <-timer.C:
		return nil
	}
}

func settleTerminalStream(stream io.ReadCloser) error {
	return terminalStreamError(stream)
}

func normalizeTerminalError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func terminalLogError(ctx context.Context, pattern string, terminalErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(terminalErr, ctxErr) {
		return wrapLogError(pattern, errors.Join(terminalErr, ctxErr))
	}
	return wrapLogError(pattern, terminalErr)
}

func wrapLogError(pattern string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("wait for log %q: %w", pattern, err)
}

func logContextError(ctx context.Context, pattern string, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("wait for log %q: timed out after %v: %w", pattern, timeout, context.DeadlineExceeded)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("wait for log %q: %w", pattern, ctx.Err())
	}
	return fmt.Errorf("wait for log %q: timed out after %v", pattern, timeout)
}
