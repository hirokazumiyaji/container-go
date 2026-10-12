package wait

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type capturedHTTPRequest struct {
	host   string
	path   string
	auth   string
	header string
}

func captureHTTPRequest(r *http.Request) capturedHTTPRequest {
	return capturedHTTPRequest{
		host:   r.Host,
		path:   r.URL.Path,
		auth:   r.Header.Get("Authorization"),
		header: r.Header.Get("X-Probe-Secret"),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func successfulHTTPResponse(r *http.Request) *http.Response {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}
}

func TestForHTTPPathCannotChangeAuthority(t *testing.T) {
	attackerRequests := make(chan capturedHTTPRequest, 8)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	originRequests := make(chan capturedHTTPRequest, 8)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	endpoint := strings.TrimPrefix(origin.URL, "http://")
	attackerAuthority := strings.TrimPrefix(attacker.URL, "http://")
	for _, test := range []struct {
		name     string
		path     string
		wantPath string
	}{
		{name: "at sign", path: "@" + attackerAuthority + "/health"},
		{name: "double slash", path: "//" + attackerAuthority + "/health"},
		{name: "triple slash", path: "///" + attackerAuthority + "/health"},
		{name: "absolute URL", path: "http://" + attackerAuthority + "/health", wantPath: "/http://" + attackerAuthority + "/health"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newFakeTarget()
			target.endpoint = endpoint

			err := ForHTTP(test.path).
				WithStartupTimeout(time.Second).
				WithPollInterval(20*time.Millisecond).
				WaitUntilReady(context.Background(), target)
			if err != nil {
				t.Fatalf("WaitUntilReady: %v", err)
			}

			select {
			case got := <-originRequests:
				wantPath := test.wantPath
				if wantPath == "" {
					wantPath = test.path
					if !strings.HasPrefix(wantPath, "/") {
						wantPath = "/" + wantPath
					}
				}
				if got.host != endpoint || got.path != wantPath {
					t.Errorf("request = host %q path %q, want host %q path %q", got.host, got.path, endpoint, wantPath)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("origin server did not receive the probe")
			}
			if len(attackerRequests) != 0 {
				t.Errorf("attacker server received %d request(s)", len(attackerRequests))
			}
		})
	}
}

func TestForHTTPPathReferenceSemantics(t *testing.T) {
	for _, test := range []struct {
		name                string
		path                string
		wantPath            string
		wantEscapedPath     string
		wantRawQuery        string
		wantForceQuery      bool
		wantFragment        string
		wantEscapedFragment string
	}{
		{
			name:            "query string",
			path:            "health?mode=ready&next=%2Fstatus",
			wantPath:        "/health",
			wantEscapedPath: "/health",
			wantRawQuery:    "mode=ready&next=%2Fstatus",
		},
		{
			name:            "empty query",
			path:            "/health?",
			wantPath:        "/health",
			wantEscapedPath: "/health",
			wantForceQuery:  true,
		},
		{
			name:                "fragment",
			path:                "/health#ready%2Fstate",
			wantPath:            "/health",
			wantEscapedPath:     "/health",
			wantFragment:        "ready/state",
			wantEscapedFragment: "ready%2Fstate",
		},
		{
			name:            "pre-escaped slash",
			path:            "/health%2Fready?mode=ready",
			wantPath:        "/health/ready",
			wantEscapedPath: "/health%2Fready",
			wantRawQuery:    "mode=ready",
		},
		{
			name:            "pre-escaped hash",
			path:            "/health%23ready",
			wantPath:        "/health#ready",
			wantEscapedPath: "/health%23ready",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan *http.Request, 1)
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests <- r
				return successfulHTTPResponse(r), nil
			})}
			target := newFakeTarget()
			target.endpoint = "container.test:8080"

			err := ForHTTP(test.path).
				WithHTTPClient(client).
				WithStartupTimeout(time.Second).
				WaitUntilReady(context.Background(), target)
			if err != nil {
				t.Fatalf("WaitUntilReady: %v", err)
			}

			req := <-requests
			if req.URL.Scheme != "http" || req.URL.Host != target.endpoint {
				t.Errorf("probe URL origin = %q, want http://%s", req.URL.Scheme+"://"+req.URL.Host, target.endpoint)
			}
			if req.URL.Path != test.wantPath || req.URL.EscapedPath() != test.wantEscapedPath {
				t.Errorf("probe path = path %q escaped %q, want path %q escaped %q", req.URL.Path, req.URL.EscapedPath(), test.wantPath, test.wantEscapedPath)
			}
			if req.URL.RawQuery != test.wantRawQuery || req.URL.ForceQuery != test.wantForceQuery {
				t.Errorf("probe query = raw %q force %t, want raw %q force %t", req.URL.RawQuery, req.URL.ForceQuery, test.wantRawQuery, test.wantForceQuery)
			}
			if req.URL.Fragment != test.wantFragment || req.URL.EscapedFragment() != test.wantEscapedFragment {
				t.Errorf("probe fragment = fragment %q escaped %q, want fragment %q escaped %q", req.URL.Fragment, req.URL.EscapedFragment(), test.wantFragment, test.wantEscapedFragment)
			}
		})
	}
}

