package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestInspectSnapshotAndGetters(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	info, err := ctr.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if info.ID() != "myctr" {
		t.Errorf("ID() = %q, want myctr", info.ID())
	}
	if info.Name() != "myctr" {
		t.Errorf("Name() = %q, want myctr", info.Name())
	}
	if info.ContainerID() != "" {
		t.Errorf("ContainerID() = %q, want empty for a name-addressed backend", info.ContainerID())
	}
	if !strings.Contains(info.Image(), "redis:7-alpine") {
		t.Errorf("Image() = %q, want redis:7-alpine", info.Image())
	}
	if info.State() != StateRunning {
		t.Errorf("State() = %q, want %q", info.State(), StateRunning)
	}
	if info.ExitCode() != 0 {
		t.Errorf("ExitCode() = %d, want 0", info.ExitCode())
	}
	if info.IP() != "192.168.64.3" {
		t.Errorf("IP() = %q, want 192.168.64.3", info.IP())
	}
	if !info.CreatedAt().IsZero() {
		t.Logf("CreatedAt() = %v", info.CreatedAt())
	}

	labels := info.Labels()
	if labels["com.github.hirokazumiyaji.container-go"] != "true" {
		t.Errorf("Labels() missing expected label: %v", labels)
	}

	// Mutating returned map must not modify internal inspect state
	labels["mutated"] = "true"
	if info.Labels()["mutated"] != "" {
		t.Error("Labels() returned mutable reference to internal map")
	}

	// Ports getter defensive copy check
	ports := info.Ports()
	if len(ports) != 0 {
		ports[0].HostPort = 9999
		if info.Ports()[0].HostPort == 9999 {
			t.Error("Ports() returned mutable reference")
		}
	}

	// Container convenience methods
	if ctr.Image() != ctr.image.reference {
		t.Errorf("ctr.Image() = %q, want %q", ctr.Image(), ctr.image.reference)
	}
	exitCode, err := ctr.ExitCode(context.Background())
	if err != nil {
		t.Fatalf("ctr.ExitCode(): %v", err)
	}
	if exitCode != 0 {
		t.Errorf("ctr.ExitCode() = %d, want 0", exitCode)
	}
}

type sequenceInspectRunner struct {
	inner     *fakeRunner
	calls     int
	responses []string
}

func (s *sequenceInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		if s.calls < len(s.responses) {
			resp := s.responses[s.calls]
			s.calls++
			return []byte(resp), nil, nil
		}
	}
	return s.inner.Run(ctx, args...)
}

func TestWaitAlreadyStopped(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	stoppedInspect := `[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine"},
      "labels": {"com.github.hirokazumiyaji.container-go": "true", "com.github.hirokazumiyaji.container-go.session": "` + sessionID() + `", "com.github.hirokazumiyaji.container-go.creation": "` + ctr.creation + `"}
    },
    "status": {
      "state": "stopped",
      "exitCode": 42
    }
  }
]`
	seq := &sequenceInspectRunner{
		inner:     f,
		responses: []string{stoppedInspect},
	}
	ctr.runner = seq

	exitCode, err := ctr.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	if exitCode != 42 {
		t.Errorf("exitCode = %d, want 42", exitCode)
	}
}

func TestWaitPollingUntilStopped(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	runningInspect := `[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine"},
      "labels": {"com.github.hirokazumiyaji.container-go": "true", "com.github.hirokazumiyaji.container-go.session": "` + sessionID() + `", "com.github.hirokazumiyaji.container-go.creation": "` + ctr.creation + `"}
    },
    "status": {
      "state": "running"
    }
  }
]`
	stoppedInspect := `[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine"},
      "labels": {"com.github.hirokazumiyaji.container-go": "true", "com.github.hirokazumiyaji.container-go.session": "` + sessionID() + `", "com.github.hirokazumiyaji.container-go.creation": "` + ctr.creation + `"}
    },
    "status": {
      "state": "stopped",
      "exitCode": 0
    }
  }
]`
	seq := &sequenceInspectRunner{
		inner:     f,
		responses: []string{runningInspect, stoppedInspect},
	}
	ctr.runner = seq

	start := time.Now()
	exitCode, err := ctr.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Errorf("expected poll delay of ~100ms, elapsed %v", time.Since(start))
	}
}

