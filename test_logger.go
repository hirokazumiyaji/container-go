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
// The stream is independent of Cleanup and of the order in which the test
// registers teardown, so a container removed by an earlier cleanup simply ends
// the stream.
func WithTestLogger(t testing.TB) Option {
	return func(c *config) error {
		if nilTestLogger(t) {
			return validationErrorf("WithTestLogger", nil, "testing.TB must not be nil")
		}
		c.testLogger = t
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
	var (
		stream io.ReadCloser
		err    error
	)
	if c.reused && !c.created {
		// A reused generation can be long-lived across many tests. Resume
		// slightly before attach time so the test does not replay the whole
		// log history but a small host/daemon clock skew cannot drop lines
		// produced just after attach. Apple Container has no --since
		// equivalent, so its backend still follows from the available history.
		stream, err = c.followLogs(ctx, time.Now().Add(-time.Second))
	} else {
		stream, err = c.FollowLogs(ctx)
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

// forwardTestLogs writes complete logical lines to the test logger. A terminal
// stream error is not reported: the common case is the container being removed
// by the test's own teardown, and a setup failure is reported by startTestLogger.
func forwardTestLogs(tb testing.TB, c *Container, stream io.Reader) {
	reader := bufio.NewReader(stream)
	for {
		line, truncated, err := readTestLogLine(reader)
		if line != "" || truncated {
			line = sanitizeTestLogLine(line)
			if truncated {
				line += " [truncated]"
			}
			tb.Logf("%s: %s", c.ID(), line)
		}
		if err != nil {
			return
		}
	}
}

// sanitizeTestLogLine removes the CR of a CRLF line ending and escapes any
// remaining control or format character, so container output cannot inject
// terminal sequences or rewrite earlier lines in the test log.
func sanitizeTestLogLine(line string) string {
	return diagnostic.Sanitize(strings.TrimSuffix(line, "\r"))
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
