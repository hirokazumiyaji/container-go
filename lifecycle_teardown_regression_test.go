package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWithAttachOutputRejectsTypedNilWriter(t *testing.T) {
	var buf *bytes.Buffer
	err := WithAttachOutput(buf)(newConfig())
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "WithAttachOutput" {
		t.Fatalf("WithAttachOutput(typed nil) error = %v, want a WithAttachOutput *ValidationError", err)
	}
}

func TestFollowLogsToRejectsTypedNilWriter(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())

	var buf *bytes.Buffer
	err := ctr.FollowLogsTo(context.Background(), buf)
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "FollowLogsTo" {
		t.Fatalf("FollowLogsTo(typed nil) error = %v, want a FollowLogsTo *ValidationError", err)
	}
}

// teardownRacingStreamerRunner fails its attach stream while the teardown
// command runs, which is the window a backend needs to end the container.
type teardownRacingStreamerRunner struct {
	*fakeRunner
	command        string
	release        chan struct{}
	started        chan struct{}
	attachDone     func() <-chan struct{}
	commandWaiting chan struct{}
	releaseCommand chan struct{}
	streamData     []byte
	streamErr      error

	blockAfterFirst bool
	secondStarted   chan struct{}
	secondOnce      sync.Once

	mu        sync.Mutex
	streams   int
	secondCtx context.Context
	startOnce sync.Once
}

