package wait

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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
	lines   [][sha256.Size]byte
}

type logReplay struct {
	previous [][sha256.Size]byte
	matches  int
}

func (r logReplay) matchesPrevious(index int, line string) bool {
	if index >= len(r.previous) {
		return false
	}
	return r.previous[index] == sha256.Sum256([]byte(line))
}

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	timeout, interval := s.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
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
	if err := callerCtx.Err(); err != nil {
		return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
	}
	state, err := targetState(waitCtx, target)
	if err != nil {
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if waitCtx.Err() == nil {
			lastStateErr = err
		}
	} else if terminalWaitState(state) {
		return stateFailure(what, state, lastCheckErr, lastStateErr)
	}

	var previous [][sha256.Size]byte
	matches := 0
	for {
		if err := callerCtx.Err(); err != nil {
			return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
		}
		if err := waitCtx.Err(); err != nil {
			return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
		}

		stream, err := target.FollowLogs(waitCtx)
		if err != nil {
			if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
				return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
			}
			if permanentLogStreamError(err) {
				return fmt.Errorf("%s: %w", what, err)
			}
			if waitCtx.Err() == nil {
				lastCheckErr = fmt.Errorf("open log stream: %w", err)
			}
		} else {
			scanResult, streamErr := s.scanStream(waitCtx, stream, match, logReplay{
				previous: previous,
				matches:  matches,
			}, target, what, &lastStateErr)
			if streamErr != nil {
				_ = stream.Close()
				return streamErr
			}
			if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
				_ = stream.Close()
				return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
			}
			previous = scanResult.lines
			matches = scanResult.matches
			// A terminal stream error wins over a matching line that may
			// have been returned in the same read.
			if scanResult.err != nil && permanentLogStreamError(scanResult.err) {
				_ = stream.Close()
				return fmt.Errorf("%s: %w", what, scanResult.err)
			}
			if matches >= s.occurrences {
				terminalErr := terminalLogStreamError(waitCtx, stream)
				_ = stream.Close()
				if terminalErr != nil {
					return fmt.Errorf("%s: %w", what, terminalErr)
				}
				return nil
			}
			if scanResult.err == nil {
				scanResult.err = io.EOF
			}
			if permanentLogStreamError(scanResult.err) {
				_ = stream.Close()
				return fmt.Errorf("%s: %w", what, scanResult.err)
			}
			lastCheckErr = fmt.Errorf("read log stream: %w", scanResult.err)
			_ = stream.Close()
		}

		// Opening or reading can fail while the lifecycle CLI is briefly
		// unavailable. Reclassify every failure so a stopped container or
		// permanent disappearance wins over the generic stream error, then
		// retry transient open errors and EOF until the startup deadline.
		if waitCtx.Err() == nil {
			state, stateErr := targetState(waitCtx, target)
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
		case <-waitCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
		case <-timer.C:
		}
	}
}

func (s *LogStrategy) scanStream(
	ctx context.Context,
	stream io.ReadCloser,
	match func(string) int,
	replay logReplay,
	target Target,
	what string,
	lastStateErr *error,
) (logScanResult, error) {
	results := make(chan logScanResult, 1)
	go func() {
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var lines [][sha256.Size]byte
		matches := replay.matches
		for scanner.Scan() {
			line := scanner.Text()
			index := len(lines)
			lines = append(lines, sha256.Sum256([]byte(line)))
			if !replay.matchesPrevious(index, line) {
				matches += match(line)
			}
			if matches >= s.occurrences {
				if settleErr := settleScanner(ctx, scanner); settleErr != nil && !errors.Is(settleErr, io.EOF) {
					results <- logScanResult{matches: matches, err: settleErr, lines: lines}
					return
				}
				break
			}
		}
		results <- logScanResult{matches: matches, err: scanner.Err(), lines: lines}
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

func permanentLogStreamError(err error) bool {
	if permanentProbeError(err) || errors.Is(err, bufio.ErrTooLong) {
		return true
	}
	var cliErr *cli.CLIError
	return errors.As(err, &cliErr)
}

func settleScanner(ctx context.Context, scanner *bufio.Scanner) error {
	result := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			// A readiness line was already found; drain briefly to let a
			// terminal stream error that follows it become observable.
		}
		result <- scanner.Err()
	}()
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func terminalLogStreamError(ctx context.Context, stream io.ReadCloser) error {
	status, ok := stream.(interface {
		Done() <-chan struct{}
		TerminalError() error
	})
	if !ok {
		return nil
	}
	done := status.Done()
	if done == nil {
		return nil
	}
	window := 5 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err()
		}
		if remaining < window {
			window = remaining
		}
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-done:
		return status.TerminalError()
	case <-timer.C:
		return nil
	}
}

func logContextTermination(callerCtx, waitCtx context.Context) error {
	if err := callerCtx.Err(); err != nil {
		return err
	}
	return waitCtx.Err()
}

func logWaitEnded(callerCtx, waitCtx context.Context, what string, timeout time.Duration, checkErr, stateErr error) error {
	if err := callerCtx.Err(); err != nil {
		return newWaitError(fmt.Sprintf("%s: %s", what, err)+diagnosticSuffix(checkErr, stateErr), err, checkErr, stateErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return newWaitError(fmt.Sprintf("%s: %s", what, context.Canceled)+diagnosticSuffix(checkErr, stateErr), context.Canceled, checkErr, stateErr)
		}
		return newWaitError(fmt.Sprintf("%s: timed out after %v", what, timeout)+diagnosticSuffix(checkErr, stateErr), context.DeadlineExceeded, checkErr, stateErr)
	}
	return newWaitError(fmt.Sprintf("%s: timed out after %v", what, timeout)+diagnosticSuffix(checkErr, stateErr), context.DeadlineExceeded, checkErr, stateErr)
}