func TestForHTTPDefaultClientHandlesCustomDefaultTransport(t *testing.T) {
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	originalTransport := http.DefaultTransport
	var customTransportCalled atomic.Bool
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		customTransportCalled.Store(true)
		return successfulHTTPResponse(r), nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(origin.URL, "http://")
	err := ForHTTP("/health").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if customTransportCalled.Load() {
		t.Error("readiness client used the custom default transport")
	}
	if originHits.Load() == 0 {
		t.Error("readiness request did not reach the origin")
	}
}

func TestForHTTPDefaultClientIgnoresAmbientProxy(t *testing.T) {
	const (
		childEnv  = "CONTAINERGO_TEST_HTTP_PROXY_CHILD"
		targetEnv = "CONTAINERGO_TEST_HTTP_PROXY_TARGET"
	)
	if os.Getenv(childEnv) == "1" {
		target := newFakeTarget()
		target.endpoint = os.Getenv(targetEnv)
		err := ForHTTP("/health").
			WithBasicAuth("user", "pass").
			WithHeader("X-Probe-Secret", "custom-secret").
			WithStartupTimeout(3*time.Second).
			WithPollInterval(20*time.Millisecond).
			WaitUntilReady(context.Background(), target)
		if err != nil {
			t.Fatalf("WaitUntilReady: %v", err)
		}
		return
	}

	var originHits atomic.Int32
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	if err := origin.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	origin.Listener = listener
	origin.Start()
	defer origin.Close()

	proxyRequests := make(chan capturedHTTPRequest, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	targetEndpoint := net.JoinHostPort(nonLoopbackIPv4(t), strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	t.Setenv("no_proxy", "*")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestForHTTPDefaultClientIgnoresAmbientProxy$", "-test.count=1")
	cmd.Env = environmentWithOverrides(map[string]string{
		childEnv:         "1",
		targetEnv:        targetEndpoint,
		"HTTP_PROXY":     proxy.URL,
		"HTTPS_PROXY":    proxy.URL,
		"NO_PROXY":       "",
		"REQUEST_METHOD": "",
	})
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy probe subprocess: %v\n%s", err, output)
	}
	if len(proxyRequests) != 0 {
		got := <-proxyRequests
		t.Errorf("ambient proxy received probe to %q with Authorization %q and X-Probe-Secret %q", got.host, got.auth, got.header)
	}
	if originHits.Load() == 0 {
		t.Error("probe did not reach the origin directly")
	}
}

func TestForHTTPDefaultClientBlocksCrossOriginRedirect(t *testing.T) {
	attackerRequests := make(chan capturedHTTPRequest, 2)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		http.Redirect(w, r, attacker.URL+"/ready", http.StatusFound)
	}))
	defer origin.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(origin.URL, "http://")
	err := ForHTTP("/start").
		WithHeader("X-Probe-Secret", "custom-secret").
		WithBasicAuth("user", "pass").
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(20*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Error("default client followed a cross-origin redirect")
	}
	if originHits.Load() == 0 {
		t.Error("origin server did not receive the probe")
	}
	if len(attackerRequests) != 0 {
		got := <-attackerRequests
		t.Errorf("redirect target received Authorization %q and X-Probe-Secret %q", got.auth, got.header)
	}
}

func TestSameOrigin(t *testing.T) {
	for _, test := range []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "same origin", a: "http://example.test/start", b: "http://example.test/ready", want: true},
		{name: "case-insensitive host", a: "http://EXAMPLE.test/start", b: "http://example.TEST/ready", want: true},
		{name: "same Unicode host", a: "http://ς.example:8080/start", b: "http://ς.example:8080/ready", want: true},
		{name: "IDNA-distinct Unicode hosts", a: "http://ς.example:8080/start", b: "http://Σ.example:8080/ready", want: false},
		{name: "same IPv6 zone", a: "http://[fe80::1%25eth0]:8080/start", b: "http://[fe80::1%25eth0]:8080/ready", want: true},
		{name: "case-distinct IPv6 zones", a: "http://[fe80::1%25eth0]:8080/start", b: "http://[fe80::1%25ETH0]:8080/ready", want: false},
		{name: "implicit HTTP port", a: "http://example.test/start", b: "http://example.test:80/ready", want: true},
		{name: "implicit HTTPS port", a: "https://example.test/start", b: "https://example.test:443/ready", want: true},
		{name: "different host", a: "http://example.test/start", b: "http://attacker.test/ready", want: false},
		{name: "different port", a: "http://example.test/start", b: "http://example.test:8080/ready", want: false},
		{name: "different scheme", a: "http://example.test/start", b: "https://example.test/ready", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, err := url.Parse(test.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := url.Parse(test.b)
			if err != nil {
				t.Fatal(err)
			}
			if got := sameOrigin(a, b); got != test.want {
				t.Errorf("sameOrigin(%q, %q) = %t, want %t", test.a, test.b, got, test.want)
			}
		})
	}
}

