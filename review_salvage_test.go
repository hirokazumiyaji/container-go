package container

import (
	"strings"
	"sync"
	"testing"
)

// ssh:// is a first-class Docker remote transport, and auto-publish against a
// remote daemon must bind 0.0.0.0 because a 127.0.0.1 bind lands on the remote
// machine. Detecting only tcp:// reported local, so the loopback-publish guard
// never fired and the caller got a published port it could not reach.
func TestReviewRemoteDockerHostCoversEveryTransport(t *testing.T) {
	remote := []string{
		"ssh://user@remote-host",
		"ssh://user@10.0.0.5",
		"http://10.0.0.5:2375",
		"https://10.0.0.5:2376",
		"tcp://10.0.0.5:2375",
		// A scheme-less remote host, which the CLI normalizes to tcp://.
		"docker.example.com:2375",
		// A bare number is a hostname to the Docker CLI, not a local port.
		"2375",
	}
	for _, host := range remote {
		t.Run(host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			if !isRemoteDockerHost() {
				t.Errorf("DOCKER_HOST=%q: want remote", host)
			}
		})
	}

	local := []string{
		"",
		"unix:///var/run/docker.sock",
		"npipe:////./pipe/docker_engine",
		// fd:// is systemd socket activation: a real Docker transport for a
		// local daemon. url.Parse yields an empty hostname for it, so it must
		// be listed explicitly rather than falling through to fail-closed.
		"fd://",
		"tcp://127.0.0.1:2375",
		"tcp://127.0.0.2:2375",
		"tcp://[::1]:2375",
		"ssh://user@127.0.0.1",
		// The Docker CLI normalizes a scheme-less DOCKER_HOST to tcp:// before
		// dialing, so these are all valid. url.Parse cannot read them, so
		// without normalization they would fall through to fail-closed and a
		// local daemon would be reported remote — which binds auto-published
		// ports to 0.0.0.0 on the developer's own machine.
		"127.0.0.1:2375",
		"localhost:2375",
		"[::1]:2375",
		// Empty host: Docker substitutes its default hostname (local).
		"tcp://:2375",
		":2375",
	}

	for _, host := range local {
		t.Run("local/"+host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			if isRemoteDockerHost() {
				t.Errorf("DOCKER_HOST=%q: want local", host)
			}
		})
	}

	// A malformed value must fail closed rather than silently be treated as a
	// local daemon.
	t.Setenv("DOCKER_HOST", "://///")
	if !isRemoteDockerHost() {
		t.Error("a malformed DOCKER_HOST must be treated as remote")
	}
}

// The client-facing host must be scheme-aware for every remote transport.
// Telling the caller to dial 127.0.0.1 for a container published on a remote
// daemon reaches nothing, which is the symptom this change exists to prevent.
func TestReviewDefaultHostIsSchemeAware(t *testing.T) {
	cases := map[string]string{
		"ssh://user@remote-host":    "remote-host",
		"ssh://user@10.0.0.5":       "10.0.0.5",
		"http://10.0.0.5:2375":      "10.0.0.5",
		"tcp://10.0.0.5:2375":       "10.0.0.5",
		"":                          "127.0.0.1",
		"unix:///var/run/d.sock":    "127.0.0.1",
		"fd://":                     "127.0.0.1",
		"npipe:////./pipe/docker_e": "127.0.0.1",
	}
	for host, want := range cases {
		t.Run(host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			if got := (dockerEngine{}).defaultHost(); got != want {
				t.Errorf("defaultHost() = %q, want %q", got, want)
			}
		})
	}
}

// A remote daemon's published address must not be rewritten to the client's own
// loopback, which is what an incomplete fix leaves behind.
func TestReviewConnectHostUsesRemoteHostName(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://user@remote-host")
	eng := dockerEngine{}
	// A 0.0.0.0 bind on a remote daemon must resolve to the remote host, not
	// to 127.0.0.1.
	if got := dockerConnectHost("0.0.0.0", eng); got != "remote-host" {
		t.Errorf("dockerConnectHost(0.0.0.0) = %q, want remote-host", got)
	}
	// Locally the same bind is this machine.
	t.Setenv("DOCKER_HOST", "")
	if got := dockerConnectHost("0.0.0.0", eng); got != "127.0.0.1" {
		t.Errorf("dockerConnectHost(0.0.0.0) locally = %q, want 127.0.0.1", got)
	}
}

