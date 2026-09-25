//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"
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
