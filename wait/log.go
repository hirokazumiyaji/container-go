package wait

import (
	"bufio"
	"bytes"
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
	maxLogLineSize    = 1024 * 1024
	maxLogReplayLines = 4096
	// terminalSettleWindow is the idle grace period after the scanner has
	// consumed pending output. maxLogSettleWindow bounds a continuously
	// active live --follow stream so readiness cannot wait forever.
	terminalSettleWindow = 100 * time.Millisecond
	maxLogSettleWindow   = 250 * time.Millisecond
)

var errLogLineTooLong = errors.New("log line exceeds 1 MiB")

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

// WithOccurrence requires the pattern to appear n times. Occurrences
// are counted across reconnects after replayed history is de-duplicated.
// A final unterminated line at clean EOF is treated as a complete line.
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
			return &ConfigError{
				Strategy: "ForLog",
				Field:    "pattern",
				Value:    s.pattern,
				Reason:   err.Error(),
			}
		}
	}
	return nil
}

type logScanResult struct {
	found       bool
	committed   bool
	err         error
	terminalErr error
	state       State
	count       int
	lines       [][sha256.Size]byte
	lineStart   int
	lineCount   int
	partial     []byte
}

type logReplay struct {
	previous      [][sha256.Size]byte
	previousStart int
	previousLines int
	count         int
	partial       []byte
}

// matchesPrevious reports whether index is in the replayed prefix. Only a
// rolling tail of line fingerprints is retained; older positions are part
// of the replay under FollowLogs' append-only history contract.
func (r logReplay) matchesPrevious(index int, line [sha256.Size]byte) bool {
	if index >= r.previousLines || len(r.previous) == 0 {
		return false
	}
	tailStart := r.previousLines - len(r.previous)
	if index < tailStart {
		return true
	}
	position := (r.previousStart + index - tailStart) % len(r.previous)
	return r.previous[position] == line
}

func appendReplayLine(lines [][sha256.Size]byte, start *int, line [sha256.Size]byte) [][sha256.Size]byte {
	if len(lines) < maxLogReplayLines {
		return append(lines, line)
	}
	lines[*start] = line
	*start = (*start + 1) % maxLogReplayLines
	return lines
}

// scanLogStream reads Scanner-like logical lines unless the requested number
// of occurrences is found. Only a clean EOF commits replay, count, and
// partial state; a non-EOF transport error rolls the scan back.
func scanLogStream(ctx context.Context, stream io.ReadCloser, match func(string) int, occurrences int, replay logReplay) logScanResult {
	return scanLogStreamTarget(ctx, nil, stream, match, occurrences, replay)
}

func scanLogStreamTarget(ctx context.Context, target Target, stream io.ReadCloser, match func(string) int, occurrences int, replay logReplay) logScanResult {
	results := make(chan logScanResult, 1)
	go func() {
		reader := bufio.NewReader(stream)
		var lines [][sha256.Size]byte
		partial := replay.partial
		lineStart := 0
		observedLines := 0
		lineCount := replay.previousLines
		count := replay.count
		replaying := true
		result := func(found, committed bool, scanErr, terminalErr error) logScanResult {
			res := logScanResult{
				found:       found,
				committed:   committed,
				err:         scanErr,
				terminalErr: terminalErr,
				count:       count,
				lines:       lines,
				lineStart:   lineStart,
				lineCount:   lineCount,
				partial:     partial,
			}
			if !committed && !found {
				res.count = replay.count
				res.lines = replay.previous
				res.lineStart = replay.previousStart
				res.lineCount = replay.previousLines
				res.partial = replay.partial
			}
			return res
		}
		for {
			line, complete, err := readLogLine(reader)
			if complete {
				hash := sha256.Sum256(line)
				if replaying && !replay.matchesPrevious(observedLines, hash) {
					replaying = false
					lineCount = observedLines
				}
				canCount := !replaying && (replay.count == 0 || observedLines >= replay.previousLines)
				if canCount {
					logical, reconcileErr := reconcileLogLine(partial, line)
					if reconcileErr != nil {
						results <- result(false, false, reconcileErr, nil)
						return
					}
					partial = nil
					count += match(string(logical))
					lineCount++
					hash = sha256.Sum256(logical)
				}
				lines = appendReplayLine(lines, &lineStart, hash)
				observedLines++
			} else if line != nil {
				var reconcileErr error
				partial, reconcileErr = reconcileLogLine(partial, line)
				if reconcileErr != nil {
					results <- result(false, false, reconcileErr, nil)
					return
				}
			}
			if err != nil {
				if isPermanentCheckError(err) || isTerminalStreamError(err) {
					results <- result(false, false, err, nil)
					return
				}
				if errors.Is(err, io.EOF) {
					committed := observedLines >= replay.previousLines
					if count >= occurrences {
						terminalErr := settleLogMatch(ctx, stream, reader)
						results <- result(true, committed, nil, terminalErr)
					} else {
						results <- result(false, committed, nil, nil)
					}
					return
				}
				results <- result(false, false, err, nil)
				return
			}
			if count >= occurrences {
				terminalErr := settleLogMatch(ctx, stream, reader)
				results <- result(true, false, nil, terminalErr)
				return
			}
		}
	}()

	if result, ok := receiveLogScanResult(ctx, target, results, ctx.Done()); ok {
		return result
	}
	terminalErr := settledTerminalError(stream)
	return logScanResult{
		err:         joinNonNil(ctx.Err(), terminalErr),
		terminalErr: terminalErr,
	}
}

