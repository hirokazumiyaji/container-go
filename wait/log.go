package wait

import (
	"bytes"
	"context"
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

// DiagnosticValues returns the log matcher that may be echoed in a failure.
func (s *LogStrategy) DiagnosticValues() []string { return []string{s.pattern} }

// DiagnosticSecrets is an alias for DiagnosticValues.
func (s *LogStrategy) DiagnosticSecrets() []string { return s.DiagnosticValues() }

const maxLogMatchOverlap = 64 * 1024

type streamingLogMatcher struct {
	pattern      string
	re           *regexp.Regexp
	occurrences  int
	count        int
	carry        []byte
	position     int
	countedStart int
	countedEnd   int
	haveMatch    bool
	lineOpen     bool
}

func newStreamingLogMatcher(pattern string, isRegexp bool, occurrences int) (*streamingLogMatcher, error) {
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

func (m *streamingLogMatcher) write(data []byte) {
	for len(data) > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			m.lineOpen = true
			m.writeSegment(data[:i])
			m.finishLine()
			data = data[i+1:]
			m.position++
			m.carry = m.carry[:0]
			m.countedStart = m.position
			m.countedEnd = m.position
			m.haveMatch = false
			continue
		}
		m.writeSegment(data)
		return
	}
}

func (m *streamingLogMatcher) writeSegment(segment []byte) {
	if len(segment) == 0 {
		return
	}
	m.lineOpen = true
	if m.re == nil {
		overlap := len(m.pattern) - 1
		if overlap < 0 {
			overlap = 0
		}
		if overlap > maxLogMatchOverlap {
			overlap = maxLogMatchOverlap
		}
		combined := make([]byte, 0, len(m.carry)+len(segment))
		combined = append(combined, m.carry...)
		combined = append(combined, segment...)
		base := m.position - len(m.carry)
		for searchFrom := 0; searchFrom <= len(combined); {
			index := bytes.Index(combined[searchFrom:], []byte(m.pattern))
			if index < 0 {
				break
			}
			index += searchFrom
			m.recordMatch(base+index, base+index+len(m.pattern))
			if m.pattern == "" {
				searchFrom = index + 1
			} else {
				searchFrom = index + len(m.pattern)
			}
		}
		m.position += len(segment)
		if overlap == 0 {
			m.carry = m.carry[:0]
		} else if len(combined) > overlap {
			m.carry = append(m.carry[:0], combined[len(combined)-overlap:]...)
		} else {
			m.carry = append(m.carry[:0], combined...)
		}
		return
	}

	overlap := maxLogMatchOverlap
	if len(m.carry) > overlap {
		m.carry = m.carry[len(m.carry)-overlap:]
	}
	combined := make([]byte, 0, len(m.carry)+len(segment))
	combined = append(combined, m.carry...)
	combined = append(combined, segment...)
	base := m.position - len(m.carry)
	for _, match := range m.re.FindAllIndex(combined, -1) {
		if match[1] == len(combined) {
			// An end-anchored or extensible match is not stable until more
			// line data arrives or the line is known to have ended.
			continue
		}
		m.recordMatch(base+match[0], base+match[1])
	}
	m.position += len(segment)
	if len(combined) > overlap {
		m.carry = append(m.carry[:0], combined[len(combined)-overlap:]...)
	} else {
		m.carry = append(m.carry[:0], combined...)
	}
}

func (m *streamingLogMatcher) finishLine() {
	if !m.lineOpen {
		return
	}
	if m.re != nil {
		base := m.position - len(m.carry)
		for _, match := range m.re.FindAllIndex(m.carry, -1) {
			m.recordMatch(base+match[0], base+match[1])
		}
	}
	m.lineOpen = false
}

func (m *streamingLogMatcher) recordMatch(start, end int) {
	if m.haveMatch && (start <= m.countedStart || end <= m.countedEnd) {
		return
	}
	m.count++
	m.countedStart = start
	m.countedEnd = end
	m.haveMatch = true
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
				matcher.write(buf[:n])
				if matcher.found() {
					close(found)
					return
				}
			}
			if readErr != nil {
				matcher.finishLine()
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
