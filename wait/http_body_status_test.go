package wait

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func httpTargetFor(server *httptest.Server) *fakeTarget {
	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(server.URL, "http://")
	return target
}

func TestForHTTPWithStatusCode(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := ForHTTP("/").
		WithStatusCode(http.StatusNoContent).
		WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), httpTargetFor(server))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if hits.Load() < 2 {
		t.Errorf("probe hits = %d, want a retry after the rejected status", hits.Load())
	}
}

func TestForHTTPWithStatusCodeRejectsOtherStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := ForHTTP("/").
		WithStatusCode(http.StatusNoContent).
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), httpTargetFor(server))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if !strings.Contains(err.Error(), "status 200 not accepted") {
		t.Errorf("error = %q, want the rejected status diagnostic", err)
	}
}

func TestForHTTPStatusSettersReplaceEachOther(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Run("matcher after fixed code", func(t *testing.T) {
		err := ForHTTP("/").
			WithStatusCode(http.StatusTeapot).
			WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }).
			WithStartupTimeout(time.Second).
			WithPollInterval(2*time.Millisecond).
			WaitUntilReady(context.Background(), httpTargetFor(server))
		if err != nil {
			t.Fatalf("WaitUntilReady: %v", err)
		}
	})
	t.Run("fixed code after matcher", func(t *testing.T) {
		err := ForHTTP("/").
			WithStatusCodeMatcher(func(int) bool { return true }).
			WithStatusCode(http.StatusTeapot).
			WithStartupTimeout(40*time.Millisecond).
			WithPollInterval(5*time.Millisecond).
			WaitUntilReady(context.Background(), httpTargetFor(server))
		if err == nil {
			t.Fatal("wait unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "status 200 not accepted") {
			t.Errorf("error = %q, want the fixed status code to win", err)
		}
	})
}

func TestForHTTPWithStatusCodeValidation(t *testing.T) {
	for _, code := range []int{-1, 0, 99, 600} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			err := ForHTTP("/").
				WithStatusCode(code).
				WithStartupTimeout(time.Second).
				WaitUntilReady(context.Background(), newFakeTarget())
			if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Field != "status code" {
				t.Fatalf("error = %v, want the status code configuration error", err)
			}
		})
	}
}

func TestForHTTPWithBody(t *testing.T) {
	payload := []byte(`{"ready":true}`)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != string(payload) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := ForHTTP("/ready").
		WithMethod(http.MethodPost).
		WithHeader("Content-Type", "application/json").
		WithBody(payload).
		WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), httpTargetFor(server))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if hits.Load() < 2 {
		t.Errorf("accepted body hits = %d, want the body replayed on every poll", hits.Load())
	}
}

func TestForHTTPWithBodySetter(t *testing.T) {
	strategy := ForHTTP("/").WithBody([]byte("payload"))
	if !strategy.bodySet || string(strategy.body) != "payload" {
		t.Fatalf("body = %q set = %v", strategy.body, strategy.bodySet)
	}
	values := strategy.DiagnosticValues()
	found := false
	for _, value := range values {
		if value == "payload" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DiagnosticValues = %v, want the request body for redaction", values)
	}
}

func TestForHTTPWithBodyClonesCallerSlice(t *testing.T) {
	body := []byte("payload")
	strategy := ForHTTP("/").WithBody(body)
	body[0] = 'X'
	if got := string(strategy.body); got != "payload" {
		t.Errorf("stored body = %q, want a clone of the caller's value", got)
	}
}

func TestForHTTPWithBodyValidation(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "over the stream window", body: bytes.Repeat([]byte("a"), cli.MaxStreamOverlap+1)},
		{name: "expanded encoding over the stream window", body: bytes.Repeat([]byte("a"), cli.MaxStreamOverlap)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ForHTTP("/").
				WithBody(tc.body).
				WithStartupTimeout(time.Second).
				WaitUntilReady(context.Background(), newFakeTarget())
			if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Field != "body" {
				t.Fatalf("error = %v, want the body configuration error", err)
			}
		})
	}
}