// The observable that matters for a scheme-less DOCKER_HOST: the actual bind
// address in the run argv. A local daemon must publish on loopback. Binding
// 0.0.0.0 here would expose the container's port on every interface of the
// developer's own machine, which is the opposite of what the loopback default
// is for.
func TestReviewAutoPublishBindsLoopbackForSchemeLessLocalHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1:2375", "localhost:2375", "", "unix:///var/run/d.sock", "tcp://:2375", ":2375"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			cfg := newConfig()
			cfg.eng = dockerEngine{}
			cfg.name = "myctr"
			cfg.exposed = []portSpec{{port: 6379, proto: "tcp"}}
			args := strings.Join((dockerEngine{}).runArgs(cfg, "redis:7-alpine", ""), " ")
			if !strings.Contains(args, "--publish 127.0.0.1::6379/tcp") {
				t.Errorf("DOCKER_HOST=%q: run args = %s, want a loopback publish", host, args)
			}
			if strings.Contains(args, "0.0.0.0") {
				t.Errorf("DOCKER_HOST=%q: run args = %s, must not bind all interfaces", host, args)
			}
		})
	}

	// A remote daemon is the opposite: all interfaces, so the client can
	// reach it through defaultHost. A bare number is a remote hostname.
	for _, host := range []string{"ssh://user@remote-host", "2375"} {
		t.Run("remote/"+host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			cfg := newConfig()
			cfg.eng = dockerEngine{}
			cfg.name = "myctr"
			cfg.exposed = []portSpec{{port: 6379, proto: "tcp"}}
			args := strings.Join((dockerEngine{}).runArgs(cfg, "redis:7-alpine", ""), " ")
			if !strings.Contains(args, "--publish 0.0.0.0::6379/tcp") {
				t.Errorf("DOCKER_HOST=%q: run args = %s, want an all-interfaces publish", host, args)
			}
		})
	}
}

// Auto-publish must keep binding loopback for a local daemon, including the
// transports that carry no hostname.
func TestReviewAutoPublishBindsLoopbackLocally(t *testing.T) {
	for _, host := range []string{"", "unix:///var/run/d.sock", "fd://", "npipe:////./pipe/docker_e"} {
		t.Setenv("DOCKER_HOST", host)
		if isRemoteDockerHost() {
			t.Errorf("DOCKER_HOST=%q: reported remote; auto-publish would bind 0.0.0.0", host)
		}
	}
	t.Setenv("DOCKER_HOST", "ssh://user@remote-host")
	if !isRemoteDockerHost() {
		t.Error("an ssh:// daemon must be remote so auto-publish binds 0.0.0.0")
	}
}

// The user-visible effect: a loopback publish on an ssh:// daemon is rejected,
// because Docker would listen on the remote machine's loopback.
func TestReviewLoopbackPublishRejectedOnSSHDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://user@remote-host")
	err := (dockerEngine{}).checkConfig(&config{
		published: []publishSpec{{hostAddr: "127.0.0.1", raw: "127.0.0.1:18080:80"}},
	})
	if err == nil {
		t.Fatal("a loopback publish on a remote ssh:// daemon was accepted")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("err = %v, want the unreachable-bind reason", err)
	}

	// A non-loopback bind is fine on the same remote daemon.
	if err := (dockerEngine{}).checkConfig(&config{
		published: []publishSpec{{hostAddr: "0.0.0.0", raw: "0.0.0.0:18080:80"}},
	}); err != nil {
		t.Errorf("a 0.0.0.0 publish on a remote daemon was rejected: %v", err)
	}
}

// A local daemon must keep accepting a loopback publish, which is the normal
// local development case.
func TestReviewLoopbackPublishStillAllowedLocally(t *testing.T) {
	for _, host := range []string{"", "unix:///var/run/docker.sock", "tcp://127.0.0.1:2375"} {
		t.Setenv("DOCKER_HOST", host)
		if err := (dockerEngine{}).checkConfig(&config{
			published: []publishSpec{{hostAddr: "127.0.0.1", raw: "127.0.0.1:18080:80"}},
		}); err != nil {
			t.Errorf("DOCKER_HOST=%q: local loopback publish rejected: %v", host, err)
		}
	}
}

// uid is promoted from the first inspect, long after the handle is published.
// Terminate read it without synchronization while cachedInfo wrote it, which is
// a data race on a published handle.
func TestReviewImmutableIDAccessIsSynchronized(t *testing.T) {
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)

	ctr := &Container{state: &containerState{}}
	if got := ctr.immutableID(); got != "" {
		t.Fatalf("immutableID() = %q before any promotion", got)
	}
	ctr.setImmutableID(first)

	// Concurrent readers and writers must not race, and the value must stay
	// the first one promoted.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got := ctr.immutableID(); got != first {
					t.Errorf("immutableID() = %q mid-race, want %q", got, first)
					return
				}
				ctr.setImmutableID(second)
			}
		}()
	}
	wg.Wait()

	if got := ctr.immutableID(); got != first {
		t.Errorf("immutableID() = %q, want the first promoted value %q", got, first)
	}
	// An empty write must not clear it.
	ctr.setImmutableID("")
	if got := ctr.immutableID(); got != first {
		t.Errorf("immutableID() = %q after an empty write, want it preserved", got)
	}
}
