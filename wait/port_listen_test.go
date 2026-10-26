package wait

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// portListenTarget extends fakeTarget with a scripted container-side listen
// check. The last scripted status repeats for later calls.
type portListenTarget struct {
	*fakeTarget
	statuses []PortListenStatus
	errs     []error
	calls    int
	ports    []string
}

func (t *portListenTarget) ContainerPortStatus(_ context.Context, port string) (PortListenStatus, error) {
	call := t.calls
	t.calls++
	t.ports = append(t.ports, port)
	if call < len(t.errs) && t.errs[call] != nil {
		return PortListenUnsupported, t.errs[call]
	}
	if len(t.statuses) == 0 {
		return PortListenUnsupported, nil
	}
	index := call
	if index >= len(t.statuses) {
		index = len(t.statuses) - 1
	}
	return t.statuses[index], nil
}

// startProxyOnlyListener accepts and immediately closes connections, like a
// Docker port proxy whose container process is not listening yet.
func startProxyOnlyListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		wg.Wait()
	})
	return ln
}

type cancelingPortListenTarget struct {
	*portListenTarget
	cancel context.CancelFunc
}

func (t *cancelingPortListenTarget) ContainerPortStatus(ctx context.Context, port string) (PortListenStatus, error) {
	status, err := t.portListenTarget.ContainerPortStatus(ctx, port)
	if status == PortListenClosed && t.calls == 2 {
		t.cancel()
	}
	return status, err
}

func TestForListeningPortRejectsProxyOnlyConnection(t *testing.T) {
	ln := startProxyOnlyListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := &cancelingPortListenTarget{
		portListenTarget: &portListenTarget{fakeTarget: newFakeTarget(), statuses: []PortListenStatus{PortListenClosed}},
		cancel:           cancel,
	}
	target.endpoint = ln.Addr().String()

	done := make(chan error, 1)
	go func() {
		done <- ForListeningPort("6379/tcp").
			WithPollInterval(5*time.Millisecond).
			WaitUntilReady(ctx, target)
	}()
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	var err error
	select {
	case err = <-done:
	case <-watchdog.C:
		t.Fatal("wait did not finish after two closed container-listen checks")
	}
	if err == nil {
		t.Fatal("host connection alone must not report readiness")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want caller cancellation", err)
	}
	if !strings.Contains(err.Error(), "not listening") {
		t.Errorf("error = %v, want a container-side listen diagnostic", err)
	}
	if target.calls < 2 {
		t.Errorf("container checks = %d, want retries after host success", target.calls)
	}
}

func TestForListeningPortWaitsForContainerListen(t *testing.T) {
	ln := startProxyOnlyListener(t)
	target := &portListenTarget{
		fakeTarget: newFakeTarget(),
		statuses:   []PortListenStatus{PortListenClosed, PortListenClosed, PortListenOpen},
	}
	target.endpoint = ln.Addr().String()

	err := ForListeningPort("6379/tcp").
		WithStartupTimeout(2*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.calls != 3 {
		t.Errorf("container checks = %d, want 3", target.calls)
	}
}

func TestForExposedPortChecksResolvedContainerPort(t *testing.T) {
	ln := startProxyOnlyListener(t)
	target := &portListenTarget{
		fakeTarget: newFakeTarget(),
		statuses:   []PortListenStatus{PortListenClosed, PortListenOpen},
	}
	target.endpoint = ln.Addr().String()

	err := ForExposedPort().
		WithStartupTimeout(2*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	// The empty specification lets the target apply the same implicit
	// selection Endpoint uses instead of guessing a host port.
	if len(target.ports) != 2 || target.ports[0] != "" || target.ports[1] != "" {
		t.Errorf("container check ports = %v, want two empty specifications", target.ports)
	}
}

func TestForListeningPortUnsupportedContainerCheckUsesHostDial(t *testing.T) {
	ln := startProxyOnlyListener(t)
	target := &portListenTarget{fakeTarget: newFakeTarget(), statuses: []PortListenStatus{PortListenUnsupported}}
	target.endpoint = ln.Addr().String()

	err := ForListeningPort("6379/tcp").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.calls != 1 {
		t.Errorf("container checks = %d, want 1", target.calls)
	}
}

func TestForListeningPortWithHostOnlySkipsContainerCheck(t *testing.T) {
	ln := startProxyOnlyListener(t)
	target := &portListenTarget{fakeTarget: newFakeTarget(), statuses: []PortListenStatus{PortListenClosed}}
	target.endpoint = ln.Addr().String()

	err := ForListeningPort("6379/tcp").
		WithHostOnly().
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.calls != 0 {
		t.Errorf("container checks = %d, want 0 with WithHostOnly", target.calls)
	}
}

func TestForListeningPortRetriesContainerCheckErrors(t *testing.T) {
	ln := startProxyOnlyListener(t)
	target := &portListenTarget{
		fakeTarget: newFakeTarget(),
		statuses:   []PortListenStatus{PortListenUnsupported, PortListenOpen},
		errs:       []error{errors.New("temporary exec failure")},
	}
	target.endpoint = ln.Addr().String()

	err := ForListeningPort("6379/tcp").
		WithStartupTimeout(2*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.calls != 2 {
		t.Errorf("container checks = %d, want 2", target.calls)
	}
}

type blockingListenTarget struct {
	*fakeTarget
	started chan struct{}
	once    sync.Once
}

func (t *blockingListenTarget) ContainerPortStatus(ctx context.Context, _ string) (PortListenStatus, error) {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return PortListenUnsupported, ctx.Err()
}

func TestForListeningPortContainerCheckHonorsCancellation(t *testing.T) {
	ln := startProxyOnlyListener(t)
	target := &blockingListenTarget{fakeTarget: newFakeTarget(), started: make(chan struct{})}
	target.endpoint = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ForListeningPort("6379/tcp").
			WithStartupTimeout(10*time.Second).
			WaitUntilReady(ctx, target)
	}()
	select {
	case <-target.started:
	case <-time.After(5 * time.Second):
		t.Fatal("container check was not called")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitUntilReady did not return after cancellation")
	}
}
