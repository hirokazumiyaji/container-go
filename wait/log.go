package wait

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// LogStrategy waits until a pattern appears in the container's log
// stream. Patterns are matched per line. If the stream ends first,
// readiness is not implied: the strategy probes whether the container is
// stopped and otherwise reports the stream ending before the pattern.
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

	found := make(chan struct{})
	scanDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		count := 0
		for scanner.Scan() {
			count += match(scanner.Text())
			if count >= s.occurrences {
				close(found)
				return
			}
		}
		scanDone <- scanner.Err()
	}()

	select {
	case <-found:
		return nil
	case err := <-scanDone:
		// The wait deadline already fired or the stream ended; check
		// container state with a bounded probe so a hung backend
		// cannot stall diagnostics. WithoutCancel detaches from the
		// expired wait deadline, WithTimeout re-bounds the probe.
		probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		running, rErr := target.Running(probeCtx)
		probeCancel()
		if rErr == nil && !running {
			return fmt.Errorf("wait for log %q: container stopped before pattern appeared", s.pattern)
		}
		return fmt.Errorf("wait for log %q: log stream ended before pattern appeared (read error: %v)", s.pattern, err)
	case <-ctx.Done():
		return fmt.Errorf("wait for log %q: timed out after %v", s.pattern, timeout)
	}
}
