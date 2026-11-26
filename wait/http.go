package wait

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// maxDrainBytes bounds how much of a probe response body is read to
// reach EOF. A readiness response is a status page, so a body past
// this size is not something the wait needs; capping it keeps a
// misbehaving endpoint from turning the drain into a memory cost.
const maxDrainBytes = 64 * 1024

// defaultHTTPRequestTimeout bounds one request made by the built-in client.
// WithRequestTimeout overrides it.
const defaultHTTPRequestTimeout = 3 * time.Second

// HTTPStrategy waits until an HTTP request against the container
// returns an acceptable status code.
type HTTPStrategy struct {
	options
	path               string
	port               string
	portSet            bool
	method             string
	statusMatcher      func(int) bool
	statusCode         int
	statusCodeSet      bool
	responseMatcher    func(io.Reader) bool
	headers            map[string]string
	body               []byte
	bodySet            bool
	username           string
	password           string
	basicAuth          bool
	useTLS             bool
	tlsConfig          *tls.Config
	insecureTLS        bool
	httpClient         *http.Client
	followRedirects    bool
	followRedirectsSet bool
	requestTimeout     time.Duration
	requestTimeoutSet  bool
}

// ForHTTP waits for a plain-HTTP endpoint at path (on the first declared
// TCP port unless WithPort is used) to return 2xx.
func ForHTTP(path string) *HTTPStrategy {
	return &HTTPStrategy{path: path, method: http.MethodGet}
}

// clone returns a strategy with an independent header map. Builders derive a
// new value from the receiver so a strategy shared by parallel callers cannot
// be reconfigured through another caller's options.
func (s *HTTPStrategy) clone() *HTTPStrategy {
	cloned := *s
	cloned.headers = maps.Clone(s.headers)
	return &cloned
}

// WithPort probes a specific declared port instead of the first one. An
// empty value is invalid; omit WithPort to select the first declared TCP
// port.
func (s *HTTPStrategy) WithPort(port string) *HTTPStrategy {
	cloned := s.clone()
	cloned.port = port
	cloned.portSet = true
	return cloned
}

// WithMethod sets the HTTP method to use.
func (s *HTTPStrategy) WithMethod(method string) *HTTPStrategy {
	cloned := s.clone()
	cloned.method = method
	return cloned
}

// WithStatusCodeMatcher replaces the default 2xx acceptance check. It also
// clears a fixed status code set by WithStatusCode.
func (s *HTTPStrategy) WithStatusCodeMatcher(matcher func(status int) bool) *HTTPStrategy {
	cloned := s.clone()
	cloned.statusMatcher = matcher
	cloned.statusCode, cloned.statusCodeSet = 0, false
	return cloned
}

// WithStatusCode accepts only the given HTTP status code instead of the
// default 2xx range. It replaces both the default and a matcher configured by
// WithStatusCodeMatcher. Codes outside 100-599 are rejected with a
// *ConfigError before probing.
func (s *HTTPStrategy) WithStatusCode(code int) *HTTPStrategy {
	cloned := s.clone()
	cloned.statusCode, cloned.statusCodeSet = code, true
	cloned.statusMatcher = nil
	return cloned
}

// WithBody sends body with every readiness request. The body is cloned, so a
// later caller mutation cannot diverge the transmitted payload from the value
// registered for diagnostic redaction. Set a Content-Type with WithHeader when
// the endpoint requires one. A body too large to protect in streamed
// diagnostics is rejected as a *ConfigError.
func (s *HTTPStrategy) WithBody(body []byte) *HTTPStrategy {
	cloned := s.clone()
	cloned.body, cloned.bodySet = bytes.Clone(body), true
	return cloned
}

// WithResponseMatcher accepts an HTTP response only when matcher reports
// true for its body. The matcher sees at most maxDrainBytes of the body and
// runs only after the status matcher accepts. A false result is a retryable
// failed check. Without a matcher an accepted status ends the wait without
// reading the body.
func (s *HTTPStrategy) WithResponseMatcher(matcher func(io.Reader) bool) *HTTPStrategy {
	cloned := s.clone()
	cloned.responseMatcher = matcher
	return cloned
}

