package wait

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ErrLogStreamSetup identifies deterministic log-stream setup failures
// that readiness strategies must not retry.
var ErrLogStreamSetup = cli.ErrStreamSetup

// LogStrategy waits until a pattern appears in the container's log
// stream. Patterns are matched per line.
//
// Log replay follows Target.FollowLogs' full-history contract: every
// connection starts at the first currently retained log line, and history
// is treated as append-only for the duration of one wait. Empty or partial
// reconnects do not move the occurrence cursor; a later full replay must
// catch up before new lines are counted. A completed replay that has the
// same length but different content is treated as a replacement baseline,
// covering backends that rotate history between reconnects.
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
	replay  logReplay
}

// logReplay is a fixed-size cursor over the longest complete log history
// observed so far. The digest is advanced across reconnects; retaining a
// fingerprint for every historical line is neither necessary nor bounded.
type logReplay struct {
	matches  int
	complete bool
	digest   [sha256.Size]byte
	lines    uint64
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
	if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
		return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
	}
	if err != nil {
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		lastStateErr = err
	} else if terminalWaitState(state) {
		return stateFailure(what, state, lastCheckErr, lastStateErr)
	}

	replay := logReplay{}
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
			scanResult, streamErr := s.scanStream(waitCtx, stream, match, replay, target, what, &lastStateErr)
			if streamErr != nil {
				_ = stream.Close()
				return streamErr
			}
			if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
				_ = stream.Close()
				return logContextScanError(callerCtx, waitCtx, what, timeout, lastCheckErr, scanResult.err)
			}
			replay = scanResult.replay
			matches := replay.matches
			// A terminal stream error wins over a matching line that may
			// have been returned in the same read.
			if scanResult.err != nil && permanentLogStreamError(scanResult.err) {
				_ = stream.Close()
				return fmt.Errorf("%s: %w", what, scanResult.err)
			}
			if matches >= s.occurrences {
				terminalErr := terminalLogStreamError(waitCtx, stream)
				_ = stream.Close()
				if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
					return logContextScanError(callerCtx, waitCtx, what, timeout, lastCheckErr, terminalErr)
				}
				if terminalErr != nil {
					return fmt.Errorf("%s: %w", what, terminalErr)
				}
				finalErr := finalLifecycleCheck(waitCtx, target, what)
				if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
					return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, finalErr)
				}
				if finalErr != nil {
					return finalErr
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
			if terminalErr := logContextTermination(callerCtx, waitCtx); terminalErr != nil {
				return logWaitEnded(callerCtx, waitCtx, what, timeout, lastCheckErr, lastStateErr)
			}
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
		scanner.Split(scanCompleteLogLine)
		digest := sha256.New()
		var digestSum [sha256.Size]byte
		var lineLength [8]byte
		var lineCount uint64
		next := replay
		divergentMatches := 0
		reachedBaseline := replay.complete && replay.lines == 0
		ended := true
		for scanner.Scan() {
			line := scanner.Text()
			binary.LittleEndian.PutUint64(lineLength[:], uint64(len(line)))
			_, _ = digest.Write(lineLength[:])
			_, _ = digest.Write([]byte(line))
			lineCount++
			currentDigest := [sha256.Size]byte(digest.Sum(digestSum[:0]))
			countLine := !replay.complete || reachedBaseline
			if replay.complete && !reachedBaseline && lineCount == replay.lines && currentDigest == replay.digest {
				reachedBaseline = true
				countLine = false
			}
			if countLine {
				next.matches += match(line)
			} else {
				divergentMatches += match(line)
			}
			if next.matches >= s.occurrences {
				ended = false
				if settleErr := settleScanner(ctx, scanner); settleErr != nil && !errors.Is(settleErr, io.EOF) {
					results <- logScanResult{err: settleErr, replay: replay}
					return
				}
				break
			}
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			// A failed transport may have delivered only a suffix of the
			// retained history. Commit neither its occurrence count nor
			// its replay baseline; the next complete connection is the
			// first trustworthy observation.
			results <- logScanResult{err: scanErr, replay: replay}
			return
		}
		replaceHistory := ended && replay.complete && !reachedBaseline && lineCount > 0 && lineCount == replay.lines
		if replaceHistory {
			next.matches += divergentMatches
		}
		if ended && scanErr == nil && (!replay.complete || reachedBaseline || replaceHistory) {
			next.complete = true
			next.digest = [sha256.Size]byte(digest.Sum(digestSum[:0]))
			next.lines = lineCount
		}
		results <- logScanResult{matches: next.matches, err: scanErr, replay: next}
	}()

	ticker := time.NewTicker(stateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case result := <-results:
			return result, nil
		case <-ticker.C:
			state, err := targetState(ctx, target)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return logScanResult{err: ctxErr}, nil
			}
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
			if result, ok := scanResultOnContextDone(results); ok {
				return result, nil
			}
			return logScanResult{err: ctx.Err()}, nil
		}
	}
}

// scanCompleteLogLine emits only newline-terminated log records. At EOF an
// unterminated suffix may be a truncated write; consume it without adding it
// to replay state so a later full-history connection can observe its
// completed form and any following ready line.
func scanCompleteLogLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		line := data[:i]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return i + 1, line, nil
	}
	if atEOF && len(data) > 0 {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

func permanentLogStreamError(err error) bool {
	if permanentProbeError(err) || errors.Is(err, ErrLogStreamSetup) || errors.Is(err, bufio.ErrTooLong) {
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
		if err, ok := scannerErrorOnContextDone(result); ok {
			return err
		}
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
	case <-ctx.Done():
		select {
		case <-done:
			return status.TerminalError()
		default:
			return ctx.Err()
		}
	case <-timer.C:
		return nil
	}
}

func scannerErrorOnContextDone(results <-chan error) (error, bool) {
	select {
	case err := <-results:
		return err, true
	default:
		return nil, false
	}
}

func scanResultOnContextDone(results <-chan logScanResult) (logScanResult, bool) {
	select {
	case result := <-results:
		return result, true
	default:
		return logScanResult{}, false
	}
}

func logContextScanError(callerCtx, waitCtx context.Context, what string, timeout time.Duration, checkErr, scanErr error) error {
	contextErr := logContextTermination(callerCtx, waitCtx)
	if contextErr == nil {
		return nil
	}
	if scanErr != nil && permanentLogStreamError(scanErr) {
		return fmt.Errorf("%s: %w", what, errors.Join(scanErr, contextErr))
	}
	if scanErr != nil {
		checkErr = joinNonNil(checkErr, fmt.Errorf("read log stream: %w", scanErr))
	}
	return logWaitEnded(callerCtx, waitCtx, what, timeout, checkErr, nil)
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
