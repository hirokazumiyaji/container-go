package wait

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
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

// ForLog waits for a substring to appear in the logs. Patterns and individual
// log lines are bounded; oversized inputs are reported as matcher errors.
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

// DiagnosticValues returns the log matcher that may be echoed in a failure.
func (s *LogStrategy) DiagnosticValues() []string { return []string{s.pattern} }

// DiagnosticSecrets is an alias for DiagnosticValues.
func (s *LogStrategy) DiagnosticSecrets() []string { return s.DiagnosticValues() }

const maxLogLineSize = 8 * 1024 * 1024

var (
	errLogLineTooLong    = errors.New("log line exceeds bounded matcher size")
	errLogPatternTooLong = errors.New("log pattern exceeds bounded matcher size")
)

type streamingLogMatcher struct {
	pattern     string
	re          *regexp.Regexp
	occurrences int
	count       int
	line        []byte
	lineOpen    bool
}

func newStreamingLogMatcher(pattern string, isRegexp bool, occurrences int) (*streamingLogMatcher, error) {
	if len(pattern) > maxLogLineSize {
		return nil, errLogPatternTooLong
	}
	if occurrences < 1 {
		occurrences = 1
	}
	m := &streamingLogMatcher{pattern: pattern, occurrences: occurrences}
	if isRegexp {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		m.re = re
	}
	return m, nil
}

func (m *streamingLogMatcher) write(data []byte) error {
	for len(data) > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			if err := m.appendLine(data[:i]); err != nil {
				return err
			}
			if err := m.finishLine(); err != nil {
				return err
			}
			data = data[i+1:]
			m.line = m.line[:0]
			m.lineOpen = false
			continue
		}
		if err := m.appendLine(data); err != nil {
			return err
		}
		return nil
	}
	return nil
}

func (m *streamingLogMatcher) appendLine(data []byte) error {
	if len(m.line)+len(data) > maxLogLineSize {
		return errLogLineTooLong
	}
	m.line = append(m.line, data...)
	m.lineOpen = true
	return nil
}

func (m *streamingLogMatcher) finishLine() error {
	if !m.lineOpen {
		return nil
	}
	if m.re == nil {
		m.count += bytes.Count(m.line, []byte(m.pattern))
	} else {
		m.count += len(m.re.FindAllIndex(m.line, -1))
	}
	m.lineOpen = false
	return nil
}

func (m *streamingLogMatcher) found() bool { return m.count >= m.occurrences }

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	timeout, _ := s.effective()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	matcher, err := newStreamingLogMatcher(s.pattern, s.isRegexp, s.occurrences)
	if err != nil {
		return safeDiagnosticError(fmt.Errorf("wait for log: %w", err), s.pattern)
	}

	stream, err := target.FollowLogs(ctx)
	if err != nil {
		return safeDiagnosticError(fmt.Errorf("wait for log: %w", err), s.pattern)
	}
	// Closing the stream unblocks the reader goroutine on timeout.
	defer stream.Close()

	found := make(chan struct{})
	scanDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := stream.Read(buf)
			if n > 0 {
				if err := matcher.write(buf[:n]); err != nil {
					scanDone <- err
					return
				}
				if matcher.found() {
					close(found)
					return
				}
			}
			if readErr != nil {
				if err := matcher.finishLine(); err != nil {
					scanDone <- err
					return
				}
				if matcher.found() {
					close(found)
					return
				}
				scanDone <- readErr
				return
			}
		}
	}()

	select {
	case <-found:
		return nil
	case readErr := <-scanDone:
		// The wait deadline already fired or the stream ended; check
		// container state with a bounded probe so a hung backend
		// cannot stall diagnostics. WithoutCancel detaches from the
		// expired wait deadline, WithTimeout re-bounds the probe.
		probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		running, rErr := target.Running(probeCtx)
		probeCancel()
		if rErr == nil && !running {
			return safeDiagnosticError(fmt.Errorf("wait for log %q: container stopped before pattern appeared", s.pattern), s.pattern)
		}
		if readErr == nil || readErr == io.EOF {
			return safeDiagnosticError(fmt.Errorf("wait for log %q: log stream ended before pattern appeared", s.pattern), s.pattern)
		}
		return safeDiagnosticError(fmt.Errorf("wait for log %q: log stream ended before pattern appeared (read error: %w)", s.pattern, readErr), s.pattern)
	case <-ctx.Done():
		return safeDiagnosticError(fmt.Errorf("wait for log %q: timed out after %v", s.pattern, timeout), s.pattern)
	}
}
