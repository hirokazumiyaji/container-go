package container

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

// maxTestLogLine bounds one forwarded log line so a workload that never emits
// a newline cannot make a test retain an unbounded string. Longer lines are
// forwarded as a truncated prefix.
const maxTestLogLine = 16 << 10

// testLogOnFailureLimit is the byte ring that WithTestLogOnFailure retains
// during the test. Only the most recent output is flushed, so a noisy
// container cannot grow the test's memory without bound.
const testLogOnFailureLimit = 64 << 10

// testLogSink is the subset of testing.TB used by the container test loggers.
// Keeping it small makes the forwarding, buffering, and failure-time flush
// testable without a real testing.T.
type testLogSink interface {
	Logf(format string, args ...any)
	Cleanup(func())
	Failed() bool
}

// WithTestLogger streams the container's log output to t while the test runs.
// For a newly created container, FollowLogs replays the available log history
// first, so output produced before this option takes effect (including startup
// output) is still forwarded. A WithReuse attach resumes slightly before attach
// time instead, so a long-lived shared container does not replay its entire
// history into every test and a small clock skew cannot drop new lines; Apple
// Container has no --since equivalent and still follows from the available
// history. The stream is closed when the test finishes. A
// log stream that cannot be opened is reported through t.Logf and does not
// fail Run.
//
// Each forwarded line is sanitized: CRLF and bare CR are treated as line
// breaks, and terminal control or format runes are escaped, so container
// output cannot move the cursor or corrupt the test log.
//
// The stream is independent of Cleanup and of the order in which the test
// registers teardown, so a container removed by an earlier cleanup simply ends
// the stream. WithTestLogOnFailure is the bounded alternative when a passing
// test should not print container logs.
func WithTestLogger(t testing.TB) Option {
	return func(c *config) error {
		if nilTestLogger(t) {
			return validationErrorf("WithTestLogger", nil, "testing.TB must not be nil")
		}
		c.testLogger = t
		c.testLogOnFailure = false
		return nil
	}
}

// WithTestLogOnFailure records the container's log output in a 64 KiB ring
// buffer while the test runs and emits it through t.Logf at t.Cleanup only
// when t.Failed reports a failure. Logs are sanitized as with WithTestLogger.
// The same stream selection and teardown behavior applies: new containers
// replay their history, Docker reuse attach resumes just before attach time,
// and a workload cannot retain more than the ring plus one bounded line.
//
// A log stream that cannot be opened is recorded in the ring as a diagnostic
// and emitted only if the test failed. WithTestLogOnFailure is intended for
// integration tests whose container output is only interesting on failure.
func WithTestLogOnFailure(t testing.TB) Option {
	return func(c *config) error {
		if t == nil {
			return validationErrorf("WithTestLogOnFailure", nil, "testing.TB must not be nil")
		}
		c.testLogger = t
		c.testLogOnFailure = true
		return nil
	}
}

