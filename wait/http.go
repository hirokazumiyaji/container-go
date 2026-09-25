package wait

import (
	"context"
	"crypto/tls"
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
	headers       map[string]string
	username      string
	password      string
	basicAuth     bool
	useTLS        bool
	tlsConfig     *tls.Config
	httpClient    *http.Client
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

// WithHeaders adds request headers to the readiness probe.
func (s *HTTPStrategy) WithHeaders(headers map[string]string) *HTTPStrategy {
	if s.headers == nil {
		s.headers = map[string]string{}
	}
	for k, v := range headers {
		s.headers[k] = v
	}
	return s
}

// WithHeader adds one request header to the readiness probe.
func (s *HTTPStrategy) WithHeader(key, value string) *HTTPStrategy {
	return s.WithHeaders(map[string]string{key: value})
}

// WithBasicAuth sends HTTP basic auth with the readiness probe.
func (s *HTTPStrategy) WithBasicAuth(username, password string) *HTTPStrategy {
	s.username, s.password, s.basicAuth = username, password, true
	return s
}

// WithTLS probes via https with the default transport.
func (s *HTTPStrategy) WithTLS() *HTTPStrategy {
	s.useTLS = true
	return s
}

// WithTLSConfig probes via https with a custom TLS config (e.g.,
// InsecureSkipVerify for self-signed test certs).
func (s *HTTPStrategy) WithTLSConfig(cfg *tls.Config) *HTTPStrategy {
	s.useTLS = true
	s.tlsConfig = cfg
	return s
}

// WithHTTPClient delegates transport and timeouts to the caller.
// WithTLS/WithTLSConfig still select the https scheme; the custom
// client supplies the TLS config (for example httptest.NewTLSServer).
func (s *HTTPStrategy) WithHTTPClient(c *http.Client) *HTTPStrategy {
	s.httpClient = c
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

// DiagnosticValues returns the request values that may be echoed in a
// readiness failure. Header names are included as well as values so a
// backend that renders a complete request remains safe.
func (s *HTTPStrategy) DiagnosticValues() []string {
	values := []string{s.path, s.port, s.method, s.username, s.password}
	for key, value := range s.headers {
		values = append(values, key, value, key+": "+value, key+"="+value)
	}
	if s.basicAuth {
		values = append(values, basicAuthValues(s.username, s.password)...)
	}
	return values
}

// DiagnosticSecrets is an alias retained for custom integrations that use
// the secret-oriented interface name.
func (s *HTTPStrategy) DiagnosticSecrets() []string { return s.DiagnosticValues() }

func (s *HTTPStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	matcher := s.statusMatcher
	if matcher == nil {
		matcher = func(status int) bool { return status >= 200 && status < 300 }
	}
	client := s.httpClient
	if client == nil {
		if s.tlsConfig != nil {
			client = &http.Client{
				Timeout:   3 * time.Second,
				Transport: &http.Transport{TLSClientConfig: s.tlsConfig},
			}
		} else {
			client = &http.Client{Timeout: 3 * time.Second}
		}
	}
	scheme := "http"
	if s.useTLS {
		scheme = "https"
	}

	values := s.DiagnosticValues()
	return poll(ctx, s.options, target, fmt.Sprintf("wait for HTTP %s %s", s.method, s.path), func(ctx context.Context) error {
		endpoint, err := target.Endpoint(ctx, s.port)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, s.method, scheme+"://"+endpoint+s.path, nil)
		if err != nil {
			return err
		}
		for k, v := range s.headers {
			req.Header.Set(k, v)
		}
		if s.basicAuth {
			req.SetBasicAuth(s.username, s.password)
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
	}, true, values...)
}