func TestWaitContextCancelled(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	runningInspect := `[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine"},
      "labels": {"com.github.hirokazumiyaji.container-go": "true", "com.github.hirokazumiyaji.container-go.session": "` + sessionID() + `", "com.github.hirokazumiyaji.container-go.creation": "` + ctr.creation + `"}
    },
    "status": {
      "state": "running"
    }
  }
]`
	seq := &sequenceInspectRunner{
		inner:     f,
		responses: []string{runningInspect, runningInspect, runningInspect},
	}
	ctr.runner = seq

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := ctr.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait error = %v, want context.DeadlineExceeded", err)
	}
}

func TestStart(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := f.callWith("start")
	if len(start) == 0 || !slices.Contains(start, "myctr") {
		t.Errorf("start call = %v, want to contain myctr", start)
	}

	// Refuses shared container
	shared := ctr.Shared()
	shared.reused = true
	shared.allowDestructive = false
	if err := shared.Start(context.Background()); !errors.Is(err, ErrSharedContainer) {
		t.Errorf("shared Start error = %v, want ErrSharedContainer", err)
	}
}

func TestRestart(t *testing.T) {
	t.Run("Apple fallback to stop and start", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		d := 2 * time.Second
		if err := ctr.Restart(context.Background(), &d); err != nil {
			t.Fatalf("Restart: %v", err)
		}

		stop := f.callWith("stop")
		start := f.callWith("start")
		if len(stop) == 0 || !slices.Contains(stop, "myctr") {
			t.Errorf("stop call = %v", stop)
		}
		if len(start) == 0 || !slices.Contains(start, "myctr") {
			t.Errorf("start call = %v", start)
		}
	})

	t.Run("Docker uses native restart", func(t *testing.T) {
		f := newDockerTestRunner()
		ctr := &Container{
			id:       "myctr",
			runner:   f,
			eng:      dockerEngine{},
			creation: "0123456789abcdef",
			sync:     &lockPack{uid: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
		}

		d := 5 * time.Second
		if err := ctr.Restart(context.Background(), &d); err != nil {
			t.Fatalf("Restart: %v", err)
		}

		restart := f.callWith("restart")
		joined := strings.Join(restart, " ")
		if !strings.Contains(joined, "-t 5") || !slices.Contains(restart, "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef") {
			t.Errorf("restart call = %v", restart)
		}
	})

	t.Run("Negative timeout rejected", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		neg := -time.Second
		err := ctr.Restart(context.Background(), &neg)
		if err == nil {
			t.Fatal("expected error for negative timeout, got nil")
		}
		var valErr *ValidationError
		if !errors.As(err, &valErr) {
			t.Fatalf("expected *ValidationError, got %T: %v", err, err)
		}
	})

	t.Run("Refuses shared container", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		shared := ctr.Shared()
		shared.reused = true
		shared.allowDestructive = false
		if err := shared.Restart(context.Background(), nil); !errors.Is(err, ErrSharedContainer) {
			t.Errorf("shared Restart error = %v, want ErrSharedContainer", err)
		}
	})
}

func TestKill(t *testing.T) {
	t.Run("Default SIGKILL", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		if err := ctr.Kill(context.Background()); err != nil {
			t.Fatalf("Kill: %v", err)
		}

		kill := f.callWith("kill")
		if len(kill) == 0 || !slices.Contains(kill, "myctr") {
			t.Errorf("kill call = %v", kill)
		}
	})

	t.Run("Custom signal", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		if err := ctr.Kill(context.Background(), "SIGTERM"); err != nil {
			t.Fatalf("Kill: %v", err)
		}

		kill := f.callWith("kill")
		joined := strings.Join(kill, " ")
		if !strings.Contains(joined, "--signal SIGTERM") || !slices.Contains(kill, "myctr") {
			t.Errorf("kill call = %v", kill)
		}
	})

	t.Run("Validation errors", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		if err := ctr.Kill(context.Background(), "SIGTERM", "SIGKILL"); err == nil {
			t.Error("expected error for multiple signals, got nil")
		}
		if err := ctr.Kill(context.Background(), ""); err == nil {
			t.Error("expected error for empty signal, got nil")
		}
	})

	t.Run("Refuses shared container", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		shared := ctr.Shared()
		shared.reused = true
		shared.allowDestructive = false
		if err := shared.Kill(context.Background()); !errors.Is(err, ErrSharedContainer) {
			t.Errorf("shared Kill error = %v, want ErrSharedContainer", err)
		}
	})
}

