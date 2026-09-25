package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// rotatingDockerInspectRunner answers inspect with a fresh host port or IP
// on every call, like a container that was restarted after the handle was
// created.
type rotatingDockerInspectRunner struct {
	*fakeRunner
	uid         string
	hostPorts   []string
	hostAddress string
	ips         []string
	mu          sync.Mutex
	inspects    int
}

func (r *rotatingDockerInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	r.mu.Lock()
	r.inspects++
	index := r.inspects - 1
	r.calls = append(r.calls, args)
	r.mu.Unlock()

	port := ""
	if len(r.hostPorts) > 0 {
		port = r.hostPorts[index%len(r.hostPorts)]
	}
	ip := ""
	if len(r.ips) > 0 {
		ip = r.ips[index%len(r.ips)]
	}
	ports := "{}"
	if port != "" {
		ports = fmt.Sprintf(`{"6379/tcp": [{"HostIp": %q, "HostPort": %q}]}`, r.hostAddress, port)
	}
	return []byte(fmt.Sprintf(`[{
    "Id": %q,
    "State": {"Status": "running"},
    "Config": {"Image": "redis:7-alpine", "Labels": {}},
    "NetworkSettings": {"IPAddress": %q, "Ports": %s}
  }]`, r.uid, ip, ports)), nil, nil
}

func (r *rotatingDockerInspectRunner) inspectCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inspects
}

func TestEndpointUsesFreshInspectResult(t *testing.T) {
	// A published-port answer comes from inspect, so it must not be
	// served from the snapshot taken when the handle was created.
	uid := strings.Repeat("a", 64)
	ports := []string{"32768", "32769", "32770"}
	f := &rotatingDockerInspectRunner{
		fakeRunner:  newTestRunner(),
		uid:         uid,
		hostPorts:   ports,
		hostAddress: "127.0.0.1",
	}
	ctr := &Container{
		id:      "myctr",
		runner:  f,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}
	ctr.rememberImmutableID(uid)

	if _, err := ctr.Endpoint(context.Background(), "6379"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	got, err := ctr.Endpoint(context.Background(), "6379")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if want := "127.0.0.1:" + ports[1]; got != want {
		t.Errorf("Endpoint = %q, want the newest inspect value %q", got, want)
	}
	if f.inspectCalls() < 2 {
		t.Errorf("inspect calls = %d, want a fresh inspect per lookup", f.inspectCalls())
	}
}

func TestContainerIPUsesFreshInspectResult(t *testing.T) {
	uid := strings.Repeat("b", 64)
	f := &rotatingDockerInspectRunner{
		fakeRunner: newTestRunner(),
		uid:        uid,
		ips:        []string{"172.17.0.2", "172.17.0.9"},
	}
	ctr := &Container{
		id:      "myctr",
		runner:  f,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 6379, proto: "tcp"}},
	}
	ctr.rememberImmutableID(uid)

	if _, err := ctr.ContainerIP(context.Background()); err != nil {
		t.Fatalf("ContainerIP: %v", err)
	}
	got, err := ctr.ContainerIP(context.Background())
	if err != nil {
		t.Fatalf("ContainerIP: %v", err)
	}
	if got != "172.17.0.9" {
		t.Errorf("ContainerIP = %q, want the newest inspect value 172.17.0.9", got)
	}
}

// replacedIdentityRunner answers the handle's immutable ID with a
// different container, the way a stale target would.
type replacedIdentityRunner struct {
	*fakeRunner
}

func (r *replacedIdentityRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] != "inspect" {
		return r.fakeRunner.Run(ctx, args...)
	}
	return []byte(fmt.Sprintf(`[{
    "Id": %q,
    "State": {"Status": "running"},
    "Config": {"Image": "redis:7-alpine", "Labels": {}},
    "NetworkSettings": {"IPAddress": "172.17.0.2", "Ports": {"6379/tcp": [{"HostIp": "127.0.0.1", "HostPort": "32768"}]}}
  }]`, strings.Repeat("d", 64))), nil, nil
}

func TestHostAndEndpointRefuseReplacedIdentity(t *testing.T) {
	// A published-port host is not derived from inspect, so the handle's
	// current identity still has to be verified before handing one out.
	published := []publishSpec{{hostAddr: "127.0.0.1", hostPort: 32768, containerPort: 6379, proto: "tcp"}}
	for _, tc := range []struct {
		name string
		call func(*Container) error
	}{
		{
			name: "Host",
			call: func(c *Container) error {
				_, err := c.Host(context.Background())
				return err
			},
		},
		{
			name: "Endpoint",
			call: func(c *Container) error {
				_, err := c.Endpoint(context.Background(), "6379")
				return err
			},
		},
		{
			name: "ContainerIP",
			call: func(c *Container) error {
				_, err := c.ContainerIP(context.Background())
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctr := &Container{
				id:        "myctr",
				runner:    &replacedIdentityRunner{fakeRunner: newTestRunner()},
				eng:       dockerEngine{},
				published: published,
				exposed:   []portSpec{{port: 6379, proto: "tcp"}},
				creation:  "0123456789abcdef",
			}
			ctr.rememberImmutableID(strings.Repeat("c", 64))
			if err := tc.call(ctr); !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("%s = %v, want ErrGenerationReplaced for a replaced identity", tc.name, err)
			}
		})
	}
}

