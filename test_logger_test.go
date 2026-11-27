package container

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingTestLogStream struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockingTestLogStream() *blockingTestLogStream {
	return &blockingTestLogStream{closed: make(chan struct{})}
}

func (s *blockingTestLogStream) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.EOF
}

func (s *blockingTestLogStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// recordingTestLogTB embeds a real testing.TB so it satisfies the interface,
// and captures Logf output instead of writing it to the test log.
type recordingTestLogTB struct {
	testing.TB
	mu    sync.Mutex
	lines []string
}

func (r *recordingTestLogTB) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func TestForwardTestLogsSanitizesControlCharacters(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	recorder := &recordingTestLogTB{TB: t}
	forwardTestLogs(recorder, ctr, strings.NewReader("ready\x1b[31m\r\nnext\rline\n"))
	joined := strings.Join(recorder.lines, "\n")
	if strings.Contains(joined, "\x1b") || strings.Contains(joined, "\r") {
		t.Fatalf("forwarded logs = %q, contain terminal controls", joined)
	}
	for _, want := range []string{"ready", "next", `\x1b`, `\r`} {
		if !strings.Contains(joined, want) {
			t.Errorf("forwarded logs = %q, missing %q", joined, want)
		}
	}
}

func TestWithTestLoggerClosesStreamWhenTestEnds(t *testing.T) {
	stream := newBlockingTestLogStream()
	f := &singleStreamRunner{fakeRunner: newTestRunner(), stream: stream}
	t.Run("inner", func(t *testing.T) {
		ctr := runTestContainer(t, f, WithTestLogger(t))
		if ctr == nil {
			t.Fatal("Run returned no container")
		}
	})
	select {
	case <-stream.closed:
	default:
		t.Fatal("test logger did not close its log stream at test end")
	}
}

func TestWithTestLoggerFollowsContainerLogs(t *testing.T) {
	f := &streamRunner{fakeRunner: newTestRunner(), streamData: "ready\n"}
	t.Run("inner", func(t *testing.T) {
		runTestContainer(t, f, WithTestLogger(t))
	})
	if !slices.Equal(f.streamArgs, []string{"logs", "--follow", "myctr"}) {
		t.Errorf("stream args = %v, want logs --follow myctr", f.streamArgs)
	}
}

func TestWithTestLoggerWithoutStreamingRunnerDoesNotFailRun(t *testing.T) {
	f := newTestRunner()
	t.Run("inner", func(t *testing.T) {
		ctr := runTestContainer(t, f, WithTestLogger(t))
		if ctr == nil {
			t.Fatal("Run returned no container")
		}
	})
}

func TestWithTestLoggerRejectsNil(t *testing.T) {
	runner := newCountingRunner(newTestRunner())
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(runner), withEngine(appleEngine{}),
		WithTestLogger(nil))
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
	if !strings.Contains(err.Error(), "testing.TB must not be nil") {
		t.Fatalf("error = %v, want nil testing.TB validation", err)
	}
	if got := runner.count(); got != 0 {
		t.Fatalf("backend CLI calls = %d, want 0", got)
	}
}

func TestReadTestLogLine(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("first\nsecond"))
	line, truncated, err := readTestLogLine(reader)
	if line != "first" || truncated || err != nil {
		t.Fatalf("first line = (%q, %v, %v), want (first, false, nil)", line, truncated, err)
	}
	line, truncated, err = readTestLogLine(reader)
	if line != "second" || truncated || !errors.Is(err, io.EOF) {
		t.Fatalf("partial final line = (%q, %v, %v), want (second, false, EOF)", line, truncated, err)
	}
}

func TestReadTestLogLineBoundsLongLines(t *testing.T) {
	long := strings.Repeat("x", maxTestLogLine+32) + "\nnext\n"
	reader := bufio.NewReader(strings.NewReader(long))
	line, truncated, err := readTestLogLine(reader)
	if err != nil {
		t.Fatalf("long line error = %v", err)
	}
	if !truncated || len(line) != maxTestLogLine {
		t.Fatalf("long line = (%d bytes, truncated=%v), want (%d, true)", len(line), truncated, maxTestLogLine)
	}
	line, truncated, err = readTestLogLine(reader)
	if line != "next" || truncated || err != nil {
		t.Fatalf("line after long line = (%q, %v, %v), want (next, false, nil)", line, truncated, err)
	}
}

func TestTestLoggerResumesReusedAttachAtAttachTime(t *testing.T) {
	f := &streamRunner{fakeRunner: newTestRunner(), streamData: "line\n"}
	c := &Container{
		id:      "myctr",
		runner:  f,
		eng:     dockerEngine{},
		sync:    &lockPack{uid: dockerFixtureID},
		reused:  true,
		created: false,
	}
	t.Run("inner", func(t *testing.T) {
		startTestLogger(&config{testLogger: t}, c)
	})
	index := slices.Index(f.streamArgs, "--since")
	if index < 0 || index+1 >= len(f.streamArgs) {
		t.Fatalf("reuse attach stream args = %v, want --since resume", f.streamArgs)
	}
	since, err := time.Parse(time.RFC3339Nano, f.streamArgs[index+1])
	if err != nil {
		t.Fatalf("parse --since %q: %v", f.streamArgs[index+1], err)
	}
	if !since.Before(time.Now()) {
		t.Errorf("--since = %v, want a time in the past", since)
	}
	if time.Since(since) < 500*time.Millisecond {
		t.Errorf("--since = %v, want a small clock-skew margin before attach time", since)
	}
}

func TestTestLoggerKeepsHistoryForCreatedHandle(t *testing.T) {
	f := &streamRunner{fakeRunner: newTestRunner(), streamData: "line\n"}
	c := &Container{
		id:      "myctr",
		runner:  f,
		eng:     dockerEngine{},
		sync:    &lockPack{uid: dockerFixtureID},
		reused:  true,
		created: true,
	}
	t.Run("inner", func(t *testing.T) {
		startTestLogger(&config{testLogger: t}, c)
	})
	if slices.Contains(f.streamArgs, "--since") {
		t.Errorf("created handle stream args = %v, want history replay", f.streamArgs)
	}
}