func settledTerminalError(stream io.ReadCloser) error {
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
	select {
	case <-done:
		return status.TerminalError()
	default:
		return nil
	}
}

func receiveLogScanResult(ctx context.Context, target Target, results <-chan logScanResult, done <-chan struct{}) (logScanResult, bool) {
	// Prefer a result queued before the context became ready. This keeps
	// a terminal CLI error visible when EOF and a deadline race.
	select {
	case result := <-results:
		return result, true
	default:
	}

	var tickerChan <-chan time.Time
	if target != nil {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		tickerChan = ticker.C
	}

	for {
		select {
		case result := <-results:
			return result, true
		case <-tickerChan:
			probeCtx, probeCancel := context.WithTimeout(ctx, lifecycleProbeTimeout)
			state, err := targetState(probeCtx, target)
			probeCancel()
			if ctx.Err() == nil && err == nil && terminalWaitState(state) {
				return logScanResult{state: state}, true
			}
		case <-done:
			select {
			case result := <-results:
				return result, true
			default:
			}
			return logScanResult{}, false
		}
	}
}

// readLogLine returns bufio.ScanLines-compatible text: a line terminator is
// removed, CRLF drops its carriage return, and data before a clean EOF is a
// final token. A nil line with a non-EOF error is an incomplete transport
// fragment and is not a logical line.
func readLogLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxLogLineSize {
			return nil, false, errLogLineTooLong
		}
		line = append(line, fragment...)
		if err == nil {
			return dropLogLineTerminator(line), true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if line == nil {
				return nil, false, err
			}
			return dropLogLineTerminator(line), true, err
		}
		return line, false, err
	}
}

func dropLogLineTerminator(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line
}

// reconcileLogLine carries an incomplete transport prefix into a completed
// logical line. A replayed line already starts with that prefix, so it
// replaces the carried bytes rather than duplicating them.
func reconcileLogLine(partial, observed []byte) ([]byte, error) {
	if len(partial) > 0 && bytes.HasPrefix(observed, partial) {
		return observed, nil
	}
	if len(partial)+len(observed) > maxLogLineSize {
		return nil, errLogLineTooLong
	}
	line := make([]byte, 0, len(partial)+len(observed))
	line = append(line, partial...)
	return append(line, observed...), nil
}

type logSettleRead struct {
	n   int
	err error
}

func nextReadyLogSettle(readResults <-chan logSettleRead, done <-chan struct{}) (logSettleRead, bool, bool) {
	select {
	case read := <-readResults:
		return read, true, false
	default:
	}
	select {
	case <-done:
		return logSettleRead{}, false, true
	default:
	}
	return logSettleRead{}, false, false
}