// erroringStream returns a fixed read error so a test can observe the
// terminal-read path.
type erroringStream struct {
	closed bool
}

func (s *erroringStream) Read([]byte) (int, error) { return 0, errors.New("stream broke") }

func (s *erroringStream) Close() error {
	s.closed = true
	return nil
}

// generationStreamRunner answers inspect with a matching managed
// generation and serves one canned log stream.
type generationStreamRunner struct {
	*fakeRunner
	name   string
	stream io.ReadCloser
	closed bool
}

func (g *generationStreamRunner) Stream(_ context.Context, _ ...string) (io.ReadCloser, error) {
	body := g.stream
	if body == nil {
		body = io.NopCloser(strings.NewReader("streamed\n"))
	}
	return &trackedStream{ReadCloser: body, closed: func() { g.closed = true }}, nil
}

func (g *generationStreamRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		g.mu.Lock()
		g.calls = append(g.calls, args)
		g.mu.Unlock()
		return []byte(reuseInspectJSONWithCreation(g.name, "running", "redis:7-alpine", "0123456789abcdef")), nil, nil
	}
	return g.fakeRunner.Run(ctx, args...)
}

type trackedStream struct {
	io.ReadCloser
	closed func()
}

func (s *trackedStream) Close() error {
	s.closed()
	return s.ReadCloser.Close()
}

func TestFollowLogsReleasesAppleLockOnReadEOF(t *testing.T) {
	// Draining the stream to EOF is terminal for the log source, so the
	// name lock must be free without a separate Close call.
	name := "eof-" + newContainerName()
	g := &generationStreamRunner{fakeRunner: newTestRunner(), name: name}
	ctr := &Container{id: name, runner: g, eng: appleEngine{}, creation: "0123456789abcdef"}

	rc, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	data, readErr := io.ReadAll(rc)
	if readErr != nil {
		t.Fatalf("ReadAll: %v", readErr)
	}
	if string(data) != "streamed\n" {
		t.Errorf("stream = %q", data)
	}
	if !g.closed {
		t.Error("the backend stream was not closed on a terminal read")
	}
	assertNameUnlocked(t, name)
	// A second Close must stay harmless.
	if err := rc.Close(); err != nil {
		t.Errorf("Close after EOF: %v", err)
	}
}

func TestFollowLogsReleasesAppleLockOnReadError(t *testing.T) {
	name := "err-" + newContainerName()
	stream := &erroringStream{}
	g := &generationStreamRunner{fakeRunner: newTestRunner(), name: name, stream: stream}
	ctr := &Container{id: name, runner: g, eng: appleEngine{}, creation: "0123456789abcdef"}

	rc, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("ReadAll succeeded although the stream fails")
	}
	if !stream.closed {
		t.Error("the backend stream was not closed on a terminal read error")
	}
	assertNameUnlocked(t, name)
}

// nilStreamRunner reports a successful stream that carries no reader.
type nilStreamRunner struct {
	*generationStreamRunner
	streamCalls int
}

func (s *nilStreamRunner) Stream(_ context.Context, _ ...string) (io.ReadCloser, error) {
	s.streamCalls++
	return nil, nil
}

func TestFollowLogsRejectsNilStream(t *testing.T) {
	name := "nil-" + newContainerName()
	g := &nilStreamRunner{generationStreamRunner: &generationStreamRunner{
		fakeRunner: newTestRunner(),
		name:       name,
	}}
	ctr := &Container{id: name, runner: g, eng: appleEngine{}, creation: "0123456789abcdef"}

	if _, err := ctr.FollowLogs(context.Background()); err == nil {
		t.Fatal("FollowLogs succeeded although the runner returned no stream")
	} else if !strings.Contains(err.Error(), "no stream") {
		t.Errorf("error = %v, want a missing-stream error", err)
	}
	if g.streamCalls != 1 {
		t.Errorf("stream calls = %d, want 1", g.streamCalls)
	}
	assertNameUnlocked(t, name)
}

// assertNameUnlocked fails when the per-name barriers are still held.
func assertNameUnlocked(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	unlock, err := lockName(ctx, name)
	if err != nil {
		t.Fatalf("name %s is still locked after the stream ended: %v", name, err)
	}
	unlock()
}

// The streaming runners above must satisfy the streaming contract.
var (
	_ cli.Streamer = (*generationStreamRunner)(nil)
	_ cli.Streamer = (*nilStreamRunner)(nil)
)
