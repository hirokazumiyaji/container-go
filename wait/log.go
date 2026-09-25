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

type logScanResult struct {
	matches int
	err     error
}

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	timeout, interval := s.effective()
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

	what := fmt.Sprintf("wait for log %q", s.pattern)
	var lastCheckErr, lastStateErr error
	state, err := targetState(ctx, target)
	if err != nil {
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ctx.Err() == nil {
			lastStateErr = err
		}
	} else if terminalWaitState(state) {
		return stateFailure(what, state, lastCheckErr, lastStateErr)
	}
	matches := 0
	for {
		if ctx.Err() != nil {
			return logWaitEnded(what, timeout, ctx.Err(), lastCheckErr, lastStateErr)
		}

		stream, err := target.FollowLogs(ctx)
		if err != nil {
			if permanentProbeError(err) {
				return fmt.Errorf("%s: %w", what, err)
			}
			if ctx.Err() == nil {
				lastCheckErr = fmt.Errorf("open log stream: %w", err)
			}
		} else {
			scanResult, streamErr := s.scanStream(ctx, stream, match, target, what, &lastStateErr)
			_ = stream.Close()
			if streamErr != nil {
				return streamErr
			}
			matches += scanResult.matches
			if matches >= s.occurrences {
				return nil
			}
			if ctx.Err() != nil {
				return logWaitEnded(what, timeout, ctx.Err(), lastCheckErr, lastStateErr)
			}
			if scanResult.err == nil {
				scanResult.err = io.EOF
			}
			if permanentProbeError(scanResult.err) {
				return fmt.Errorf("%s: %w", what, scanResult.err)
			}
			lastCheckErr = fmt.Errorf("read log stream: %w", scanResult.err)
		}

		// Opening or reading can fail while the lifecycle CLI is briefly
		// unavailable. Reclassify every failure so a stopped container or
		// permanent disappearance wins over the generic stream error, then
		// retry transient open errors and EOF until the startup deadline.
		if ctx.Err() == nil {
			state, stateErr := targetState(ctx, target)
			if stateErr != nil {
				if permanentProbeError(stateErr) {
					return fmt.Errorf("%s: %w", what, stateErr)
				}
				lastStateErr = stateErr
			} else if terminalWaitState(state) {
				return stateFailure(what, state, lastCheckErr, lastStateErr)
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return logWaitEnded(what, timeout, ctx.Err(), lastCheckErr, lastStateErr)
		case <-timer.C:
		}
	}
}

func (s *LogStrategy) scanStream(
	ctx context.Context,
	stream io.ReadCloser,
	match func(string) int,
	target Target,
	what string,
	lastStateErr *error,
) (logScanResult, error) {
	results := make(chan logScanResult, 1)
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		matches := 0
		for scanner.Scan() {
			matches += match(scanner.Text())
			if matches >= s.occurrences {
				break
			}
		}
		results <- logScanResult{matches: matches, err: scanner.Err()}
	}()

	ticker := time.NewTicker(stateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case result := <-results:
			return result, nil
		case <-ticker.C:
			state, err := targetState(ctx, target)
			if err != nil {
				if permanentProbeError(err) {
					return logScanResult{}, fmt.Errorf("%s: %w", what, err)
				}
				*lastStateErr = err
				continue
			}
			if terminalWaitState(state) {
				return logScanResult{}, stateFailure(what, state, nil, *lastStateErr)
			}
		case <-ctx.Done():
			return logScanResult{err: ctx.Err()}, nil
		}
	}
}

func logWaitEnded(what string, timeout time.Duration, waitErr error, checkErr, stateErr error) error {
	if errors.Is(waitErr, context.Canceled) {
		return fmt.Errorf("%s: %w%s", what, context.Canceled, diagnosticSuffix(checkErr, stateErr))
	}
	return fmt.Errorf("%s: timed out after %v%s", what, timeout, diagnosticSuffix(checkErr, stateErr))
}