// settleLogMatch actively consumes merged output after a readiness match.
// A single delayed read leaves a public io.Pipe blocked, which can prevent
// the follow process from exiting and hide its terminal diagnostic. The
// scanner therefore keeps reading until the process reports Done, the
// stream returns a terminal error, the caller ends, or the bounded live-
// stream grace period expires.
func settleLogMatch(ctx context.Context, stream io.ReadCloser, reader *bufio.Reader) error {
	status, hasStatus := stream.(interface {
		Done() <-chan struct{}
		TerminalError() error
	})
	var done <-chan struct{}
	if hasStatus {
		done = status.Done()
	}

	boundedWindow := func(max time.Duration) time.Duration {
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0
			}
			if remaining < max {
				return remaining
			}
		}
		return max
	}
	idleWindow := boundedWindow(terminalSettleWindow)
	maxWindow := boundedWindow(maxLogSettleWindow)
	if idleWindow <= 0 {
		return ctx.Err()
	}
	if maxWindow < idleWindow {
		maxWindow = idleWindow
	}
	idleTimer := time.NewTimer(idleWindow)
	maxTimer := time.NewTimer(maxWindow)
	defer idleTimer.Stop()
	defer maxTimer.Stop()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleWindow)
	}

	readResults := make(chan logSettleRead, 1)
	buf := make([]byte, 32*1024)
	startRead := func() {
		go func() {
			n, err := reader.Read(buf)
			readResults <- logSettleRead{n: n, err: err}
		}()
	}
	startRead()

	settleTerminal := func() error {
		var drainErr error
		if drainer, ok := stream.(interface{ Drain(context.Context) error }); ok {
			drainErr = drainer.Drain(ctx)
		}
		return joinNonNil(status.TerminalError(), drainErr)
	}

	handleRead := func(read logSettleRead) (bool, error) {
		if read.err != nil {
			if isTerminalStreamError(read.err) && hasStatus {
				return true, settleTerminal()
			}
			if isPermanentCheckError(read.err) || isTerminalStreamError(read.err) {
				return true, read.err
			}
			if errors.Is(read.err, io.EOF) {
				if hasStatus {
					select {
					case <-done:
						return true, settleTerminal()
					default:
					}
				}
				return true, nil
			}
			return true, read.err
		}
		if read.n == 0 {
			startRead()
			return false, nil
		}
		resetIdle()
		startRead()
		return false, nil
	}

	idleExpired := false
	maxExpired := false
	for {
		// Before accepting either timer, consume evidence that is already
		// queued. This closes the select race where a ready terminal read
		// could lose to an idle/max timer and incorrectly report success.
		if read, hasRead, doneReady := nextReadyLogSettle(readResults, done); hasRead {
			terminal, err := handleRead(read)
			if terminal {
				return err
			}
			// Consume at most one queued read before entering the select.
			// Falling through is important: a continuously readable stream
			// can otherwise keep this priority path busy forever and starve
			// the idle/max timers and caller context.
		} else if doneReady {
			return settleTerminal()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A queued read or a settled process can become ready in the
			// same turn as caller cancellation. Drain the evidence already
			// queued before returning the context cause, so a terminal CLI
			// error is not hidden by a scheduling race.
			for attempts := 0; attempts < 4; attempts++ {
				select {
				case <-done:
					return joinNonNil(settleTerminal(), ctxErr)
				default:
				}
				read, hasRead, doneReady := nextReadyLogSettle(readResults, done)
				if doneReady {
					return joinNonNil(settleTerminal(), ctxErr)
				}
				if !hasRead {
					break
				}
				terminal, err := handleRead(read)
				if terminal {
					return joinNonNil(err, ctxErr)
				}
			}
			return ctxErr
		}
		if idleExpired || maxExpired {
			return nil
		}

		select {
		case read := <-readResults:
			terminal, err := handleRead(read)
			if terminal {
				return err
			}
		case <-done:
			return settleTerminal()
		case <-idleTimer.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			idleExpired = true
		case <-maxTimer.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			maxExpired = true
		case <-ctx.Done():
			// Stream completion and caller cancellation can become ready in
			// the same scheduling turn. Recheck settled evidence once so a
			// terminal exit remains visible beside the context cause.
			if read, hasRead, doneReady := nextReadyLogSettle(readResults, done); hasRead {
				terminal, err := handleRead(read)
				if terminal {
					return joinNonNil(err, ctx.Err())
				}
			} else if doneReady {
				return joinNonNil(settleTerminal(), ctx.Err())
			}
			return ctx.Err()
		}
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
			return &ConfigError{
				Strategy: "ForLog",
				Field:    "pattern",
				Value:    s.pattern,
				Reason:   err.Error(),
			}
		}
		match = func(line string) int { return len(re.FindAllString(line, -1)) }
	} else {
		match = func(line string) int { return strings.Count(line, s.pattern) }
	}

	var replay logReplay
	// Keep one latest cause for each independent source. A newer cause
	// replaces the retained one instead of being appended, so a noisy follow
	// stream cannot grow an unbounded error tree until the caller's deadline
	// and one source never contributes twice to a single error.
	var lastStreamErr, lastProbeErr error
	record := func(streamErr, probeErr error) error {
		if streamErr != nil {
			lastStreamErr = streamErr
		}
		if probeErr != nil {
			lastProbeErr = probeErr
		}
		return joinNonNil(lastStreamErr, lastProbeErr)
	}
	retained := func() error { return record(nil, nil) }
	for {
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, retained()); terminalErr != nil {
			return terminalErr
		}

		stream, err := target.FollowLogs(waitCtx)
		if err != nil {
			if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, record(err, nil)); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(err) || isTerminalStreamError(err) {
				return wrapWaitCause(what, err, lastProbeErr)
			}
		} else {
			result := scanLogStreamTarget(waitCtx, target, stream, match, s.occurrences, replay)
			if terminalWaitState(result.state) {
				_ = stream.Close()
				return waitStoppedStateError(what, result.state, retained())
			}
			terminalStreamErr := result.terminalErr
			resultErr := record(terminalStreamErr, nil)
			if result.found {
				if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, resultErr); terminalErr != nil {
					_ = stream.Close()
					return terminalErr
				}
			}
			_ = stream.Close()
			if result.found {
				if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, resultErr); terminalErr != nil {
					return terminalErr
				}
				if terminalStreamErr != nil {
					return wrapWaitCause(what, terminalStreamErr, lastProbeErr)
				}
				if err := finalLifecycleError(callerCtx, waitCtx, target, what, timeout, interval, resultErr); err != nil {
					return err
				}
				return nil
			}

			if result.committed {
				replay = logReplay{
					previous:      result.lines,
					previousStart: result.lineStart,
					previousLines: result.lineCount,
					count:         result.count,
					partial:       result.partial,
				}
			}
			if result.err != nil {
				if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, record(result.err, nil)); terminalErr != nil {
					return terminalErr
				}
				if isPermanentCheckError(result.err) || isTerminalStreamError(result.err) {
					return wrapWaitCause(what, result.err, lastProbeErr)
				}
			}
		}
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, retained()); terminalErr != nil {
			return terminalErr
		}

		// A clean EOF (and a transient stream read error) may be
		// reconnectable. The probe is bounded by the existing wait context;
		// it never detaches from or extends the caller's budget.
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			return waitContextError(what, err, retained())
		}
		state, probeErr := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			probeErr = joinNonNil(probeErr, probeCtxErr)
		}
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, record(nil, probeErr)); terminalErr != nil {
			return terminalErr
		}
		if probeErr != nil {
			// A CLIError here came from the independent state probe, not
			// from the log stream. Only typed permanent state errors stop
			// the wait; transient daemon/inspect failures remain causes and
			// are retried with the next log connection.
			if isPermanentCheckError(probeErr) {
				return wrapWaitCause(what, probeErr, retained())
			}
		} else if terminalWaitState(state) {
			return waitStoppedStateError(what, state, retained())
		}

		if err := waitForReconnect(waitCtx, interval); err != nil {
			return waitContextTerminationError(callerCtx, waitCtx, what, timeout, retained())
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