// WithHeaders adds request headers to the readiness probe.
func (s *HTTPStrategy) WithHeaders(headers map[string]string) *HTTPStrategy {
	cloned := s.clone()
	if len(headers) > 0 && cloned.headers == nil {
		cloned.headers = make(map[string]string, len(headers))
	}
	for k, v := range headers {
		cloned.headers[k] = v
	}
	return cloned
}

// WithHeader adds one request header to the readiness probe.
func (s *HTTPStrategy) WithHeader(key, value string) *HTTPStrategy {
	return s.WithHeaders(map[string]string{key: value})
}

// WithBasicAuth sends HTTP basic auth with the readiness probe.
func (s *HTTPStrategy) WithBasicAuth(username, password string) *HTTPStrategy {
	cloned := s.clone()
	cloned.username, cloned.password, cloned.basicAuth = username, password, true
	return cloned
}

// WithTLS probes via https with the default transport.
func (s *HTTPStrategy) WithTLS() *HTTPStrategy {
	cloned := s.clone()
	cloned.useTLS = true
	return cloned
}

// WithTLSConfig probes via https with a custom TLS config. The config is
// cloned before use, so the caller's value is never mutated. A self-signed
// test certificate is better handled with WithAllowInsecureTLS.
func (s *HTTPStrategy) WithTLSConfig(cfg *tls.Config) *HTTPStrategy {
	cloned := s.clone()
	cloned.useTLS = true
	cloned.tlsConfig = cfg
	return cloned
}

// WithAllowInsecureTLS probes via https without verifying the server
// certificate. It is for tests only: it sets TLSClientConfig.InsecureSkipVerify,
// which disables certificate verification and leaves the probe open to a
// man-in-the-middle. The caller's WithTLSConfig value is never mutated; the
// built-in client clones it before applying the override.
//
// A caller-supplied WithHTTPClient supplies its own transport and TLS
// config, so this option only selects the https scheme for that client.
func (s *HTTPStrategy) WithAllowInsecureTLS() *HTTPStrategy {
	cloned := s.clone()
	cloned.useTLS = true
	cloned.insecureTLS = true
	return cloned
}

// WithFollowRedirects controls whether the readiness request follows HTTP
// redirects. false installs a CheckRedirect that returns
// http.ErrUseLastResponse, so the redirect response itself is validated by the
// status and body matchers. true restores the default policy: the built-in
// client follows same-origin redirects up to 10, while a caller-supplied
// WithHTTPClient keeps its own policy.
//
// When this option is unset, a caller-supplied client's redirect policy is
// left untouched.
func (s *HTTPStrategy) WithFollowRedirects(follow bool) *HTTPStrategy {
	cloned := s.clone()
	cloned.followRedirects, cloned.followRedirectsSet = follow, true
	return cloned
}

// WithRequestTimeout bounds one HTTP request independently of
// WithStartupTimeout, so a hung request cannot consume the whole startup
// budget. A non-positive duration is rejected as a *ConfigError before
// probing. When unset, the built-in client keeps its 3s per-request timeout
// and a caller-supplied WithHTTPClient keeps its own Timeout.
func (s *HTTPStrategy) WithRequestTimeout(d time.Duration) *HTTPStrategy {
	cloned := s.clone()
	cloned.requestTimeout, cloned.requestTimeoutSet = d, true
	return cloned
}

// WithHTTPClient delegates transport, proxy, redirect, and timeout
// policy to the caller. WithTLS/WithTLSConfig still select the https
// scheme; the custom client supplies the TLS config (for example
// httptest.NewTLSServer). WithRequestTimeout and WithFollowRedirects(false)
// clone the client before overriding its Timeout or CheckRedirect, so the
// caller's value is never mutated.
func (s *HTTPStrategy) WithHTTPClient(c *http.Client) *HTTPStrategy {
	cloned := s.clone()
	cloned.httpClient = c
	return cloned
}

func (s *HTTPStrategy) WithStartupTimeout(d time.Duration) *HTTPStrategy {
	cloned := s.clone()
	cloned.startupTimeout = d
	return cloned
}

func (s *HTTPStrategy) WithPollInterval(d time.Duration) *HTTPStrategy {
	cloned := s.clone()
	cloned.pollInterval = d
	return cloned
}