func TestPauseAndUnpause(t *testing.T) {
	t.Run("Docker pause and unpause", func(t *testing.T) {
		f := newDockerTestRunner()
		ctr := &Container{
			id:       "myctr",
			runner:   f,
			eng:      dockerEngine{},
			creation: "0123456789abcdef",
			sync:     &lockPack{uid: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
		}

		if err := ctr.Pause(context.Background()); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		pause := f.callWith("pause")
		if len(pause) == 0 || !slices.Contains(pause, "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef") {
			t.Errorf("pause call = %v", pause)
		}

		if err := ctr.Unpause(context.Background()); err != nil {
			t.Fatalf("Unpause: %v", err)
		}
		unpause := f.callWith("unpause")
		if len(unpause) == 0 || !slices.Contains(unpause, "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef") {
			t.Errorf("unpause call = %v", unpause)
		}
	})

	t.Run("Apple returns ErrUnsupportedCapability", func(t *testing.T) {
		f := newTestRunner()
		ctr := runTestContainer(t, f)

		if err := ctr.Pause(context.Background()); !errors.Is(err, ErrUnsupportedCapability) {
			t.Errorf("Pause error = %v, want ErrUnsupportedCapability", err)
		}
		if err := ctr.Unpause(context.Background()); !errors.Is(err, ErrUnsupportedCapability) {
			t.Errorf("Unpause error = %v, want ErrUnsupportedCapability", err)
		}
	})

	t.Run("Refuses shared container", func(t *testing.T) {
		f := newDockerTestRunner()
		ctr := &Container{
			id:               "myctr",
			runner:           f,
			eng:              dockerEngine{},
			creation:         "0123456789abcdef",
			sync:             &lockPack{uid: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
			reused:           true,
			allowDestructive: false,
		}

		if err := ctr.Pause(context.Background()); !errors.Is(err, ErrSharedContainer) {
			t.Errorf("Pause error = %v, want ErrSharedContainer", err)
		}
		if err := ctr.Unpause(context.Background()); !errors.Is(err, ErrSharedContainer) {
			t.Errorf("Unpause error = %v, want ErrSharedContainer", err)
		}
	})
}

type fakeStreamerRunner struct {
	*fakeRunner
	streamData string
}

func (f *fakeStreamerRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	f.recordCall(args)
	return io.NopCloser(strings.NewReader(f.streamData)), nil
}

func TestFollowLogsTo(t *testing.T) {
	f := &fakeStreamerRunner{
		fakeRunner: newTestRunner(),
		streamData: "first log line\nsecond log line\n",
	}
	ctr := runTestContainer(t, f)

	var buf bytes.Buffer
	if err := ctr.FollowLogsTo(context.Background(), &buf); err != nil {
		t.Fatalf("FollowLogsTo: %v", err)
	}

	if buf.String() != "first log line\nsecond log line\n" {
		t.Errorf("got %q, want expected log lines", buf.String())
	}

	// Nil writer validation
	err := ctr.FollowLogsTo(context.Background(), nil)
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "FollowLogsTo" {
		t.Errorf("nil writer error = %v, want a FollowLogsTo *ValidationError", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// failingStreamerRunner serves a stream that fails with err instead of
// producing output.
type failingStreamerRunner struct {
	inner *fakeRunner
	err   error
}

func (r *failingStreamerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return r.inner.Run(ctx, args...)
}

func (r *failingStreamerRunner) Stream(context.Context, ...string) (io.ReadCloser, error) {
	return io.NopCloser(errorReader{err: r.err}), nil
}

func TestFollowLogsToReportsOutputDeliveryFailure(t *testing.T) {
	deliveryErr := errors.New("writer rejected log output")
	f := &fakeStreamerRunner{fakeRunner: newTestRunner(), streamData: "log line\n"}
	ctr := runTestContainer(t, f)

	err := ctr.FollowLogsTo(context.Background(), &failingOutputWriter{err: deliveryErr})
	if !errors.Is(err, deliveryErr) || !errors.Is(err, ErrOutputDelivery) {
		t.Fatalf("FollowLogsTo error = %v, want the writer and output-delivery errors", err)
	}
}

func TestFollowLogsToDoesNotSwallowStreamCancellation(t *testing.T) {
	f := &failingStreamerRunner{inner: newTestRunner(), err: context.Canceled}
	ctr := runTestContainer(t, f)

	// The caller's context stays live, so a stream that ends with a
	// cancellation failed instead of reaching the end of the output.
	err := ctr.FollowLogsTo(context.Background(), io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FollowLogsTo error = %v, want context.Canceled", err)
	}
}

func TestRunRejectsAttachOutputWithReuse(t *testing.T) {
	f := newTestRunner()
	var buf bytes.Buffer
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithReuse(),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if ctr != nil {
		t.Fatalf("Run returned container %+v for a rejected option combination", ctr)
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Option != "WithAttachOutput" {
		t.Fatalf("Run error = %v, want a WithAttachOutput *ValidationError", err)
	}
	if run := f.callWith("run"); run != nil {
		t.Errorf("rejected combination still reached the backend: %v", run)
	}
}

// flakyInspectRunner fails the first len(errs) container inspects, then
// replays responses before falling back to the inner runner.
type flakyInspectRunner struct {
	inner     *fakeRunner
	errs      []error
	responses []string
	inspects  int
}

func (r *flakyInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		n := r.inspects
		r.inspects++
		if n < len(r.errs) {
			return nil, nil, r.errs[n]
		}
		if i := n - len(r.errs); i < len(r.responses) {
			return []byte(r.responses[i]), nil, nil
		}
	}
	return r.inner.Run(ctx, args...)
}

func stoppedInspectResponse(ctr *Container, exitCode int) string {
	return `[
  {
    "id": "myctr",
    "configuration": {
      "id": "myctr",
      "image": {"reference": "redis:7-alpine"},
      "labels": {"com.github.hirokazumiyaji.container-go": "true", "com.github.hirokazumiyaji.container-go.session": "` + sessionID() + `", "com.github.hirokazumiyaji.container-go.creation": "` + ctr.creation + `"}
    },
    "status": {
      "state": "stopped",
      "exitCode": ` + strconv.Itoa(exitCode) + `
    }
  }
]`
}

func TestWaitRetriesTransientInspectErrors(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	runner := &flakyInspectRunner{
		inner: f,
		errs: []error{&cli.CLIError{
			Binary:   "container",
			Args:     []string{"inspect", "myctr"},
			ExitCode: 1,
			Stderr:   "temporary failure: connection reset by peer",
		}},
		responses: []string{stoppedInspectResponse(ctr, 7)},
	}
	ctr.runner = runner

	start := time.Now()
	exitCode, err := ctr.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exitCode != 7 {
		t.Errorf("exitCode = %d, want 7", exitCode)
	}
	if time.Since(start) < waitExitPollInterval {
		t.Error("Wait returned without retrying the transient inspect error")
	}
}

func TestWaitFailsFastOnTerminalInspectError(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)

	runner := &flakyInspectRunner{
		inner: f,
		errs: []error{&cli.CLIError{
			Binary:   "container",
			Args:     []string{"inspect", "myctr"},
			ExitCode: 1,
			Stderr:   "invalid argument",
		}},
	}
	ctr.runner = runner

	if _, err := ctr.Wait(context.Background()); err == nil {
		t.Fatal("Wait returned nil for a terminal inspect error")
	}
	if runner.inspects != 1 {
		t.Errorf("inspects = %d, want 1: a terminal error must not be retried", runner.inspects)
	}
}

// recordingStreamerRunner serves one immediately-ending stream per call and
// records the context and argv each stream runs under. errs optionally
// overrides individual streams with a terminal read error.
type recordingStreamerRunner struct {
	inner *fakeRunner
	data  string
	errs  []error
	mu    sync.Mutex
	ctxs  []context.Context
	args  [][]string
}

func (r *recordingStreamerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return r.inner.Run(ctx, args...)
}

func (r *recordingStreamerRunner) Stream(ctx context.Context, streamArgs ...string) (io.ReadCloser, error) {
	r.mu.Lock()
	index := len(r.ctxs)
	r.ctxs = append(r.ctxs, ctx)
	r.args = append(r.args, slices.Clone(streamArgs))
	var streamErr error
	if index < len(r.errs) {
		streamErr = r.errs[index]
	}
	r.mu.Unlock()
	if streamErr != nil {
		return io.NopCloser(errorReader{err: streamErr}), nil
	}
	return io.NopCloser(strings.NewReader(r.data)), nil
}

func (r *recordingStreamerRunner) streams() []context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ctxs)
}

func (r *recordingStreamerRunner) streamArgs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.args)
}