func (r *teardownRacingStreamerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == r.command {
		close(r.release)
		// Wait for the attach stream to finish, so the failure is recorded
		// before the teardown command returns.
		if r.attachDone != nil {
			<-r.attachDone()
		}
		if r.commandWaiting != nil {
			close(r.commandWaiting)
		}
		if r.releaseCommand != nil {
			<-r.releaseCommand
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

// attachStreamDoneForTest returns the goroutine completion channel, which
// closes even when AttachedDone is held open for a teardown outcome.
func attachStreamDoneForTest(t *testing.T, c *Container) <-chan struct{} {
	t.Helper()
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	return p.attachStreamDone
}

func (r *teardownRacingStreamerRunner) Stream(ctx context.Context, _ ...string) (io.ReadCloser, error) {
	r.startOnce.Do(func() { close(r.started) })
	r.mu.Lock()
	r.streams++
	first := r.streams == 1
	if !first && r.blockAfterFirst {
		r.secondCtx = ctx
		r.secondOnce.Do(func() {
			if r.secondStarted != nil {
				close(r.secondStarted)
			}
		})
	}
	r.mu.Unlock()
	if first {
		return io.NopCloser(&releasedReader{release: r.release, data: r.streamData, err: r.streamErr}), nil
	}
	if r.blockAfterFirst {
		return io.NopCloser(contextReader{ctx: ctx}), nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (r *teardownRacingStreamerRunner) secondStreamContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secondCtx
}

type contextReader struct {
	ctx context.Context
}

func (r contextReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

type releasedReader struct {
	release <-chan struct{}
	data    []byte
	err     error
	sent    bool
}

func (r *releasedReader) Read(p []byte) (int, error) {
	<-r.release
	if !r.sent && len(r.data) > 0 {
		r.sent = true
		return copy(p, r.data), nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func TestTeardownDoesNotReportStreamError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		run     func(*Container) error
	}{
		{
			name:    "stop",
			command: "stop",
			run: func(c *Container) error {
				return c.Stop(context.Background(), nil)
			},
		},
		{
			name:    "terminating kill",
			command: "kill",
			run: func(c *Container) error {
				return c.Kill(context.Background(), "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &teardownRacingStreamerRunner{
				fakeRunner: newTestRunner(),
				command:    tc.command,
				release:    make(chan struct{}),
				started:    make(chan struct{}),
				streamErr:  errors.New("follow stream reset"),
			}
			ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
			select {
			case <-f.started:
			case <-time.After(2 * time.Second):
				t.Fatal("attach stream never started")
			}
			f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }

			if err := tc.run(ctr); err != nil {
				t.Fatalf("%s: %v", tc.command, err)
			}
			if err := ctr.AttachedError(); err != nil {
				t.Fatalf("AttachedError() = %v, want nil: the failure belongs to the teardown", err)
			}
		})
	}
}

func TestTeardownPublishesStreamErrorWhenCommandFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		run     func(*Container) error
	}{
		{
			name:    "stop",
			command: "stop",
			run: func(c *Container) error {
				return c.Stop(context.Background(), nil)
			},
		},
		{
			name:    "terminating kill",
			command: "kill",
			run: func(c *Container) error {
				return c.Kill(context.Background(), "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamErr := errors.New("follow stream reset")
			f := &teardownRacingStreamerRunner{
				fakeRunner: newTestRunner(),
				command:    tc.command,
				release:    make(chan struct{}),
				started:    make(chan struct{}),
				streamErr:  streamErr,
			}
			ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
			select {
			case <-f.started:
			case <-time.After(2 * time.Second):
				t.Fatal("attach stream never started")
			}
			f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }
			f.failPrefix = tc.command

			if err := tc.run(ctr); err == nil {
				t.Fatal("teardown command returned nil, want the injected failure")
			}
			if err := ctr.AttachedError(); !errors.Is(err, streamErr) {
				t.Fatalf("AttachedError() = %v, want the stream failure observed while the command failed", err)
			}
		})
	}
}

func TestTeardownHoldsAttachedDoneUntilCommandOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		run     func(*Container) error
	}{
		{
			name:    "stop",
			command: "stop",
			run: func(c *Container) error {
				return c.Stop(context.Background(), nil)
			},
		},
		{
			name:    "terminating kill",
			command: "kill",
			run: func(c *Container) error {
				return c.Kill(context.Background(), "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamErr := errors.New("follow stream reset")
			f := &teardownRacingStreamerRunner{
				fakeRunner:     newTestRunner(),
				command:        tc.command,
				release:        make(chan struct{}),
				started:        make(chan struct{}),
				commandWaiting: make(chan struct{}),
				releaseCommand: make(chan struct{}),
				streamErr:      streamErr,
			}
			ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
			select {
			case <-f.started:
			case <-time.After(2 * time.Second):
				t.Fatal("attach stream never started")
			}
			f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }
			f.failPrefix = tc.command

			commandDone := make(chan error, 1)
			go func() { commandDone <- tc.run(ctr) }()
			select {
			case <-f.commandWaiting:
			case <-time.After(2 * time.Second):
				t.Fatal("command did not reach the attach failure")
			}
			select {
			case <-ctr.AttachedDone():
				t.Fatal("AttachedDone closed before the teardown outcome was known")
			default:
			}

			close(f.releaseCommand)
			if err := <-commandDone; err == nil {
				t.Fatal("teardown command returned nil, want the injected failure")
			}
			select {
			case <-ctr.AttachedDone():
			case <-time.After(2 * time.Second):
				t.Fatal("AttachedDone did not close after the teardown outcome")
			}
			if err := ctr.AttachedError(); !errors.Is(err, streamErr) {
				t.Fatalf("AttachedError() = %v, want the stream failure", err)
			}
		})
	}
}

