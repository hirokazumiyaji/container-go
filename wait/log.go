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

// WithPollInterval sets the delay before reconnecting a log stream
// that ends before the pattern is found.
func (s *LogStrategy) WithPollInterval(d time.Duration) *LogStrategy {
	s.pollInterval = d
	return s
}

func (s *LogStrategy) validate() error {
	if err := s.options.validate(); err != nil {
		return err
	}
	if s.pattern == "" {
		return invalidConfigf("log pattern must not be empty")
	}
	if s.occurrences <= 0 {
		return invalidConfigf("log occurrence count must be positive")
	}
	if s.isRegexp {
		if _, err := regexp.Compile(s.pattern); err != nil {
			return invalidConfigf("invalid log pattern %q: %v", s.pattern, err)
		}
	}
	return nil
}

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := s.validate(); err != nil {
		return err
	}

	timeout, interval := s.effective()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var match func(line string) int
	if s.isRegexp {
		re, err := regexp.Compile(s.pattern)
		if err != nil {
			// validate compiled this already; keep the error classified as
			// a configuration failure if the strategy is mutated later.
			return invalidConfigf("invalid log pattern %q: %v", s.pattern, err)
		}
		match = func(line string) int { return len(re.FindAllString(line, -1)) }
	} else {
		match = func(line string) int { return strings.Count(line, s.pattern) }
	}

	count := 0
	for {
		stream, err := target.FollowLogs(ctx)
		if err != nil {
			return fmt.Errorf("wait for log %q: %w", s.pattern, err)
		}
		found, scanErr := scanLogStream(ctx, stream, match, &count, s.occurrences)
		// Closing the stream unblocks the scanner goroutine on timeout or
		// after a reconnect.
		_ = stream.Close()
		if found {
			return nil
		}
		if isPermanentCheckError(scanErr) {
			return fmt.Errorf("wait for log %q: %w", s.pattern, scanErr)
		}
		if ctx.Err() != nil {
			return logContextError(ctx, s.pattern, timeout)
		}

		// A log stream can end while the container is still running. Keep
		// the configured poll interval meaningful by reconnecting instead
		// of treating the first EOF as a permanent readiness failure.
		probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		running, runningErr := target.Running(probeCtx)
		probeCancel()
		if isPermanentCheckError(runningErr) {
			return fmt.Errorf("wait for log %q: %w", s.pattern, runningErr)
		}
		if runningErr == nil && !running {
			return fmt.Errorf("wait for log %q: container stopped before pattern appeared", s.pattern)
		}

		if err := waitForReconnect(ctx, interval); err != nil {
			return logContextError(ctx, s.pattern, timeout)
		}
	}
}

func scanLogStream(ctx context.Context, stream io.ReadCloser, match func(string) int, count *int, occurrences int) (bool, error) {
	found := make(chan struct{})
	scanDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			*count += match(scanner.Text())
			if *count >= occurrences {
				close(found)
				return
			}
		}
		scanDone <- scanner.Err()
	}()

	select {
	case <-found:
		return true, nil
	case err := <-scanDone:
		return false, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func waitForReconnect(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logContextError(ctx context.Context, pattern string, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("wait for log %q: %w", pattern, context.Canceled)
	}
	return logTimeoutError{pattern: pattern, timeout: timeout}
}

type logTimeoutError struct {
	pattern string
	timeout time.Duration
}

func (e logTimeoutError) Error() string {
	return fmt.Sprintf("wait for log %q: timed out after %v", e.pattern, e.timeout)
}

func (e logTimeoutError) Unwrap() []error { return []error{context.DeadlineExceeded} }
