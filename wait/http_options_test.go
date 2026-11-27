package wait

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForHTTPAllowInsecureTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "https://")

	err := ForHTTP("/").WithTLS().
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly trusted the self-signed certificate")
	}

	if err := ForHTTP("/").WithAllowInsecureTLS().
		WithStartupTimeout(time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady with WithAllowInsecureTLS: %v", err)
	}
}

func TestForHTTPAllowInsecureTLSClonesCallerConfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "https://")
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	// The override is applied when the client is built, so it also covers a
	// TLS config installed after WithAllowInsecureTLS.
	err := ForHTTP("/").
		WithAllowInsecureTLS().
		WithTLSConfig(cfg).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("WithAllowInsecureTLS mutated the caller's TLS config")
	}
}

func TestForHTTPFollowRedirects(t *testing.T) {
	var readyHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/ready", http.StatusFound)
			return
		}
		readyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	wait := func(strategy *HTTPStrategy) error {
		t.Helper()
		return strategy.WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	}

	if err := wait(ForHTTP("/start")); err != nil {
		t.Fatalf("default redirect policy: %v", err)
	}
	if readyHits.Load() == 0 {
		t.Fatal("default client did not follow a same-origin redirect")
	}

	readyHits.Store(0)
	if err := wait(ForHTTP("/start").
		WithFollowRedirects(false).
		WithStatusCode(http.StatusFound)); err != nil {
		t.Fatalf("WithFollowRedirects(false): %v", err)
	}
	if readyHits.Load() != 0 {
		t.Errorf("/ready was requested %d time(s); the redirect response should be validated instead", readyHits.Load())
	}

	readyHits.Store(0)
	if err := wait(ForHTTP("/start").
		WithFollowRedirects(false).
		WithFollowRedirects(true)); err != nil {
		t.Fatalf("WithFollowRedirects(true): %v", err)
	}
	if readyHits.Load() == 0 {
		t.Fatal("WithFollowRedirects(true) did not restore redirect following")
	}
}

func TestForHTTPFollowRedirectsFalseDoesNotMutateCustomClient(t *testing.T) {
	var readyHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/ready", http.StatusFound)
			return
		}
		readyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	err := ForHTTP("/start").
		WithHTTPClient(client).
		WithFollowRedirects(false).
		WithStatusCode(http.StatusFound).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if readyHits.Load() != 0 {
		t.Errorf("/ready was requested %d time(s); WithFollowRedirects(false) did not apply", readyHits.Load())
	}
	if client.CheckRedirect == nil {
		t.Fatal("WithFollowRedirects(false) cleared the caller's CheckRedirect")
	}
}

func TestForHTTPRequestTimeoutBoundsEachRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	err := ForHTTP("/").
		WithRequestTimeout(20*time.Millisecond).
		WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if hits.Load() < 2 {
		t.Errorf("requests = %d, want a retry after the timed-out request", hits.Load())
	}
}

func TestForHTTPRequestTimeoutOverridesCustomClientWithoutMutation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	err := ForHTTP("/").
		WithHTTPClient(client).
		WithRequestTimeout(20*time.Millisecond).
		WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if client.Timeout != 5*time.Second {
		t.Errorf("caller client Timeout = %v, want it unchanged", client.Timeout)
	}
}

func TestForHTTPRequestTimeoutRejectsNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Millisecond} {
		t.Run(d.String(), func(t *testing.T) {
			err := Validate(ForHTTP("/").WithRequestTimeout(d))
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("error = %T %v, want *ConfigError", err, err)
			}
			if configErr.Strategy != "ForHTTP" || configErr.Field != "request timeout" {
				t.Errorf("ConfigError = %+v, want strategy ForHTTP and field request timeout", configErr)
			}
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Errorf("error = %v, want ErrInvalidConfiguration", err)
			}
		})
	}
}

func TestForHTTPHostPreservedOnSameOriginRedirect(t *testing.T) {
	hosts := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/ready", http.StatusFound)
			return
		}
		hosts <- r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	err := ForHTTP("/start").
		WithHeader("Host", "virtual.example.com").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	select {
	case host := <-hosts:
		if host != "virtual.example.com" {
			t.Errorf("redirected request Host = %q, want the explicit Host header", host)
		}
	default:
		t.Fatal("redirect target did not receive the probe")
	}
}
