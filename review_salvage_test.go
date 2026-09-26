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
		"tcp://127.0.0.1:2375",
		"tcp://127.0.0.2:2375",
		"tcp://[::1]:2375",
		"ssh://user@127.0.0.1",
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

// defaultHost still only understands tcp://, because it decides the auto-publish
// bind address and that rewrite is defined for tcp. The two must not be
// conflated: deriving remote detection from defaultHost is what missed ssh://.
func TestReviewDefaultHostRemainsTCPOnly(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://user@remote-host")
	if got := (dockerEngine{}).defaultHost(); got != "127.0.0.1" {
		t.Errorf("defaultHost() = %q, want 127.0.0.1", got)
	}
	if !isRemoteDockerHost() {
		t.Error("an ssh:// daemon must still be detected as remote")
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

	ctr := &Container{}
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
