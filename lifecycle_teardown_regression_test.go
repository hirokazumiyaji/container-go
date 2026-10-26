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

func attachCancelForTest(t *testing.T, c *Container) context.CancelFunc {
	t.Helper()
	p := c.pack()
	p.attachMu.Lock()
	defer p.attachMu.Unlock()
	return p.attachCancel
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

// signallingRunner reports when a lifecycle command's backend call returned,
// which is when its attach teardown starts draining the stream.
type signallingRunner struct {
	*recordingStreamerRunner
	command string
	done    chan struct{}
	once    sync.Once
}

func (r *signallingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	out, errOut, err := r.recordingStreamerRunner.Run(ctx, args...)
	if len(args) > 0 && args[0] == r.command {
		r.once.Do(func() { close(r.done) })
	}
	return out, errOut, err
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

func TestStartReattachesWhileStopDrainsWedgedStream(t *testing.T) {
	f := &signallingRunner{
		recordingStreamerRunner: &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"},
		command:                 "stop",
		done:                    make(chan struct{}),
	}
	sink := &blockingSink{entered: make(chan struct{}), released: make(chan struct{})}
	ctr := runTestContainer(t, f, WithAttachOutput(sink))
	t.Cleanup(func() { close(sink.released) })
	oldDone := ctr.AttachedDone()
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("attach stream never wrote to the sink")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- ctr.Stop(context.Background(), nil) }()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop command did not return")
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for attachCancelForTest(t, ctr) != nil {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-ctr.AttachedDone():
	case <-time.After(2 * time.Second):
		t.Fatal("Start treated the stream Stop was draining as active")
	}
	select {
	case <-oldDone:
		t.Fatal("the wedged stream finished despite the blocked sink")
	default:
	}
	if err := <-stopErr; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestHeldAttachedDoneClosesAfterInternalCompletion(t *testing.T) {
	done := make(chan struct{})
	streamDone := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		closeAttachDoneAfterStream(done, streamDone)
		close(returned)
	}()
	select {
	case <-done:
		t.Fatal("held AttachedDone closed before the stream's internal completion")
	case <-time.After(20 * time.Millisecond):
	}

	close(streamDone)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("held AttachedDone did not close after the stream completed")
	}
	<-returned
}

// stopBlockingRunner holds a graceful stop inside the backend so a test can
// check that a forced kill still reaches it.
type stopBlockingRunner struct {
	*fakeRunner
	stopStarted chan struct{}
	releaseStop chan struct{}
	killStarted chan struct{}
}