func (r *recordingStreamerRunner) waitForStreams(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(r.streams()) < want {
		if time.Now().After(deadline) {
			t.Fatalf("attach streams = %d, want %d", len(r.streams()), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWithAttachOutputStreamCancelsOnCompletion(t *testing.T) {
	f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"}
	var buf bytes.Buffer
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("AttachedDone() = nil after Run, want a channel")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not finish")
	}
	if got := buf.String(); got != "attached line\n" {
		t.Errorf("attached output = %q, want the stream contents", got)
	}
	streams := f.streams()
	if len(streams) != 1 {
		t.Fatalf("attach streams = %d, want 1", len(streams))
	}
	select {
	case <-streams[0].Done():
	case <-time.After(time.Second):
		t.Fatal("attach stream context was not cancelled when the stream ended on its own")
	}
}

func TestRestartFallbackReattachesAttachedOutput(t *testing.T) {
	f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"}
	var buf bytes.Buffer
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := ctr.AttachedDone()
	if first == nil {
		t.Fatal("AttachedDone() = nil after Run, want a channel")
	}
	f.waitForStreams(t, 1)

	if err := ctr.Restart(context.Background(), nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if stop := f.inner.lockedCallWith("stop"); len(stop) == 0 {
		t.Fatal("Restart fallback did not stop the container")
	}
	if start := f.inner.lockedCallWith("start"); len(start) == 0 {
		t.Fatal("Restart fallback did not start the container")
	}

	second := ctr.AttachedDone()
	if second == nil {
		t.Fatal("AttachedDone() = nil after Restart, want the replacement stream channel")
	}
	if second == first {
		t.Fatal("AttachedDone() still reports the stream that Restart ended")
	}
	f.waitForStreams(t, 2)
}

func TestDockerRestartReattachesAttachedOutput(t *testing.T) {
	f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"}
	f.inner.imagePresent = true
	var buf bytes.Buffer
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(dockerEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := ctr.AttachedDone()
	if first == nil {
		t.Fatal("AttachedDone() = nil after Run, want a channel")
	}
	f.waitForStreams(t, 1)
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("initial attached stream did not finish")
	}

	if err := ctr.Restart(context.Background(), nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if restart := f.inner.lockedCallWith("restart"); len(restart) == 0 {
		t.Fatal("Restart did not use the native restart command")
	}
	if stop := f.inner.lockedCallWith("stop"); len(stop) != 0 {
		t.Fatal("native Restart fell back to Stop")
	}
	if start := f.inner.lockedCallWith("start"); len(start) != 0 {
		t.Fatal("native Restart fell back to Start")
	}

	second := ctr.AttachedDone()
	if second == nil {
		t.Fatal("AttachedDone() = nil after Restart, want the replacement stream channel")
	}
	if second == first {
		t.Fatal("AttachedDone() still reports the stream that Restart ended")
	}
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement attached stream did not finish")
	}
	if err := ctr.AttachedError(); err != nil {
		t.Fatalf("AttachedError() = %v after a clean replacement stream", err)
	}
	f.waitForStreams(t, 2)
	args := f.streamArgs()
	if len(args) != 2 {
		t.Fatalf("attach streams = %d, want 2", len(args))
	}
	// Docker retains logs across a restart, so the replacement stream must
	// resume with --since rather than replaying the whole history.
	if !slices.Contains(args[1], "--since") {
		t.Errorf("replacement stream args = %v, want --since", args[1])
	}
	if got := buf.String(); got != "attached line\nattached line\n" {
		t.Errorf("attached output = %q, want each stream once", got)
	}
}

func TestStartReattachesAttachedOutputAfterStop(t *testing.T) {
	streamErr := &cli.CLIError{Binary: "container", Args: []string{"logs"}, ExitCode: 1, Stderr: "stream failed"}
	for _, tc := range []struct {
		name      string
		firstErr  error
		secondErr error
		wantErr   error
	}{
		{
			name:     "successful replacement keeps the previous error",
			firstErr: streamErr,
			wantErr:  streamErr,
		},
		{
			name:      "replacement error is reported",
			secondErr: streamErr,
			wantErr:   streamErr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &recordingStreamerRunner{
				inner: newTestRunner(),
				data:  "attached line\n",
				errs:  []error{tc.firstErr, tc.secondErr},
			}
			var buf bytes.Buffer
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"),
				WithAttachOutput(&buf),
				withRunner(f),
				withEngine(appleEngine{}),
			)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			first := ctr.AttachedDone()
			if first == nil {
				t.Fatal("AttachedDone() = nil after Run, want a channel")
			}
			f.waitForStreams(t, 1)
			select {
			case <-first:
			case <-time.After(2 * time.Second):
				t.Fatal("initial attached stream did not finish")
			}
			if tc.firstErr != nil && ctr.AttachedError() == nil {
				t.Fatal("AttachedError() = nil after a failed initial stream")
			}

			if err := ctr.Stop(context.Background(), nil); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if err := ctr.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if start := f.inner.lockedCallWith("start"); len(start) == 0 {
				t.Fatal("Start did not invoke the start command")
			}

			second := ctr.AttachedDone()
			if second == nil {
				t.Fatal("AttachedDone() = nil after Start, want the replacement stream channel")
			}
			if second == first {
				t.Fatal("AttachedDone() still reports the stream that Stop ended")
			}
			select {
			case <-second:
			case <-time.After(2 * time.Second):
				t.Fatal("replacement attached stream did not finish")
			}
			if err := ctr.AttachedError(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("AttachedError() = %v, want %v", err, tc.wantErr)
			}
			f.waitForStreams(t, 2)
			args := f.streamArgs()
			if len(args) != 2 {
				t.Fatalf("attach streams = %d, want 2", len(args))
			}
			// Apple Container truncates its log at start, so the ordinary
			// follow already resumes with only the new run's output.
			if !slices.Equal(args[1], []string{"logs", "--follow", "myctr"}) {
				t.Errorf("replacement stream args = %v, want the ordinary follow argv", args[1])
			}
		})
	}
}

func TestRestartPreservesAttachedErrorAcrossReattach(t *testing.T) {
	firstErr := &cli.CLIError{Binary: "container", Args: []string{"logs"}, ExitCode: 1, Stderr: "first stream failed"}
	secondErr := &cli.CLIError{Binary: "container", Args: []string{"logs"}, ExitCode: 1, Stderr: "second stream failed"}
	for _, tc := range []struct {
		name     string
		errs     []error
		wantErrs []error
	}{
		{
			name:     "successful replacement keeps the previous failure",
			errs:     []error{firstErr},
			wantErrs: []error{firstErr},
		},
		{
			name:     "later failure accumulates with the previous failure",
			errs:     []error{firstErr, secondErr},
			wantErrs: []error{firstErr, secondErr},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n", errs: tc.errs}
			var buf bytes.Buffer
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"),
				WithAttachOutput(&buf),
				withRunner(f),
				withEngine(appleEngine{}),
			)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			first := ctr.AttachedDone()
			if first == nil {
				t.Fatal("AttachedDone() = nil after Run, want a channel")
			}
			f.waitForStreams(t, 1)
			select {
			case <-first:
			case <-time.After(2 * time.Second):
				t.Fatal("initial attached stream did not finish")
			}
			if err := ctr.AttachedError(); !errors.Is(err, firstErr) {
				t.Fatalf("AttachedError() = %v after the initial stream, want its failure", err)
			}

			if err := ctr.Restart(context.Background(), nil); err != nil {
				t.Fatalf("Restart: %v", err)
			}
			second := ctr.AttachedDone()
			if second == nil {
				t.Fatal("AttachedDone() = nil after Restart, want the replacement stream channel")
			}
			if second == first {
				t.Fatal("AttachedDone() still reports the stream that Restart ended")
			}
			select {
			case <-second:
			case <-time.After(2 * time.Second):
				t.Fatal("replacement attached stream did not finish")
			}
			f.waitForStreams(t, 2)

			// The channel a caller retained from the first stream stays
			// closed, and every failure recorded so far remains matchable.
			select {
			case <-first:
			default:
				t.Fatal("the initial AttachedDone channel was reopened by the restart")
			}
			attachedErr := ctr.AttachedError()
			for _, wantErr := range tc.wantErrs {
				if !errors.Is(attachedErr, wantErr) {
					t.Fatalf("AttachedError() = %v after reattach, want it to match %v", attachedErr, wantErr)
				}
			}
		})
	}
}

