package wait

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// HTTPStrategy waits until an HTTP request against the container
// returns an acceptable status code.
type HTTPStrategy struct {
	options
	path          string
	port          string
	method        string
	statusMatcher func(int) bool
}

// ForHTTP waits for a plain-HTTP endpoint at path (on the first
// declared port unless WithPort is used) to return 2xx.
func ForHTTP(path string) *HTTPStrategy {
	return &HTTPStrategy{path: path, method: http.MethodGet}
}

// WithPort probes a specific declared port instead of the first one.
func (s *HTTPStrategy) WithPort(port string) *HTTPStrategy {
	s.port = port
	return s
}

// WithMethod sets the HTTP method to use.
func (s *HTTPStrategy) WithMethod(method string) *HTTPStrategy {
	s.method = method
	return s
}

// WithStatusCodeMatcher replaces the default 2xx acceptance check.
func (s *HTTPStrategy) WithStatusCodeMatcher(matcher func(status int) bool) *HTTPStrategy {
	s.statusMatcher = matcher
	return s
}

func (s *HTTPStrategy) WithStartupTimeout(d time.Duration) *HTTPStrategy {
	s.startupTimeout = d
	return s
}

func (s *HTTPStrategy) WithPollInterval(d time.Duration) *HTTPStrategy {
	s.pollInterval = d
	return s
}

func (s *HTTPStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	matcher := s.statusMatcher
	if matcher == nil {
		matcher = func(status int) bool { return status >= 200 && status < 300 }
	}
	client := &http.Client{Timeout: 3 * time.Second}

	return poll(ctx, s.options, target, fmt.Sprintf("wait for HTTP %s %s", s.method, s.path), func(ctx context.Context) error {
		endpoint, err := target.Endpoint(ctx, s.port)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, s.method, "http://"+endpoint+s.path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if !matcher(resp.StatusCode) {
			return fmt.Errorf("status %d not accepted", resp.StatusCode)
		}
		return nil
	})
}
