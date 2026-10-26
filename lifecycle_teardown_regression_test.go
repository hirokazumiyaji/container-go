package container

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	command    string
	release    chan struct{}
	started    chan struct{}
	attachDone func() <-chan struct{}
	streamData []byte
	streamErr  error
}

func (r *teardownRacingStreamerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == r.command {
		close(r.release)
		// Wait for the attach stream to finish, so the failure is recorded
		// before the teardown command returns.
		if r.attachDone != nil {
			<-r.attachDone()
		}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *teardownRacingStreamerRunner) Stream(context.Context, ...string) (io.ReadCloser, error) {
	close(r.started)
	return io.NopCloser(&releasedReader{release: r.release, data: r.streamData, err: r.streamErr}), nil
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
			f.attachDone = ctr.AttachedDone

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
			f.attachDone = ctr.AttachedDone
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
			f.attachDone = ctr.AttachedDone

			if err := tc.run(ctr); err != nil {
				t.Fatalf("%s: %v", tc.command, err)
			}
			if err := ctr.AttachedError(); !errors.Is(err, deliveryErr) || !errors.Is(err, ErrOutputDelivery) {
				t.Fatalf("AttachedError() = %v, want the output-delivery failure", err)
			}
		})
	}
}
