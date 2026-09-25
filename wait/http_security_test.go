package wait

import (
	"context"
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

func TestForHTTPPathCannotChangeAuthority(t *testing.T) {
	attackerRequests := make(chan capturedHTTPRequest, 2)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	originRequests := make(chan capturedHTTPRequest, 2)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests <- captureHTTPRequest(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	endpoint := strings.TrimPrefix(origin.URL, "http://")
	attackerAuthority := strings.TrimPrefix(attacker.URL, "http://")
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "at sign", path: "@" + attackerAuthority + "/health"},
		{name: "double slash", path: "//" + attackerAuthority + "/health"},
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
				wantPath := test.path
				if !strings.HasPrefix(wantPath, "/") {
					wantPath = "/" + wantPath
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
		t.Errorf("ambient proxy received probe to %q with Authorization %q", got.host, got.auth)
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

func TestForHTTPDefaultClientAllowsSameOriginRedirect(t *testing.T) {
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
	if err := ForHTTP("/start").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if readyHits.Load() == 0 {
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
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[strings.ToUpper(key)]; !overridden {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}