func (r *stopBlockingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "stop":
		close(r.stopStarted)
		select {
		case <-r.releaseStop:
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	case "kill":
		close(r.killStarted)
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestForceKillRunsDuringGracefulStop(t *testing.T) {
	r := &stopBlockingRunner{
		fakeRunner:  newTestRunner(),
		stopStarted: make(chan struct{}),
		releaseStop: make(chan struct{}),
		killStarted: make(chan struct{}),
	}
	ctr := runTestContainer(t, r)

	stopDone := make(chan error, 1)
	go func() { stopDone <- ctr.Stop(context.Background(), nil) }()
	select {
	case <-r.stopStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("graceful Stop never reached the backend")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := ctr.Kill(ctx, ""); err != nil {
		t.Fatalf("Kill during graceful Stop: %v", err)
	}
	select {
	case <-r.killStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill returned without signalling the backend")
	}

	close(r.releaseStop)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// staleSuppressionRunner serves a first attach stream that fails while a
// graceful Stop is in the backend, then a replacement stream that stays alive
// until the test releases it. Kill always fails at the backend.
type staleSuppressionRunner struct {
	*fakeRunner
	firstStarted  chan struct{}
	firstRelease  chan struct{}
	firstDone     func() <-chan struct{}
	stopEntered   chan struct{}
	releaseStop   chan struct{}
	secondStarted chan struct{}
	secondRelease chan struct{}
	firstErr      error
	secondErr     error

	mu      sync.Mutex
	streams int
}

func (r *staleSuppressionRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "stop":
		close(r.firstRelease)
		<-r.firstDone()
		close(r.stopEntered)
		<-r.releaseStop
		return nil, nil, nil
	case "kill":
		return nil, nil, errors.New("kill backend failed")
	}
	return r.fakeRunner.Run(context.Background(), args...)
}

func (r *staleSuppressionRunner) Stream(context.Context, ...string) (io.ReadCloser, error) {
	r.mu.Lock()
	r.streams++
	index := r.streams
	r.mu.Unlock()
	if index == 1 {
		return &releasedErrorReader{started: r.firstStarted, release: r.firstRelease, err: r.firstErr}, nil
	}
	return &releasedErrorReader{started: r.secondStarted, release: r.secondRelease, err: r.secondErr}, nil
}

type releasedErrorReader struct {
	started chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (r *releasedErrorReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, r.err
}

func (r *releasedErrorReader) Close() error { return nil }

// killBlockingRunner holds a terminating kill in the backend so a
// replacement stream can time out waiting for the output gate.
type killBlockingRunner struct {
	*recordingStreamerRunner
	killStarted chan struct{}
	releaseKill chan struct{}
}

func (r *killBlockingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "kill" {
		close(r.killStarted)
		<-r.releaseKill
	}
	return r.recordingStreamerRunner.Run(ctx, args...)
}

func TestSuccessfulTeardownKeepsReplacementGateFailure(t *testing.T) {
	f := &killBlockingRunner{
		recordingStreamerRunner: &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"},
		killStarted:             make(chan struct{}),
		releaseKill:             make(chan struct{}),
	}
	sink := &blockingSink{entered: make(chan struct{}), released: make(chan struct{})}
	ctr := runTestContainer(t, f, WithAttachOutput(sink))
	t.Cleanup(func() { close(sink.released) })
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("attach stream never wrote to the sink")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- ctr.Stop(context.Background(), nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for attachCancelForTest(t, ctr) != nil {
		if time.Now().After(deadline) {
			t.Fatal("Stop did not mark the wedged stream inactive")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	killDone := make(chan error, 1)
	go func() { killDone <- ctr.Kill(context.Background(), "") }()
	select {
	case <-f.killStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill never reached the backend")
	}

	// The replacement cannot take the gate from the wedged stream. The Kill
	// still owns its teardown, but the successful teardown must not discard
	// the delivery failure.
	select {
	case <-ctr.AttachedDone():
	case <-time.After(3 * time.Second):
		t.Fatal("replacement stream did not time out on the wedged sink")
	}
	if err := ctr.AttachedError(); !errors.Is(err, ErrOutputDelivery) {
		t.Fatalf("AttachedError() = %v, want ErrOutputDelivery for the gate timeout", err)
	}

	close(f.releaseKill)
	if err := <-killDone; err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestFailedKillOfReplacementDoesNotSuppressItsFailure(t *testing.T) {
	firstErr := errors.New("first stream reset")
	secondErr := errors.New("replacement stream reset")
	f := &staleSuppressionRunner{
		fakeRunner:    newTestRunner(),
		firstStarted:  make(chan struct{}),
		firstRelease:  make(chan struct{}),
		stopEntered:   make(chan struct{}),
		releaseStop:   make(chan struct{}),
		secondStarted: make(chan struct{}),
		secondRelease: make(chan struct{}),
		firstErr:      firstErr,
		secondErr:     secondErr,
	}
	ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
	f.firstDone = func() <-chan struct{} { return attachStreamDoneForTest(t, ctr) }
	select {
	case <-f.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("attach stream never started")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- ctr.Stop(context.Background(), nil) }()
	select {
	case <-f.stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never recorded the first stream failure")
	}

	// The first stream is finished, so Start attaches a replacement while
	// Stop still owns the first stream's outcome.
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-f.secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement attach stream never started")
	}

	// The terminating Kill of the replacement fails at the backend, so the
	// replacement keeps running and its failure is genuine.
	if err := ctr.Kill(context.Background(), ""); err == nil {
		t.Fatal("Kill returned nil, want the injected backend failure")
	}

	close(f.secondRelease)
	select {
	case <-ctr.AttachedDone():
	case <-time.After(2 * time.Second):
		t.Fatal("replacement stream did not finish")
	}
	if err := ctr.AttachedError(); !errors.Is(err, secondErr) {
		t.Fatalf("AttachedError() = %v, want the replacement's failure", err)
	}

	close(f.releaseStop)
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestOverlappingTeardownsSharePendingFailure(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())
	streamDone := make(chan struct{})
	p := ctr.pack()
	p.attachMu.Lock()
	p.attachStreamDone = streamDone
	p.attachMu.Unlock()

	first := ctr.markAttachTeardown()
	second := ctr.markAttachTeardown()
	if first.state != second.state {
		t.Fatal("overlapping teardowns of one stream did not share their outcome")
	}

	streamErr := errors.New("follow stream reset")
	done := make(chan struct{})
	if !ctr.recordAttachError(done, streamDone, streamErr) {
		t.Fatal("stream failure was not deferred to the owning teardowns")
	}
	ctr.clearAttachTeardown(first)
	select {
	case <-done:
		t.Fatal("the first failed command published the failure while another still owned it")
	default:
	}
	if err := ctr.AttachedError(); err != nil {
		t.Fatalf("AttachedError() = %v before the last command failed, want the failure still pending", err)
	}

	close(streamDone)
	ctr.clearAttachTeardown(second)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("held AttachedDone did not close after the last command failed")
	}
	if err := ctr.AttachedError(); !errors.Is(err, streamErr) {
		t.Fatalf("AttachedError() = %v, want the shared stream failure", err)
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
