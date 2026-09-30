package wait

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// connCountingServer is an httptest server that records how many TCP
// connections it accepted. Request counts cannot show connection
// churn: one pooled connection answers many probes, so the cost the
// drain avoids is invisible without watching the accept side.
type connCountingServer struct {
	*httptest.Server
	conns atomic.Int64
}

// newConnCountingServer serves handler and counts accepted
// connections.
func newConnCountingServer(t *testing.T, handler http.HandlerFunc) *connCountingServer {
	t.Helper()
	s := &connCountingServer{}
	s.Server = httptest.NewUnstartedServer(handler)
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			s.conns.Add(1)
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}

// tricklingBodyHandler answers with a small body delivered in chunks
// spaced further apart than net/http's internal post-close drain is
// willing to wait (50ms, maxPostCloseReadTime).
//
// This is the shape that makes the drain observable. net/http does
// attempt to drain an unread body after Close so the connection can go
// back to the pool, but it gives up after 50ms, and it only tries at
// all while the declared ContentLength fits under 256KiB
// (maxPostCloseReadBytes). A body that is still arriving at 80ms is
// therefore abandoned every time, and the next probe opens a fresh
// connection. A probe that reads the body itself is not subject to
// that deadline.
func tricklingBodyHandler(t *testing.T, chunks int, delay time.Duration) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing; the body would not trickle")
			return
		}
		// Header first, so the client sees the status immediately and
		// the probe proceeds to read the body.
		flusher.Flush()
		for range chunks {
			fmt.Fprint(w, "not yet\n")
			flusher.Flush()
			time.Sleep(delay)
		}
	}
}

// TestForHTTPPollsReuseOneConnection is the regression for the undrained
// response body. Closing the body without reading it leaves the
// connection to net/http's post-close drain, which abandons a body that
// is still arriving, so every poll iteration after the first paid for a
// new TCP connection. Reading the body keeps one connection alive for
// the whole poll.
func TestForHTTPPollsReuseOneConnection(t *testing.T) {
	var probes atomic.Int64
	srv := newConnCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		tricklingBodyHandler(t, 4, 20*time.Millisecond)(w, r)
	})

	target := newFakeTarget()
	target.endpoint = srv.Listener.Addr().String()

	// Each probe reads a body that takes ~80ms to arrive, so the
	// startup timeout has to cover several iterations.
	s := ForHTTP("/").WithPollInterval(time.Millisecond).WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want timeout when the endpoint never becomes ready")
	}

	// The poll loop has to have iterated for the count to mean
	// anything: one probe on one connection is indistinguishable from
	// reuse.
	if got := probes.Load(); got < 3 {
		t.Fatalf("server answered %d probes; want at least 3 to exercise reuse", got)
	}
	if got := srv.conns.Load(); got != 1 {
		t.Errorf("server accepted %d connections for %d probes; want exactly 1: "+
			"an undrained body is not returned to the connection pool", got, probes.Load())
	}
}

// TestForHTTPDrainIsBounded keeps the cap honest. A body larger than
// maxDrainBytes is deliberately not read to EOF, so an endpoint cannot
// turn the drain into unbounded memory growth. The wait still has to
// terminate and still has to honour the status it received.
func TestForHTTPDrainIsBounded(t *testing.T) {
	// 8MiB, far past the 64KiB cap.
	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = 'x'
	}
	srv := newConnCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	target := newFakeTarget()
	target.endpoint = srv.Listener.Addr().String()

	done := make(chan error, 1)
	go func() { done <- ForHTTP("/").WaitUntilReady(context.Background(), target) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitUntilReady() = %v, want nil: the 200 status was received", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("WaitUntilReady did not return; the drain is not bounded")
	}
}

// TestForHTTPStatusVerdictSurvivesDrainError pins that a drain failure
// does not turn a received response into a probe error. The response
// arrived, so the matcher decides the outcome and the drain result is
// not the wait's verdict.
func TestForHTTPStatusVerdictSurvivesDrainError(t *testing.T) {
	// Announce a body, deliver part of it, then abort the connection.
	// The client sees a 200 and then a body read error.
	srv := newConnCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "partial")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})

	target := newFakeTarget()
	target.endpoint = srv.Listener.Addr().String()

	err := ForHTTP("/").WithStartupTimeout(5*time.Second).WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady() = %v, want nil: the 200 status arrived before the body failed", err)
	}
}

// TestForHTTPRejectsAnOversizedStatusVerdict confirms the cap does not
// swallow the status: a response past the drain cap that the matcher
// rejects still reports the status, not a drain problem.
func TestForHTTPRejectsAnOversizedStatusVerdict(t *testing.T) {
	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = 'x'
	}
	srv := newConnCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(body)
	})

	target := newFakeTarget()
	target.endpoint = srv.Listener.Addr().String()

	err := ForHTTP("/").WithStartupTimeout(3*time.Second).WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want an error when the status is 500")
	}
	if got := err.Error(); !strings.Contains(got, "status 500") {
		t.Errorf("error = %v, want it to report the status verdict", err)
	}
}
