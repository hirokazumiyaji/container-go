package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
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
	f.calls = append(f.calls, args)
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
	if err := ctr.FollowLogsTo(context.Background(), nil); err == nil {
		t.Error("expected error for nil writer, got nil")
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
	f.calls = append(f.calls, args)
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