func TestStartDoesNotDuplicateActiveAttachedStream(t *testing.T) {
	f := &blockingStreamerRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))
	t.Cleanup(ctr.StopAttached)

	countStreams := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		streams := 0
		for _, call := range f.calls {
			if call[0] == "logs" {
				streams++
			}
		}
		return streams
	}
	deadline := time.Now().Add(2 * time.Second)
	for countStreams() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("initial attached stream never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if streams := countStreams(); streams != 1 {
		t.Fatalf("attach streams = %d, want the still-active stream only", streams)
	}
}

func TestLifecycleWithoutAttachOutputStartsNoStream(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		f := &recordingStreamerRunner{inner: newTestRunner()}
		ctr := runTestContainer(t, f)
		if err := ctr.Stop(context.Background(), nil); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := ctr.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if streams := f.streams(); len(streams) != 0 {
			t.Fatalf("attach streams = %d, want none without WithAttachOutput", len(streams))
		}
		if done := ctr.AttachedDone(); done != nil {
			t.Fatal("AttachedDone() is non-nil without WithAttachOutput")
		}
	})

	t.Run("docker restart", func(t *testing.T) {
		f := &recordingStreamerRunner{inner: newTestRunner()}
		f.inner.imagePresent = true
		ctr, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"),
			withRunner(f),
			withEngine(dockerEngine{}),
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if err := ctr.Restart(context.Background(), nil); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		if streams := f.streams(); len(streams) != 0 {
			t.Fatalf("attach streams = %d, want none without WithAttachOutput", len(streams))
		}
		if done := ctr.AttachedDone(); done != nil {
			t.Fatal("AttachedDone() is non-nil without WithAttachOutput")
		}
	})
}