func TestStopAttachedDoesNotDiscardPendingTeardownFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		run     func(*Container) error
	}{
		{
			name:    "stop",
			command: "stop",
			run: func(c *Container) error {
				return c.Stop(context.Background(), nil)
			},
		},
		{
			name:    "terminating kill",
			command: "kill",
			run: func(c *Container) error {
				return c.Kill(context.Background(), "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamErr := errors.New("follow stream reset")
			f := &teardownRacingStreamerRunner{
				fakeRunner:     newTestRunner(),
				command:        tc.command,
				release:        make(chan struct{}),
				started:        make(chan struct{}),
				commandWaiting: make(chan struct{}),
				releaseCommand: make(chan struct{}),
				streamErr:      streamErr,
			}
			ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
			select {
			case <-f.started:
			case <-time.After(2 * time.Second):
				t.Fatal("attach stream never started")
			}
			f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }
			f.failPrefix = tc.command

			commandDone := make(chan error, 1)
			go func() { commandDone <- tc.run(ctr) }()
			select {
			case <-f.commandWaiting:
			case <-time.After(2 * time.Second):
				t.Fatal("command did not reach the attach failure")
			}

			ctr.StopAttached()
			if err := ctr.AttachedError(); err != nil {
				t.Fatalf("AttachedError() = %v before the command failed, want the failure still pending", err)
			}
			select {
			case <-ctr.AttachedDone():
				t.Fatal("StopAttached closed AttachedDone while the command still owned the outcome")
			default:
			}

			close(f.releaseCommand)
			if err := <-commandDone; err == nil {
				t.Fatal("teardown command returned nil, want the injected failure")
			}
			select {
			case <-ctr.AttachedDone():
			case <-time.After(2 * time.Second):
				t.Fatal("AttachedDone did not close after the command failed")
			}
			if err := ctr.AttachedError(); !errors.Is(err, streamErr) {
				t.Fatalf("AttachedError() = %v, want the preserved stream failure", err)
			}
		})
	}
}

func TestStartReattachesAfterAttachedDoneCloses(t *testing.T) {
	f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"}
	ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))

	const reattachments = 50
	for i := 0; i < reattachments; i++ {
		select {
		case <-ctr.AttachedDone():
		case <-time.After(2 * time.Second):
			t.Fatalf("attached stream %d did not finish", i)
		}
		if err := ctr.Start(context.Background()); err != nil {
			t.Fatalf("Start after stream %d: %v", i, err)
		}
	}
	f.waitForStreams(t, reattachments+1)
}

func TestStartDuringTeardownPreservesPendingFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		run     func(*Container) error
	}{
		{
			name:    "stop",
			command: "stop",
			run: func(c *Container) error {
				return c.Stop(context.Background(), nil)
			},
		},
		{
			name:    "terminating kill",
			command: "kill",
			run: func(c *Container) error {
				return c.Kill(context.Background(), "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamErr := errors.New("follow stream reset")
			f := &teardownRacingStreamerRunner{
				fakeRunner:     newTestRunner(),
				command:        tc.command,
				release:        make(chan struct{}),
				started:        make(chan struct{}),
				commandWaiting: make(chan struct{}),
				releaseCommand: make(chan struct{}),
				streamErr:      streamErr,
			}
			ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
			oldDone := ctr.AttachedDone()
			select {
			case <-f.started:
			case <-time.After(2 * time.Second):
				t.Fatal("attach stream never started")
			}
			f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }
			f.failPrefix = tc.command

			commandDone := make(chan error, 1)
			go func() { commandDone <- tc.run(ctr) }()
			select {
			case <-f.commandWaiting:
			case <-time.After(2 * time.Second):
				t.Fatal("command did not reach the attach failure")
			}

			// Start replaces the stream while the command that owns the
			// deferred failure is still running.
			if err := ctr.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			select {
			case <-oldDone:
				t.Fatal("Start closed the AttachedDone owned by the teardown command")
			default:
			}
			if err := ctr.AttachedError(); err != nil {
				t.Fatalf("AttachedError() = %v before the command failed, want the failure still pending", err)
			}

			close(f.releaseCommand)
			if err := <-commandDone; err == nil {
				t.Fatal("teardown command returned nil, want the injected failure")
			}
			select {
			case <-oldDone:
			case <-time.After(2 * time.Second):
				t.Fatal("the previous stream's AttachedDone did not close after the command failed")
			}
			if err := ctr.AttachedError(); !errors.Is(err, streamErr) {
				t.Fatalf("AttachedError() = %v, want the preserved stream failure", err)
			}
		})
	}
}

