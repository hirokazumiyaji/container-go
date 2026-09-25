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
)

const (
	maxLogLineSize       = 1024 * 1024
	terminalSettleWindow = 5 * time.Millisecond
)

var errLogLineTooLong = errors.New("log line exceeds 1 MiB")

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

// WithOccurrence requires the pattern to appear n times. Occurrences
// are counted across reconnects after replayed history is de-duplicated.
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

type logScanResult struct {
	found       bool
	err         error
	terminalErr error
	count       int
	lines       [][sha256.Size]byte
}

type logReplay struct {
	previous [][sha256.Size]byte
	count    int
}

func (r logReplay) matchesPrevious(index int, line string) bool {
	if index >= len(r.previous) {
		return false
	}
	return r.previous[index] == sha256.Sum256([]byte(line))
}

// scanLogStream reads a complete stream unless the requested number of
// occurrences is found. A read error returned together with a matching
// final line is reported as an error rather than allowing that line to
// satisfy readiness.
func scanLogStream(ctx context.Context, stream io.ReadCloser, match func(string) int, occurrences int, replay logReplay) logScanResult {
	results := make(chan logScanResult, 1)
	go func() {
		reader := bufio.NewReader(stream)
		var lines [][sha256.Size]byte
		count := replay.count
		for {
			line, err := readLogLine(reader)
			if len(line) > 0 {
				index := len(lines)
				lines = append(lines, sha256.Sum256([]byte(line)))
				if !replay.matchesPrevious(index, line) {
					count += match(line)
				}
			}
			if err != nil {
				if isPermanentCheckError(err) || isTerminalStreamError(err) {
					results <- logScanResult{err: err, count: count, lines: lines}
					return
				}
				if errors.Is(err, io.EOF) {
					if count >= occurrences {
						results <- logScanResult{found: true, count: count, lines: lines}
					} else {
						results <- logScanResult{count: count, lines: lines}
					}
					return
				}
				results <- logScanResult{err: err, count: count, lines: lines}
				return
			}
			if count >= occurrences {
				terminalErr := settleLogReader(ctx, reader)
				results <- logScanResult{found: true, terminalErr: terminalErr, count: count, lines: lines}
				return
			}
		}
	}()

	if result, ok := receiveLogScanResult(results, ctx.Done()); ok {
		return result
	}
	return logScanResult{err: ctx.Err()}
}

func receiveLogScanResult(results <-chan logScanResult, done <-chan struct{}) (logScanResult, bool) {
	// Prefer a result queued before the context became ready. This keeps
	// a terminal CLI error visible when EOF and a deadline race.
	select {
	case result := <-results:
		return result, true
	default:
	}
	select {
	case result := <-results:
		return result, true
	case <-done:
		select {
		case result := <-results:
			return result, true
		default:
		}
	}
	return logScanResult{}, false
}

func readLogLine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxLogLineSize {
			return "", errLogLineTooLong
		}
		line = append(line, fragment...)
		if err == nil {
			return string(line), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(line), err
	}
}

func settleLogReader(ctx context.Context, reader *bufio.Reader) error {
	window := terminalSettleWindow
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err()
		}
		if remaining < window {
			window = remaining
		}
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		readErr <- err
	}()
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case err := <-readErr:
		if err == nil {
			return nil
		}
		if isPermanentCheckError(err) || isTerminalStreamError(err) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case <-timer.C:
		return nil
	}
}

func (s *LogStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := s.validate(); err != nil {
		return err
	}

	timeout, interval := s.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	what := fmt.Sprintf("wait for log %q", s.pattern)

	var match func(line string) int
	if s.isRegexp {
		re, err := regexp.Compile(s.pattern)
		if err != nil {
			return invalidConfigf("invalid log pattern %q: %v", s.pattern, err)
		}
		match = func(line string) int { return len(re.FindAllString(line, -1)) }
	} else {
		match = func(line string) int { return strings.Count(line, s.pattern) }
	}

	var previous [][sha256.Size]byte
	var count int
	var lastErr error
	for {
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr); terminalErr != nil {
			return terminalErr
		}

		stream, err := target.FollowLogs(waitCtx)
		if err != nil {
			if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr); terminalErr != nil {
				return terminalErr
			}
			return wrapWaitCause(what, err, lastErr)
		}

		result := scanLogStream(waitCtx, stream, match, s.occurrences, logReplay{previous: previous, count: count})
		if result.found {
			if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr); terminalErr != nil {
				_ = stream.Close()
				return terminalErr
			}
		}
		var terminalStreamErr error
		if result.found {
			terminalStreamErr = result.terminalErr
			if terminalStreamErr == nil {
				terminalStreamErr = terminalErrorIfSettled(waitCtx, stream)
			}
		}
		_ = stream.Close()
		if result.found {
			if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr); terminalErr != nil {
				return terminalErr
			}
			if terminalStreamErr != nil {
				return wrapWaitCause(what, terminalStreamErr, lastErr)
			}
			return nil
		}

		previous = result.lines
		count = result.count
		if result.err != nil {
			if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, result.err)); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(result.err) || isTerminalStreamError(result.err) {
				return wrapWaitCause(what, result.err, lastErr)
			}
			lastErr = joinNonNil(lastErr, result.err)
		}
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr); terminalErr != nil {
			return terminalErr
		}

		// A clean EOF (and a transient stream read error) may be
		// reconnectable. The probe is bounded by the existing wait context;
		// it never detaches from or extends the caller's budget.
		probeCtx, probeCancel := boundedProbeContext(waitCtx, 5*time.Second)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			return waitContextError(what, err, lastErr)
		}
		running, runningErr := target.Running(probeCtx)
		probeCancel()
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, runningErr)); terminalErr != nil {
			return terminalErr
		}
		if runningErr != nil {
			if isPermanentCheckError(runningErr) || isTerminalStreamError(runningErr) {
				return wrapWaitCause(what, runningErr, lastErr)
			}
			lastErr = joinNonNil(lastErr, runningErr)
		} else if !running {
			return waitStoppedError(what, joinNonNil(lastErr, result.err))
		}

		if err := waitForReconnect(waitCtx, interval); err != nil {
			return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr)
		}
	}
}

func boundedProbeContext(ctx context.Context, max time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.WithCancel(ctx)
		}
		if remaining < max {
			max = remaining
		}
	}
	return context.WithTimeout(ctx, max)
}

func terminalErrorIfSettled(ctx context.Context, stream io.ReadCloser) error {
	status, hasStatus := stream.(interface {
		Done() <-chan struct{}
		TerminalError() error
	})
	if !hasStatus || status.Done() == nil {
		// Generic readers are settled by settleLogReader while the
		// scanner still owns its bufio.Reader. Starting another Read here
		// would race with a timed-out read-ahead goroutine.
		return nil
	}

	window := terminalSettleWindow
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
	case <-status.Done():
		return status.TerminalError()
	case <-timer.C:
		// A live follow stream is expected to remain open. The caller
		// closes it after this bounded settling check.
		return nil
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