// DiagnosticValues returns the caller-provided secrets that can appear in a
// wait failure. Request metadata (method, path, port, and header names) is
// intentionally excluded: it is useful context, not a secret, and registering
// it would mask ordinary words such as GET in every diagnostic. Values of
// secret-named query parameters are included because a secret can be carried
// in the URL; other query values are boundary values.
func (s *HTTPStrategy) DiagnosticValues() []string {
	var values []string
	if s.bodySet {
		values = append(values, string(s.body))
	}
	if s.username != "" {
		values = append(values, s.username)
	}
	if s.password != "" {
		values = append(values, s.password)
	}
	for key, value := range s.headers {
		values = append(values, value)
		if isCookieHeader(key) {
			values = append(values, cookieDiagnosticValues(value)...)
		}
	}
	secrets, _ := queryDiagnosticValues(s.path)
	values = append(values, secrets...)
	if s.basicAuth {
		values = append(values, basicAuthValues(s.username, s.password)...)
	}
	return values
}

func (s *HTTPStrategy) diagnosticBoundaryValues() []string {
	_, boundary := queryDiagnosticValues(s.path)
	return boundary
}

func isCookieHeader(key string) bool {
	return strings.EqualFold(key, "cookie") || strings.EqualFold(key, "set-cookie") || strings.EqualFold(key, "cookie2")
}

// queryDiagnosticValues splits the query by hand rather than with
// url.ParseQuery, which drops a pair containing ';' or a bad escape and would
// leave its value unregistered. Each value is returned raw and, when it
// decodes, unescaped. Values of secret-named parameters are secrets; other
// values are boundary values unless they are too short or common to matter.
func queryDiagnosticValues(rawPath string) (secrets, boundary []string) {
	_, rawQuery, ok := strings.Cut(rawPath, "?")
	if !ok {
		return nil, nil
	}
	rawQuery, _, _ = strings.Cut(rawQuery, "#")
	for _, pair := range strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		key, value, _ := strings.Cut(pair, "=")
		if value == "" {
			continue
		}
		forms := []string{value}
		if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
			forms = append(forms, decoded)
		}
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		if cli.IsSecretName(decodedKey) {
			secrets = append(secrets, forms...)
			continue
		}
		for _, form := range forms {
			if len(form) >= 4 && !strings.EqualFold(form, "true") && !strings.EqualFold(form, "false") {
				boundary = append(boundary, form)
			}
		}
	}
	return secrets, boundary
}