// blockingSink blocks every write until released and records how many writers
// were inside Write at once, so a test can hold an attach stream inside its
// copy loop and detect overlapping streams.
type blockingSink struct {
	once      sync.Once
	entered   chan struct{}
	released  chan struct{}
	mu        sync.Mutex
	active    int
	maxActive int
	completed int
}

func (s *blockingSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	<-s.released
	s.mu.Lock()
	s.active--
	s.completed++
	s.mu.Unlock()
	return len(p), nil
}

func (s *blockingSink) stats() (maxActive, completed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxActive, s.completed
}

func TestRestartFallbackSerializesAttachStreams(t *testing.T) {
	f := &recordingStreamerRunner{inner: newTestRunner(), data: "attached line\n"}
	sink := &blockingSink{entered: make(chan struct{}), released: make(chan struct{})}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(sink),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("attach stream never wrote to the sink")
	}

	// Cancellation cannot interrupt the blocked write, so the previous stream
	// is still alive when Restart re-attaches. The replacement stream must not
	// write to the same sink concurrently, and it must report the wedged sink
	// instead of blocking forever.
	if err := ctr.Restart(context.Background(), nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("AttachedDone() = nil after Restart, want the replacement stream channel")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement stream never reported the wedged sink")
	}
	if got := len(f.streams()); got != 1 {
		t.Fatalf("attach streams = %d while the previous stream is still writing, want 1", got)
	}
	if err := ctr.AttachedError(); err == nil {
		t.Error("AttachedError() = nil, want the abandoned replacement stream failure")
	}

	// Once the sink drains, a later restart attaches again and the two
	// streams never overlap inside the sink.
	close(sink.released)
	if err := ctr.Restart(context.Background(), nil); err != nil {
		t.Fatalf("Restart after release: %v", err)
	}
	f.waitForStreams(t, 2)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, completed := sink.stats(); completed >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both attach streams did not reach the sink after release")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if maxActive, _ := sink.stats(); maxActive != 1 {
		t.Errorf("max concurrent sink writers = %d, want 1", maxActive)
	}
}