func TestForHTTPRedirectRejectsDistinctDialingAuthorities(t *testing.T) {
	for _, test := range []struct {
		name     string
		initial  string
		location string
	}{
		{
			name:     "IDNA-distinct Unicode hosts",
			initial:  "http://ς.example:8080/start",
			location: "http://Σ.example:8080/ready",
		},
		{
			name:     "case-distinct IPv6 zones",
			initial:  "http://[fe80::1%25eth0]:8080/start",
			location: "http://[fe80::1%25ETH0]:8080/ready",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			leaked := make(chan string, 1)
			client := &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if requests.Add(1) == 1 {
						return &http.Response{
							StatusCode: http.StatusFound,
							Status:     "302 Found",
							Header:     http.Header{"Location": []string{test.location}},
							Body:       io.NopCloser(strings.NewReader("")),
							Request:    req,
						}, nil
					}
					leaked <- req.Header.Get("X-Probe-Secret")
					return successfulHTTPResponse(req), nil
				}),
				CheckRedirect: checkHTTPProbeRedirect,
			}

			req, err := http.NewRequest(http.MethodGet, test.initial, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Probe-Secret", "custom-secret")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
			select {
			case got := <-leaked:
				t.Errorf("redirect target received X-Probe-Secret %q", got)
			default:
			}
		})
	}
}

func TestForHTTPDefaultClientAllowsSameOriginRedirect(t *testing.T) {
	readyRequests := make(chan capturedHTTPRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/ready", http.StatusFound)
			return
		}
		readyRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")
	err := ForHTTP("/start").
		WithHeader("X-Probe-Secret", "custom-secret").
		WithBasicAuth("user", "pass").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	select {
	case got := <-readyRequests:
		if got.auth == "" || got.header != "custom-secret" {
			t.Errorf("same-origin redirect received Authorization %q and X-Probe-Secret %q", got.auth, got.header)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("same-origin redirect was not followed")
	}
}

func TestForHTTPCustomClientPreservesRedirectPolicy(t *testing.T) {
	attackerRequests := make(chan capturedHTTPRequest, 1)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/ready", http.StatusFound)
	}))
	defer origin.Close()

	var redirects atomic.Int32
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		redirects.Add(1)
		return nil
	}}
	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(origin.URL, "http://")
	err := ForHTTP("/start").
		WithHeader("X-Probe-Secret", "custom-secret").
		WithBasicAuth("user", "pass").
		WithHTTPClient(client).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if redirects.Load() != 1 {
		t.Errorf("custom CheckRedirect calls = %d, want 1", redirects.Load())
	}
	got := <-attackerRequests
	if got.auth == "" || got.header != "custom-secret" {
		t.Errorf("redirect target received Authorization %q and X-Probe-Secret %q", got.auth, got.header)
	}
}

func TestForHTTPCustomClientPreservesProxyPolicy(t *testing.T) {
	proxyRequests := make(chan capturedHTTPRequest, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	defer client.CloseIdleConnections()
	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(origin.URL, "http://")
	err = ForHTTP("/health").
		WithHeader("X-Probe-Secret", "custom-secret").
		WithBasicAuth("user", "pass").
		WithHTTPClient(client).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	got := <-proxyRequests
	if got.auth == "" || got.header != "custom-secret" {
		t.Errorf("proxy received Authorization %q and X-Probe-Secret %q", got.auth, got.header)
	}
	if originHits.Load() != 0 {
		t.Errorf("origin received %d direct request(s), want 0", originHits.Load())
	}
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		var ip net.IP
		switch value := address.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip4 := ip.To4(); ip4 != nil && ip4.IsGlobalUnicast() && !ip4.IsLoopback() {
			return ip4.String()
		}
	}
	t.Skip("no non-loopback IPv4 address available")
	return ""
}

func environmentWithOverrides(overrides map[string]string) []string {
	normalized := make(map[string]string, len(overrides))
	for key, value := range overrides {
		normalized[strings.ToUpper(key)] = value
	}
	environment := make([]string, 0, len(os.Environ())+len(normalized))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := normalized[strings.ToUpper(key)]; !overridden {
			environment = append(environment, entry)
		}
	}
	for key, value := range normalized {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func TestEnvironmentWithOverridesRemovesCaseInsensitiveKeys(t *testing.T) {
	t.Setenv("no_proxy", "inherited")
	t.Setenv("http_proxy", "http://inherited.invalid")

	environment := environmentWithOverrides(map[string]string{
		"NO_PROXY":   "",
		"HTTP_PROXY": "http://replacement.invalid",
	})
	counts := map[string]int{}
	values := map[string]string{}
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		counts[key]++
		values[key] = value
	}
	for _, key := range []string{"NO_PROXY", "HTTP_PROXY"} {
		if counts[key] != 1 {
			t.Errorf("%s entries = %d, want 1: %q", key, counts[key], environment)
		}
	}
	if values["NO_PROXY"] != "" || values["HTTP_PROXY"] != "http://replacement.invalid" {
		t.Errorf("overridden values = NO_PROXY %q HTTP_PROXY %q", values["NO_PROXY"], values["HTTP_PROXY"])
	}
}