func (s *HTTPStrategy) validate() error {
	if err := s.options.validate("ForHTTP"); err != nil {
		return err
	}
	if s.portSet {
		if err := validateTCPPortSpec("ForHTTP", s.port); err != nil {
			return err
		}
	}
	if s.bodySet && (len(s.body) > cli.MaxStreamOverlap || !cli.StreamValueFits(string(s.body))) {
		return invalidConfigf("ForHTTP", "body", "", "is too large to redact safely from streamed diagnostics")
	}
	if s.statusCodeSet && (s.statusCode < 100 || s.statusCode > 599) {
		return invalidConfigf("ForHTTP", "status code", strconv.Itoa(s.statusCode), "must be between 100 and 599")
	}
	if s.method == "" {
		return invalidConfigf("ForHTTP", "method", "", "must not be empty")
	}
	if s.requestTimeoutSet && s.requestTimeout <= 0 {
		return invalidConfigf("ForHTTP", "request timeout", s.requestTimeout.String(), "must be positive")
	}
	if s.path != "" &&
		!strings.HasPrefix(s.path, "/") &&
		!strings.HasPrefix(s.path, "@") &&
		!strings.HasPrefix(s.path, "http://") &&
		!strings.HasPrefix(s.path, "https://") &&
		!strings.ContainsAny(s.path, "?#") {
		return invalidConfigf("ForHTTP", "path", s.path, "path must start with /")
	}
	for i := 0; i < len(s.path); i++ {
		if s.path[i] <= ' ' || s.path[i] == 0x7f {
			return invalidConfigf("ForHTTP", "path", s.path, "path contains a control or space")
		}
	}
	probeURL, err := buildHTTPProbeURL("http", "wait.invalid:80", s.path)
	if err != nil {
		return invalidConfigf("ForHTTP", "path", s.path, "%v", err)
	}
	if _, err := http.NewRequestWithContext(context.Background(), s.method, probeURL, nil); err != nil {
		return invalidConfigf("ForHTTP", "method or path", "", "%v", err)
	}
	for key, value := range s.headers {
		if err := validateHTTPHeader("ForHTTP", key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *HTTPStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	defer func() { retErr = safeStrategyError(retErr, s) }()
	if err := s.validate(); err != nil {
		return err
	}

	matcher := s.statusMatcher
	if matcher == nil {
		if s.statusCodeSet {
			status := s.statusCode
			matcher = func(candidate int) bool { return candidate == status }
		} else {
			matcher = func(status int) bool { return status >= 200 && status < 300 }
		}
	}
	client := s.httpClient
	if client == nil {
		client = newDefaultHTTPClient(s)
		defer client.CloseIdleConnections()
	} else if s.requestTimeoutSet || (s.followRedirectsSet && !s.followRedirects) {
		// Override on a shallow copy so the caller's client and its
		// transport stay untouched.
		overridden := *client
		if s.requestTimeoutSet {
			overridden.Timeout = s.requestTimeout
		}
		if s.followRedirectsSet && !s.followRedirects {
			overridden.CheckRedirect = stopHTTPProbeRedirect
		}
		client = &overridden
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
			return fatalCheckError{err: invalidConfigf("ForHTTP", "request", "", "%v", err)}
		}
		var requestBody io.Reader
		if s.bodySet {
			requestBody = bytes.NewReader(s.body)
		}
		req, err := http.NewRequestWithContext(ctx, s.method, probeURL, requestBody)
		if err != nil {
			return fatalCheckError{err: invalidConfigf("ForHTTP", "request", "", "%v", err)}
		}
		for k, v := range s.headers {
			// net/http ignores a "Host" entry in req.Header and derives
			// the wire header from req.Host, so a virtual-host probe
			// has to set that field directly.
			if strings.EqualFold(k, "Host") {
				req.Host = v
				continue
			}
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
		// Check status before draining. An accepted response ends the
		// wait, so a stalled body must not delay success past the
		// startup deadline unless a response matcher has to read it.
		// Only rejected responses need another poll iteration, and
		// those need a drained body so net/http can return the
		// connection to the idle pool instead of paying a fresh TCP
		// (and, under WithTLSConfig, a fresh handshake) on every retry.
		// The drain is bounded because readiness bodies are small and
		// an endpoint that streams an unbounded body must not be able
		// to grow the wait's memory usage. A drain error is not the
		// probe's verdict: the response already arrived, so the status
		// decides the outcome either way.
		if !matcher(resp.StatusCode) {
			_, _ = io.CopyN(io.Discard, resp.Body, maxDrainBytes)
			return fmt.Errorf("status %d not accepted", resp.StatusCode)
		}
		if s.responseMatcher == nil {
			return nil
		}
		body := io.LimitReader(resp.Body, maxDrainBytes)
		if !s.responseMatcher(body) {
			_, _ = io.Copy(io.Discard, body)
			return fmt.Errorf("status %d accepted but response body rejected", resp.StatusCode)
		}
		return nil
	})
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

func newDefaultHTTPClient(s *HTTPStrategy) *http.Client {
	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && defaultTransport != nil {
		transport = defaultTransport.Clone()
	}
	transport.Proxy = nil
	if tlsConfig := s.probeTLSConfig(); tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}
	requestTimeout := defaultHTTPRequestTimeout
	if s.requestTimeoutSet {
		requestTimeout = s.requestTimeout
	}
	checkRedirect := checkHTTPProbeRedirect
	if s.followRedirectsSet && !s.followRedirects {
		checkRedirect = stopHTTPProbeRedirect
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect,
		Timeout:       requestTimeout,
	}
}

// probeTLSConfig returns the TLS config for the built-in transport. The
// caller's WithTLSConfig value is cloned before the test-only
// InsecureSkipVerify override is applied.
func (s *HTTPStrategy) probeTLSConfig() *tls.Config {
	if s.tlsConfig == nil {
		if !s.insecureTLS {
			return nil
		}
		return &tls.Config{InsecureSkipVerify: true}
	}
	tlsConfig := s.tlsConfig.Clone()
	if s.insecureTLS {
		tlsConfig.InsecureSkipVerify = true
	}
	return tlsConfig
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

// stopHTTPProbeRedirect makes net/http return the redirect response instead of
// following it, so the status and body matchers validate that response.
func stopHTTPProbeRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
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
