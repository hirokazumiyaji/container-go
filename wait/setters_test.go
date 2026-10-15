package wait

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStrategySetters(t *testing.T) {
	t.Run("HTTP WithPort/WithMethod", func(t *testing.T) {
		s := ForHTTP("/health").WithPort("8080/tcp").WithMethod(http.MethodPost)
		if s.port != "8080/tcp" || !s.portSet || s.method != http.MethodPost {
			t.Errorf("port=%q portSet=%v method=%q", s.port, s.portSet, s.method)
		}
	})
	t.Run("HTTP headers and basic auth", func(t *testing.T) {
		s := ForHTTP("/").WithHeader("X-Test", "1").WithBasicAuth("u", "p")
		if s.headers["X-Test"] != "1" || !s.basicAuth || s.username != "u" {
			t.Errorf("headers=%v auth=%v", s.headers, s.basicAuth)
		}
	})
	t.Run("HTTP TLS and custom client", func(t *testing.T) {
		s := ForHTTP("/").WithTLS()
		if !s.useTLS {
			t.Error("WithTLS did not set useTLS")
		}
		s2 := ForHTTP("/").WithTLSConfig(&tls.Config{InsecureSkipVerify: true})
		if !s2.useTLS || s2.tlsConfig == nil {
			t.Error("WithTLSConfig not stored")
		}
		c := &http.Client{Timeout: time.Second}
		s3 := ForHTTP("/").WithHTTPClient(c)
		if s3.httpClient != c {
			t.Error("WithHTTPClient not stored")
		}
	})
	t.Run("Exec WithExitCodeMatcher", func(t *testing.T) {
		s := ForExec([]string{"true"}).WithExitCodeMatcher(func(code int) bool { return code == 42 })
		if s.exitMatcher == nil || !s.exitMatcher(42) || s.exitMatcher(0) {
			t.Error("exit matcher not stored")
		}
	})
	t.Run("Log WithPollInterval", func(t *testing.T) {
		s := ForLog("x").WithPollInterval(7 * time.Millisecond)
		if s.pollInterval != 7*time.Millisecond {
			t.Errorf("pollInterval=%v", s.pollInterval)
		}
	})
	t.Run("ForExposedPort", func(t *testing.T) {
		s := ForExposedPort().WithStartupTimeout(time.Second)
		if s.port != "" || s.startupTimeout != time.Second {
			t.Errorf("exposed=%+v", s)
		}
	})
	t.Run("ForAll WithStartupTimeout", func(t *testing.T) {
		s := ForAll().WithStartupTimeout(time.Second)
		if s.startupTimeout != time.Second {
			t.Errorf("startupTimeout=%v", s.startupTimeout)
		}
	})
	t.Run("ForAny WithStartupTimeout", func(t *testing.T) {
		s := ForAny().WithStartupTimeout(time.Second)
		if s.startupTimeout != time.Second {
			t.Errorf("startupTimeout=%v", s.startupTimeout)
		}
	})
}

func TestForHTTPWithHeadersAndBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant") != "acme" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != "user" || p != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	s := ForHTTP("/").
		WithMethod(http.MethodPost).
		WithHeader("X-Tenant", "acme").
		WithBasicAuth("user", "pass").
		WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForHTTPWithCustomClient(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	s := ForHTTP("/").
		WithStatusCodeMatcher(func(code int) bool { return code == http.StatusTeapot }).
		WithHTTPClient(&http.Client{Timeout: 2 * time.Second}).
		WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if hits == 0 {
		t.Error("custom client never used")
	}
}

func TestForHTTPWithTLSAndCustomClientUsesHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("expected TLS request")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "https://")
	s := ForHTTP("/").
		WithTLS().
		WithHTTPClient(srv.Client()).
		WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}