// nilTestLogger reports whether t is nil, including a typed-nil *testing.T or
// *testing.B stored in the interface. A plain comparison misses that case and
// the first Logf or Cleanup call would panic.
func nilTestLogger(t testing.TB) bool {
	if t == nil {
		return true
	}
	value := reflect.ValueOf(t)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// startTestLogger begins forwarding the container's logs to the configured
// test. It must run on the caller's goroutine so t.Cleanup is registered in the
// test's cleanup list; the forwarding itself runs in a background goroutine.
func startTestLogger(cfg *config, c *Container) {
	tb := cfg.testLogger
	if tb == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	// A reused generation can be long-lived across many tests. Resume
	// slightly before attach time so the test does not replay the whole
	// log history but a small host/daemon clock skew cannot drop lines
	// produced just after attach. Apple Container has no --since
	// equivalent, so its backend still follows from the available history.
	var (
		stream io.ReadCloser
		err    error
	)
	if c.reused && !c.created {
		stream, err = c.followLogs(ctx, FollowLogsOptions{}, time.Now().Add(-time.Second))
	} else {
		stream, err = c.FollowLogs(ctx)
	}

	if cfg.testLogOnFailure {
		startTestLogOnFailure(tb, c, cancel, stream, err)
		return
	}
	if err != nil {
		cancel()
		tb.Logf("container-go: stream logs for %s: %v", c.ID(), err)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardTestLogs(tb, c, stream)
	}()
	// The cleanup goroutine owns Close so a blocking stream is not closed
	// concurrently by the forwarding goroutine.
	tb.Cleanup(func() {
		cancel()
		_ = stream.Close()
		<-done
	})
}

// startTestLogOnFailure retains the stream in a ring buffer and defers the
// t.Logf calls to Cleanup. The open error, if any, is recorded in the same
// buffer so a failing test can see why no logs were captured.
func startTestLogOnFailure(tb testLogSink, c *Container, cancel context.CancelFunc, stream io.ReadCloser, openErr error) {
	buffer := newTestLogBuffer()
	if openErr != nil {
		cancel()
		for _, line := range normalizeTestLogLine("container-go: stream logs for " + c.ID() + ": " + openErr.Error()) {
			buffer.add(c.ID() + ": " + line)
		}
		tb.Cleanup(func() { flushTestLogBuffer(tb, buffer) })
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		collectTestLogs(buffer, c, stream)
	}()
	tb.Cleanup(func() {
		cancel()
		_ = stream.Close()
		<-done
		flushTestLogBuffer(tb, buffer)
	})
}

// flushTestLogBuffer emits accumulated lines only for a failed test.
func flushTestLogBuffer(tb testLogSink, buffer *testLogBuffer) {
	if !tb.Failed() {
		return
	}
	for _, line := range strings.Split(buffer.String(), "\n") {
		if line == "" {
			continue
		}
		tb.Logf("%s", line)
	}
}

// collectTestLogs records sanitized lines in the bounded buffer. A terminal
// stream error is not recorded: the common case is the container being
// removed by the test's own teardown, and a setup failure is recorded by
// startTestLogOnFailure.
func collectTestLogs(buffer *testLogBuffer, c *Container, stream io.Reader) {
	reader := bufio.NewReader(stream)
	for {
		line, truncated, err := readTestLogLine(reader)
		if line != "" || truncated {
			if truncated {
				line += " [truncated]"
			}
			for _, part := range normalizeTestLogLine(line) {
				if part == "" {
					continue
				}
				buffer.add(c.ID() + ": " + part)
			}
		}
		if err != nil {
			return
		}
	}
}

// forwardTestLogs writes complete logical lines to the test logger. A terminal
// stream error is not reported: the common case is the container being removed
// by the test's own teardown, and a setup failure is reported by startTestLogger.
func forwardTestLogs(tb testLogSink, c *Container, stream io.Reader) {
	reader := bufio.NewReader(stream)
	for {
		line, truncated, err := readTestLogLine(reader)
		if line != "" || truncated {
			if truncated {
				line += " [truncated]"
			}
			for _, part := range normalizeTestLogLine(line) {
				if part == "" {
					continue
				}
				tb.Logf("%s: %s", c.ID(), part)
			}
		}
		if err != nil {
			return
		}
	}
}

// normalizeTestLogLine turns one stream line into the logical lines a test
// logger should emit. CRLF and bare CR become line breaks, and every
// remaining control or format rune is escaped so terminal escape sequences
// and cursor movement cannot corrupt the test output.
func normalizeTestLogLine(line string) []string {
	line = strings.ReplaceAll(line, "\r\n", "\n")
	line = strings.ReplaceAll(line, "\r", "\n")
	parts := strings.Split(line, "\n")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	for i, part := range parts {
		parts[i] = diagnostic.Sanitize(part)
	}
	return parts
}

// testLogBuffer is a fixed-size byte ring of sanitized log lines. The ring
// keeps the most recent output; a leading partial line produced by ring
// truncation is dropped at flush time so only complete records are emitted.
type testLogBuffer struct {
	tail    *diagnostic.TailBuffer
	written int
}

func newTestLogBuffer() *testLogBuffer {
	return &testLogBuffer{tail: diagnostic.NewTailBuffer(testLogOnFailureLimit)}
}

func (b *testLogBuffer) add(line string) {
	b.written += len(line) + 1
	_, _ = io.WriteString(b.tail, line+"\n")
}

func (b *testLogBuffer) String() string {
	data := b.tail.String()
	if b.written <= testLogOnFailureLimit {
		return data
	}
	// The retained window starts mid-line. Drop the incomplete prefix so a
	// structural escape cannot be emitted half-written.
	if i := strings.IndexByte(data, '\n'); i >= 0 {
		return data[i+1:]
	}
	return ""

}

// readTestLogLine reads one newline-terminated line, bounding the retained
// prefix. The returned error is the first non-ErrBufferFull read error; any
// bytes read alongside it are still returned.
func readTestLogLine(reader *bufio.Reader) (string, bool, error) {
	var line []byte
	truncated := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 && !truncated {
			room := maxTestLogLine - len(line)
			if len(chunk) > room {
				line = append(line, chunk[:room]...)
				truncated = true
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		return string(line), truncated, err
	}
}