func TestAttachedErrorReportsStreamFailure(t *testing.T) {
	streamErr := &cli.CLIError{Binary: "container", Args: []string{"logs"}, ExitCode: 1, Stderr: "log stream failed"}
	f := &failingStreamerRunner{inner: newTestRunner(), err: streamErr}
	ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))

	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("AttachedDone() = nil, want a channel")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not finish")
	}
	if err := ctr.AttachedError(); !errors.Is(err, streamErr) {
		t.Fatalf("AttachedError() = %v, want the stream failure", err)
	}
}

func TestKillNonFatalSignalKeepsAttachedStream(t *testing.T) {
	f := &blockingStreamerRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f, WithAttachOutput(io.Discard))

	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("AttachedDone() = nil, want a channel")
	}
	if err := ctr.Kill(context.Background(), "SIGUSR1"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-done:
		t.Fatal("a catchable signal detached the log stream while the process may still run")
	default:
	}

	if err := ctr.Kill(context.Background(), "SIGKILL"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not terminate after SIGKILL")
	}
	if err := ctr.AttachedError(); err != nil {
		t.Errorf("AttachedError() = %v, want nil for a teardown cancellation", err)
	}
}

func TestWithAttachOutputOptionValidation(t *testing.T) {
	cfg := newConfig()
	err := WithAttachOutput(nil)(cfg)
	if err == nil {
		t.Fatal("expected error for nil writer, got nil")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if valErr.Option != "WithAttachOutput" {
		t.Errorf("Option = %q, want WithAttachOutput", valErr.Option)
	}
}

type blockingStreamerRunner struct {
	*fakeRunner
}

func (f *blockingStreamerRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	f.recordCall(args)
	pr, pw := io.Pipe()
	go func() {
		<-ctx.Done()
		_ = pw.CloseWithError(ctx.Err())
	}()
	return pr, nil
}

func TestFollowLogsToPreservesContextCancellation(t *testing.T) {
	f := &blockingStreamerRunner{
		fakeRunner: newTestRunner(),
	}
	ctr := runTestContainer(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	var buf bytes.Buffer
	err := ctr.FollowLogsTo(ctx, &buf)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FollowLogsTo error = %v, want context.Canceled", err)
	}
}

func TestWithAttachOutputLifetimeAndTermination(t *testing.T) {
	f := &blockingStreamerRunner{
		fakeRunner: newTestRunner(),
	}

	createCtx, cancelCreate := context.WithCancel(context.Background())
	var buf bytes.Buffer
	ctr, err := Run(createCtx, "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Cancel the creation context. The attached log stream must stay alive!
	cancelCreate()

	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("expected non-nil AttachedDone channel")
	}

	select {
	case <-done:
		t.Fatal("attached stream was prematurely closed when creation context was cancelled")
	default:
		// Stream is still alive as expected
	}

	// Terminate the container; the attached log stream must finish cleanly.
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	select {
	case <-done:
		// Cleanly closed
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not terminate after container termination")
	}
}

func TestWithAttachOutputStoppedOnStop(t *testing.T) {
	f := &blockingStreamerRunner{
		fakeRunner: newTestRunner(),
	}

	var buf bytes.Buffer
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("expected non-nil AttachedDone channel")
	}

	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-done:
		// Cleanly closed on Stop
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not terminate after container Stop")
	}
}

func TestWithAttachOutputStoppedOnKill(t *testing.T) {
	f := &blockingStreamerRunner{
		fakeRunner: newTestRunner(),
	}

	var buf bytes.Buffer
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"),
		WithAttachOutput(&buf),
		withRunner(f),
		withEngine(appleEngine{}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := ctr.AttachedDone()
	if done == nil {
		t.Fatal("expected non-nil AttachedDone channel")
	}

	if err := ctr.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case <-done:
		// Cleanly closed on Kill
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not terminate after container Kill")
	}
}
