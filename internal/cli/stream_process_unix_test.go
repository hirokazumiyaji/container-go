//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStreamCloseReapsDirectChild(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep is unavailable: %v", err)
	}
	stream, err := (&ExecRunner{Binary: sleepPath}).Stream(context.Background(), "30")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() { _ = stream.Close() })

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped after Close")
	}
	if got := ps.waitCalls.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want exactly one", got)
	}
	if ps.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil after Wait")
	}
}

func TestStreamCancellationReapsDirectChildWithoutReadOrClose(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep is unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := (&ExecRunner{Binary: sleepPath}).Stream(ctx, "30")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() { _ = stream.Close() })

	cancel()
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped after cancellation")
	}
	if got := ps.waitCalls.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want exactly one", got)
	}
	if ps.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil after Wait")
	}
}

func TestStreamExitReapStress(t *testing.T) {
	stub := writeStub(t, `exit 0`)
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := (&ExecRunner{Binary: stub}).Stream(ctx, "logs")
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: Stream: %v", i, err)
		}
		ps := stream.(*processStream)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for operation := range 3 {
			wg.Add(1)
			go func(operation int) {
				defer wg.Done()
				<-start
				switch (i + operation) % 3 {
				case 0:
					_ = stream.Close()
				case 1:
					cancel()
				default:
					_, _ = stream.Read(make([]byte, 32))
				}
			}(operation)
		}
		close(start)
		wg.Wait()
		select {
		case <-ps.waitDone:
		case <-time.After(5 * time.Second):
			cancel()
			_ = stream.Close()
			t.Fatalf("iteration %d: direct child was not reaped", i)
		}
		if got := ps.waitCalls.Load(); got != 1 {
			t.Fatalf("iteration %d: wait calls = %d, want exactly one", i, got)
		}
		if ps.cmd.ProcessState == nil {
			t.Fatalf("iteration %d: ProcessState is nil after Wait", i)
		}
		cancel()
		_ = stream.Close()
	}
}

func TestTerminateProcessTreePinsChildBeforeWaitInterleaving(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	configureProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	groupReached := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		releaseNow()
	})

	ops := unixProcessOps{
		signal:  (*os.Process).Signal,
		getpgid: unix.Getpgid,
		killGroup: func(pid int, sig syscall.Signal) error {
			close(groupReached)
			<-release
			return unix.Kill(pid, sig)
		},
		kill: cmd.Process.Kill,
		// The test seam supplies the stopped-state observation that
		// production obtains from the platform identity barrier.
		stopped: func() (bool, error) { return true, nil },
	}
	resultDone := make(chan terminationResult, 1)
	go func() { resultDone <- terminateProcessTreeWithOps(cmd, ops) }()
	select {
	case <-groupReached:
	case <-time.After(2 * time.Second):
		t.Fatal("termination did not reach the group signal")
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
		t.Fatal("waiter completed while the pinned child awaited group termination")
	case <-time.After(50 * time.Millisecond):
	}
	releaseNow()

	select {
	case result := <-resultDone:
		if !result.active || result.err != nil {
			t.Fatalf("termination result = %+v, want active successful signal", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("termination did not complete after group release")
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not observe group termination")
	}
}

func TestUnixCancellationPreservesGenuinePositiveExit(t *testing.T) {
	exitErr := terminalErrorExitErrorWithCode(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ps := &processStream{
		ctx:       ctx,
		binary:    "docker",
		args:      []string{"logs", "--follow", "x"},
		stderr:    &tailBuffer{},
		cancelled: true,
	}
	ps.drainCompleted.Store(true)
	err := ps.terminalError(exitErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal error = %v, want context cancellation", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 1 {
		t.Fatalf("terminal error = %v, want genuine exit-1 CLIError", err)
	}
}

func TestStreamCloseTerminatesDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	stub := writeStub(t, descendantScript(`wait "$child"`))
	stream, err := (&ExecRunner{Binary: stub}).Stream(context.Background(), pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() {
		_ = stream.Close()
		_ = writeDescendantStopFile(pidFile)
	})
	descendantPID := waitForDescendantPID(t, pidFile)

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-stream.(*processStream).waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped after Close")
	}
	// A process group signal terminates descendants, but it does not reap
	// them. Accept a zombie here: the platform init/subreaper owns that
	// responsibility once the direct child is gone.
	assertStreamProcessTerminated(t, descendantPID)
}

func TestStreamCloseAfterParentExitDoesNotWaitForDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	stub := writeStub(t, descendantScript(`exit 0`))
	stream, err := (&ExecRunner{Binary: stub}).Stream(context.Background(), pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() {
		_ = stream.Close()
		_ = writeDescendantStopFile(pidFile)
	})
	descendantPID := waitForDescendantPID(t, pidFile)
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped")
	}

	// Once the direct child is reaped, Close must not use its PID/PGID
	// again. Closing the source descriptors still lets Close return even
	// when a descendant inherited those descriptors.
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := writeDescendantStopFile(pidFile); err != nil {
		t.Fatalf("stop descendant: %v", err)
	}
	assertStreamProcessTerminated(t, descendantPID)
}

func TestStreamCancellationTerminatesDescendantsWithoutReadOrClose(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	stub := writeStub(t, descendantScript(`wait "$child"`))
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := (&ExecRunner{Binary: stub}).Stream(ctx, pidFile)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ps := stream.(*processStream)
	t.Cleanup(func() {
		_ = stream.Close()
		_ = writeDescendantStopFile(pidFile)
	})
	descendantPID := waitForDescendantPID(t, pidFile)

	cancel()
	select {
	case <-ps.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child was not reaped after cancellation")
	}
	assertStreamProcessTerminated(t, descendantPID)
}

func descendantScript(action string) string {
	return `done="$1.done"; (while [ ! -f "$done" ]; do sleep 0.05; done) & child=$!; printf '%s\n' "$child" > "$1"; ` + action
}

func writeDescendantStopFile(pidFile string) error {
	return os.WriteFile(pidFile+".done", nil, 0o600)
}

func waitForDescendantPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant PID was not written to %s", path)
	return 0
}

func assertStreamProcessTerminated(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	state := ""
	for time.Now().Before(deadline) {
		var err error
		state, err = processState(pid)
		if err != nil || state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d was not terminated (state %q)", pid, state)
}
