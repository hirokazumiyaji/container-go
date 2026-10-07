package wait

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// HTTPStrategy waits until an HTTP request against the container
// returns an acceptable status code.
type HTTPStrategy struct {
	options
	path          string
	port          string
	portSet       bool
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

// ForHTTP waits for a plain-HTTP endpoint at path (on the first declared
// TCP port unless WithPort is used) to return 2xx.
func ForHTTP(path string) *HTTPStrategy {
	return &HTTPStrategy{path: path, method: http.MethodGet}
}

// WithPort probes a specific declared port instead of the first one. An
// empty value is invalid; omit WithPort to select the first declared TCP
// port.
func (s *HTTPStrategy) WithPort(port string) *HTTPStrategy {
	s.port = port
	s.portSet = true
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

// WithHTTPClient delegates transport, proxy, redirect, and timeout
// policy to the caller. WithTLS/WithTLSConfig still select the https
// scheme; the custom client supplies the TLS config (for example
// httptest.NewTLSServer).
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

func (s *HTTPStrategy) validate() error {
	if err := s.options.validate(); err != nil {
		return err
	}
	if s.portSet {
		if err := validateTCPPortSpec("ForHTTP", s.port); err != nil {
			return err
		}
	}
	if s.method == "" {
		return invalidConfigf("HTTP method must not be empty")
	}
	if s.path != "" &&
		!strings.HasPrefix(s.path, "/") &&
		!strings.HasPrefix(s.path, "@") &&
		!strings.HasPrefix(s.path, "http://") &&
		!strings.HasPrefix(s.path, "https://") &&
		!strings.ContainsAny(s.path, "?#") {
		return invalidConfigf("invalid HTTP path %q: path must start with /", s.path)
	}
	for i := 0; i < len(s.path); i++ {
		if s.path[i] <= ' ' || s.path[i] == 0x7f {
			return invalidConfigf("invalid HTTP path %q: path contains a control or space", s.path)
		}
	}
	probeURL, err := buildHTTPProbeURL("http", "wait.invalid:80", s.path)
	if err != nil {
		return invalidConfigf("invalid HTTP path %q: %v", s.path, err)
	}
	if _, err := http.NewRequestWithContext(context.Background(), s.method, probeURL, nil); err != nil {
		return invalidConfigf("invalid HTTP method or path: %v", err)
	}
	for key, value := range s.headers {
		if err := validateHTTPHeader(key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *HTTPStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := s.validate(); err != nil {
		return err
	}

	matcher := s.statusMatcher
	if matcher == nil {
		matcher = func(status int) bool { return status >= 200 && status < 300 }
	}
	client := s.httpClient
	if client == nil {
		client = newDefaultHTTPClient(s.tlsConfig)
		defer client.CloseIdleConnections()
	}
	scheme := "http"
	if s.useTLS {
		scheme = "https"
	}

	return poll(ctx, s.options, target, fmt.Sprintf("wait for HTTP %s %s", s.method, s.path), func(ctx context.Context) error {
		endpoint, err := target.Endpoint(ctx, s.port)
		if err != nil {
			return err
		}
		probeURL, err := buildHTTPProbeURL(scheme, endpoint, s.path)
		if err != nil {
			return fatalCheckError{err: invalidConfigf("invalid HTTP request: %v", err)}
		}
		req, err := http.NewRequestWithContext(ctx, s.method, probeURL, nil)
		if err != nil {
			return fatalCheckError{err: invalidConfigf("invalid HTTP request: %v", err)}
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
	}, true)
}

func buildHTTPProbeURL(scheme, endpoint, callerPath string) (string, error) {
	if _, err := url.Parse(scheme + "://" + endpoint + "/"); err != nil {
		return "", err
	}
	pathRef, err := url.Parse(callerPath)
	if err == nil && pathRef.Scheme == "" && pathRef.Opaque == "" && pathRef.Host == "" && pathRef.User == nil && !strings.HasPrefix(callerPath, "//") {
		return (&url.URL{
			Scheme:      scheme,
			Host:        endpoint,
			Path:        pathRef.Path,
			RawPath:     pathRef.RawPath,
			ForceQuery:  pathRef.ForceQuery,
			RawQuery:    pathRef.RawQuery,
			Fragment:    pathRef.Fragment,
			RawFragment: pathRef.RawFragment,
		}).String(), nil
	}

	// Keep authority-looking legacy paths literal while still parsing their
	// query, fragment, and pre-escaped path components.
	literalPath, literalErr := url.Parse("/." + callerPath)
	if literalErr != nil {
		return "", literalErr
	}
	literalPath.Path = strings.TrimPrefix(literalPath.Path, "/.")
	literalPath.RawPath = strings.TrimPrefix(literalPath.RawPath, "/.")
	return (&url.URL{
		Scheme:      scheme,
		Host:        endpoint,
		Path:        literalPath.Path,
		RawPath:     literalPath.RawPath,
		ForceQuery:  literalPath.ForceQuery,
		RawQuery:    literalPath.RawQuery,
		Fragment:    literalPath.Fragment,
		RawFragment: literalPath.RawFragment,
	}).String(), nil
}

func newDefaultHTTPClient(tlsConfig *tls.Config) *http.Client {
	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && defaultTransport != nil {
		transport = defaultTransport.Clone()
	}
	transport.Proxy = nil
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkHTTPProbeRedirect,
		Timeout:       3 * time.Second,
	}
}

func checkHTTPProbeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !sameOrigin(via[0].URL, req.URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		sameOriginHost(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func sameOriginHost(a, b string) bool {
	if a == b {
		return true
	}

	// net/http canonicalizes non-ASCII hostnames with IDNA before dialing,
	// while net/url leaves them as Unicode. Unicode case folding is not an
	// IDNA equivalence check (for example, final sigma and capital sigma),
	// so fail closed instead of treating distinct dialing authorities as
	// equal. Exact raw host matches above remain valid.
	if !isASCII(a) || !isASCII(b) {
		return false
	}

	// IPv6 zone identifiers are part of the dialing authority. netip.Addr
	// equality compares the address and preserves the zone exactly, unlike
	// strings.EqualFold.
	if addr, err := netip.ParseAddr(a); err == nil {
		other, err := netip.ParseAddr(b)
		return err == nil && addr == other
	}

	// DNS names are ASCII case-insensitive.
	return strings.EqualFold(a, b)
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