func TestStopDoesNotCancelStreamReattachedDuringTeardown(t *testing.T) {
	streamErr := errors.New("follow stream reset")
	f := &teardownRacingStreamerRunner{
		fakeRunner:      newTestRunner(),
		command:         "stop",
		release:         make(chan struct{}),
		started:         make(chan struct{}),
		commandWaiting:  make(chan struct{}),
		releaseCommand:  make(chan struct{}),
		streamErr:       streamErr,
		blockAfterFirst: true,
		secondStarted:   make(chan struct{}),
	}
	ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
	oldDone := ctr.AttachedDone()
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("attach stream never started")
	}
	f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }

	commandDone := make(chan error, 1)
	go func() { commandDone <- ctr.Stop(context.Background(), nil) }()
	select {
	case <-f.commandWaiting:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not reach the attach failure")
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-f.secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement attach stream never started")
	}
	replacementCtx := f.secondStreamContext()

	close(f.releaseCommand)
	if err := <-commandDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-oldDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the stopped stream's AttachedDone did not close after Stop succeeded")
	}
	select {
	case <-replacementCtx.Done():
		t.Fatal("Stop cancelled the stream that Start reattached during the command")
	default:
	}
	if err := ctr.AttachedError(); err != nil {
		t.Fatalf("AttachedError() = %v, want the successful teardown's failure discarded", err)
	}

	ctr.StopAttached()
	select {
	case <-replacementCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("StopAttached did not cancel the replacement stream")
	}
}

func TestStartReattachesAfterStopAbandonsWedgedStream(t *testing.T) {
	f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"}
	sink := &blockingSink{entered: make(chan struct{}), released: make(chan struct{})}
	ctr := runTestContainer(t, f, WithAttachOutput(sink))
	t.Cleanup(func() { close(sink.released) })
	oldDone := ctr.AttachedDone()
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("attach stream never wrote to the sink")
	}

	// Stop cancels the stream, but the sink stays wedged past attachStopWait.
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-oldDone:
		t.Fatal("the wedged stream finished despite the blocked sink")
	default:
	}

	// Start must replace the cancelled follower instead of treating its open
	// completion channel as an active stream.
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-ctr.AttachedDone():
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not replace the cancelled wedged stream")
	}
	if err := ctr.AttachedError(); err == nil {
		t.Fatal("AttachedError() = nil, want the abandoned replacement's failure")
	}
}

func TestTeardownCommandsSerializeAttachResolution(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())
	first := ctr.markAttachTeardown()

	acquired := make(chan struct{})
	go func() {
		second := ctr.markAttachTeardown()
		ctr.clearAttachTeardown(second)
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("a second teardown acquired attach resolution before the first resolved")
	case <-time.After(100 * time.Millisecond):
	}

	ctr.clearAttachTeardown(first)
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the second teardown did not proceed after the first resolved")
	}
}

func TestTeardownReportsOutputDeliveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		run     func(*Container) error
	}{
		{
			name:    "stop",
			command: "stop",
			run: func(c *Container) error {
				return c.Stop(context.Background(), nil)
			},
		},
		{
			name:    "terminating kill",
			command: "kill",
			run: func(c *Container) error {
				return c.Kill(context.Background(), "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deliveryErr := errors.New("output sink failed")
			f := &teardownRacingStreamerRunner{
				fakeRunner: newTestRunner(),
				command:    tc.command,
				release:    make(chan struct{}),
				started:    make(chan struct{}),
				streamData: []byte("shutdown log line\n"),
			}
			ctr := runTestContainer(t, f, WithAttachOutput(&failingOutputWriter{err: deliveryErr}))
			select {
			case <-f.started:
			case <-time.After(2 * time.Second):
				t.Fatal("attach stream never started")
			}
			f.attachDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }

			if err := tc.run(ctr); err != nil {
				t.Fatalf("%s: %v", tc.command, err)
			}
			if err := ctr.AttachedError(); !errors.Is(err, deliveryErr) || !errors.Is(err, ErrOutputDelivery) {
				t.Fatalf("AttachedError() = %v, want the output-delivery failure", err)
			}
		})
	}
}
