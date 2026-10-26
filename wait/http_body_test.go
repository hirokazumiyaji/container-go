package wait

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForHTTPResponseMatcherAcceptsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"UP"}`)
	}))
	t.Cleanup(srv.Close)

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")

	err := ForHTTP("/").
		WithResponseMatcher(func(body io.Reader) bool {
			data, err := io.ReadAll(body)
			return err == nil && strings.Contains(string(data), `"UP"`)
		}).
		WithStartupTimeout(3*time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForHTTPResponseMatcherRetriesRejectedBody(t *testing.T) {
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"status":"STARTING"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"UP"}`)
	}))
	t.Cleanup(srv.Close)

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")

	err := ForHTTP("/").
		WithResponseMatcher(func(body io.Reader) bool {
			data, err := io.ReadAll(body)
			return err == nil && strings.Contains(string(data), `"UP"`)
		}).
		WithPollInterval(time.Millisecond).
		WithStartupTimeout(3*time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := probes.Load(); got < 2 {
		t.Fatalf("probes = %d, want the rejected body to be retried", got)
	}
}

// responseBody records how much of a response body the probe read and
// whether it closed the body.
type responseBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *responseBody) Close() error {
	b.closed = true
	return nil
}

func newResponseClient(bodySize int, status int, bodies *[]*responseBody) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := &responseBody{reader: strings.NewReader(strings.Repeat("x", bodySize))}
		*bodies = append(*bodies, body)
		return &http.Response{
			StatusCode: status,
			Body:       body,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
}

func TestForHTTPResponseMatcherReadIsBounded(t *testing.T) {
	const bodySize = 8 << 20
	var bodies []*responseBody
	client := newResponseClient(bodySize, http.StatusOK, &bodies)
	target := newFakeTarget()
	target.endpoint = "127.0.0.1:1"

	err := ForHTTP("/").
		WithHTTPClient(client).
		WithResponseMatcher(func(body io.Reader) bool {
			// Read only a prefix of the capped reader. The strategy must
			// still drain the remainder within the same cap.
			var prefix [10]byte
			_, _ = io.ReadFull(body, prefix[:])
			return false
		}).
		WithPollInterval(time.Millisecond).
		WithStartupTimeout(20*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want a timeout while the matcher keeps rejecting the body")
	}
	if len(bodies) == 0 {
		t.Fatal("no response bodies were read")
	}
	for i, body := range bodies {
		if body.read != maxDrainBytes {
			t.Errorf("body %d read %d bytes, want the %d-byte cap", i, body.read, maxDrainBytes)
		}
		if !body.closed {
			t.Errorf("body %d was not closed", i)
		}
	}
}

func TestForHTTPResponseMatcherSkipsRejectedStatus(t *testing.T) {
	const bodySize = 8 << 20
	var bodies []*responseBody
	client := newResponseClient(bodySize, http.StatusServiceUnavailable, &bodies)
	target := newFakeTarget()
	target.endpoint = "127.0.0.1:1"

	var matcherCalls atomic.Int64
	err := ForHTTP("/").
		WithHTTPClient(client).
		WithResponseMatcher(func(io.Reader) bool {
			matcherCalls.Add(1)
			return true
		}).
		WithPollInterval(time.Millisecond).
		WithStartupTimeout(20*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want a timeout while the status keeps being rejected")
	}
	if got := matcherCalls.Load(); got != 0 {
		t.Errorf("response matcher calls = %d, want none for a rejected status", got)
	}
	if len(bodies) == 0 {
		t.Fatal("no response bodies were drained")
	}
	for i, body := range bodies {
		if body.read > maxDrainBytes {
			t.Errorf("body %d read %d bytes, want at most %d", i, body.read, maxDrainBytes)
		}
		if !body.closed {
			t.Errorf("body %d was not closed", i)
		}
	}
}

func TestForHTTPResponseMatcherAcceptsPrefixWithoutWaitingForRemainder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "UP")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	err := ForHTTP("/").
		WithResponseMatcher(func(body io.Reader) bool {
			var prefix [2]byte
			_, err := io.ReadFull(body, prefix[:])
			return err == nil && string(prefix[:]) == "UP"
		}).
		WithStartupTimeout(200*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v, want success after the matcher accepts", err)
	}
}
