package domain

import (
	"errors"
	"testing"
)

type endpointCase struct {
	name        string
	network     EndpointNetwork
	backend     EndpointBackend
	container   string
	port        PortSpec
	published   string
	wantHost    string
	wantPort    int
	wantErr     error
	wantStale   bool
	wantErrText string
}

func runEndpointCases(t *testing.T, cases []endpointCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := ResolveEndpoint(tc.network, tc.backend, EndpointRequest{
				Container: tc.container,
				Port:      tc.port,
				Published: parseTestPublish(t, tc.published),
			})
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ResolveEndpoint() error = %v", err)
				}
				if host != tc.wantHost || port != tc.wantPort {
					t.Fatalf("ResolveEndpoint() = (%q, %d), want (%q, %d)", host, port, tc.wantHost, tc.wantPort)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveEndpoint() error = %v, want %v", err, tc.wantErr)
			}
			if got := EndpointSnapshotStale(err); got != tc.wantStale {
				t.Errorf("EndpointSnapshotStale(%v) = %t, want %t", err, got, tc.wantStale)
			}
			if err.Error() != tc.wantErrText {
				t.Errorf("error = %q, want %q", err, tc.wantErrText)
			}
		})
	}
}

func parseTestPublish(t *testing.T, raw string) *PublishSpec {
	t.Helper()
	if raw == "" {
		return nil
	}
	spec, err := ParsePublishSpec(raw)
	if err != nil {
		t.Fatalf("ParsePublishSpec(%q): %v", raw, err)
	}
	return &spec
}

func TestResolveEndpointHost(t *testing.T) {
	defaultHost := EndpointBackend{DefaultHost: "127.0.0.1"}
	runEndpointCases(t, []endpointCase{
		{
			name:      "direct-ip uses the container address",
			network:   EndpointNetwork{IP: "192.0.2.1"},
			backend:   EndpointBackend{DirectIP: true, DefaultHost: "127.0.0.1"},
			container: "ctr",
			wantHost:  "192.0.2.1",
		},
		{
			name:      "direct-ip prefers the published address",
			network:   EndpointNetwork{IP: "192.0.2.1"},
			backend:   EndpointBackend{DirectIP: true, DefaultHost: "127.0.0.1"},
			container: "ctr",
			published: "0.0.0.0:16379:6379/tcp",
			wantHost:  "127.0.0.1",
		},
		{
			name:        "direct-ip reports a missing address",
			network:     EndpointNetwork{},
			backend:     EndpointBackend{DirectIP: true, DefaultHost: "127.0.0.1"},
			container:   "ctr",
			wantErr:     errNoReportedAddress,
			wantStale:   true,
			wantErrText: "container ctr has no reported IP address",
		},
		{
			name:      "bridge without a publish uses the default host",
			network:   EndpointNetwork{IP: "172.17.0.2", Mode: "bridge"},
			backend:   defaultHost,
			container: "ctr",
			wantHost:  "127.0.0.1",
		},
		{
			name: "bridge with a publish uses the inspected binding",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 16379},
				},
			},
			backend:   defaultHost,
			container: "ctr",
			published: "127.0.0.1:16379:6379/tcp",
			wantHost:  "127.0.0.1",
		},
		{
			name: "an empty bind address uses the local default host",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "", HostPort: 16379},
				},
			},
			backend:   defaultHost,
			container: "ctr",
			published: "16379:6379/tcp",
			wantHost:  "127.0.0.1",
		},
		{
			name: "an empty bind address uses the remote default host",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "", HostPort: 16379},
				},
			},
			backend:   EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container: "ctr",
			published: "16379:6379/tcp",
			wantHost:  "10.0.0.5",
		},
		{
			name: "unspecified IPv4 binding becomes loopback",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "0.0.0.0", HostPort: 16379},
				},
			},
			backend:   defaultHost,
			container: "ctr",
			published: "16379:6379/tcp",
			wantHost:  "127.0.0.1",
		},
		{
			name: "unspecified IPv6 binding becomes loopback",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "::", HostPort: 16379},
				},
			},
			backend:   defaultHost,
			container: "ctr",
			published: "16379:6379/tcp",
			wantHost:  "::1",
		},
		{
			name: "expanded IPv6 loopback is canonical",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "::1", HostPort: 16379},
				},
			},
			backend:   defaultHost,
			container: "ctr",
			published: "[0:0:0:0:0:0:0:1]:16379:6379/tcp",
			wantHost:  "::1",
		},
		{
			name: "remote loopback binding is unreachable",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 16379},
				},
			},
			backend:     EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container:   "ctr",
			published:   "127.0.0.1:16379:6379/tcp",
			wantErr:     ErrEndpointUnreachable,
			wantErrText: `container endpoint is unreachable: Docker port 16379 is bound to remote-daemon loopback "127.0.0.1"`,
		},
		{
			name: "remote unspecified binding uses the remote default host",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "0.0.0.0", HostPort: 16379},
				},
			},
			backend:   EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container: "ctr",
			published: "16379:6379/tcp",
			wantHost:  "10.0.0.5",
		},
		{
			name:        "missing published binding is stale",
			network:     EndpointNetwork{IP: "172.17.0.2", Mode: "bridge"},
			backend:     defaultHost,
			container:   "ctr",
			published:   "127.0.0.1:16379:6379/tcp",
			wantErr:     ErrPortNotExposed,
			wantStale:   true,
			wantErrText: `port is not declared or has no usable host binding: published port "127.0.0.1:16379:6379/tcp" has no host binding in network mode "bridge"`,
		},
		{
			name:      "host network uses the daemon host",
			network:   EndpointNetwork{Mode: NetworkModeHost},
			backend:   defaultHost,
			container: "ctr",
			wantHost:  "127.0.0.1",
		},
		{
			name:        "host network rejects a requested publish",
			network:     EndpointNetwork{Mode: NetworkModeHost},
			backend:     defaultHost,
			container:   "ctr",
			published:   "127.0.0.1:16379:6379/tcp",
			wantErr:     ErrPortNotExposed,
			wantErrText: "port is not declared or has no usable host binding: Docker host networking has no library-managed host port binding; Host returns the daemon host, but Endpoint does not infer a port",
		},
		{
			name:        "none network has no reachable host",
			network:     EndpointNetwork{Mode: NetworkModeNone},
			backend:     defaultHost,
			container:   "ctr",
			wantErr:     ErrNoReachableHost,
			wantErrText: `container has no reachable host: container ctr uses network mode "none"`,
		},
		{
			name:        "none network ignores a requested publish",
			network:     EndpointNetwork{Mode: NetworkModeNone},
			backend:     defaultHost,
			container:   "ctr",
			published:   "127.0.0.1:16379:6379/tcp",
			wantErr:     ErrNoReachableHost,
			wantErrText: `container has no reachable host: container ctr uses network mode "none"`,
		},
	})
}

func TestResolveEndpointPort(t *testing.T) {
	tcp6379 := PortSpec{Port: 6379, Proto: "tcp"}
	runEndpointCases(t, []endpointCase{
		{
			name:      "direct-ip uses the published host and port",
			network:   EndpointNetwork{IP: "192.0.2.1"},
			backend:   EndpointBackend{DirectIP: true, DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			published: "127.0.0.1:16379:6379/tcp",
			wantHost:  "127.0.0.1",
			wantPort:  16379,
		},
		{
			name:      "direct-ip uses the container address and port",
			network:   EndpointNetwork{IP: "192.0.2.1"},
			backend:   EndpointBackend{DirectIP: true, DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "192.0.2.1",
			wantPort:  6379,
		},
		{
			name:        "direct-ip reports a missing address",
			network:     EndpointNetwork{},
			backend:     EndpointBackend{DirectIP: true, DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     errNoReportedAddress,
			wantStale:   true,
			wantErrText: "container ctr has no reported IP address",
		},
		{
			name: "published binding wins over exposed",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 16379},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			published: "127.0.0.1:16379:6379/tcp",
			wantHost:  "127.0.0.1",
			wantPort:  16379,
		},
		{
			name: "a missing published binding does not fall back to exposed",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
				},
			},
			backend:     EndpointBackend{DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			published:   "127.0.0.1:16379:6379/tcp",
			wantErr:     ErrPortNotExposed,
			wantStale:   true,
			wantErrText: `port is not declared or has no usable host binding: published port "127.0.0.1:16379:6379/tcp" has no host binding in network mode "bridge"`,
		},
		{
			name: "exposed binding uses the assigned port",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "127.0.0.1",
			wantPort:  49153,
		},
		{
			name: "an empty bind address uses the local default host",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "", HostPort: 49153},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "127.0.0.1",
			wantPort:  49153,
		},
		{
			name: "an empty bind address uses the remote default host",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "", HostPort: 49153},
				},
			},
			backend:   EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "10.0.0.5",
			wantPort:  49153,
		},
		{
			name: "remote unspecified IPv6 binding uses the remote default host",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "::", HostPort: 49153},
				},
			},
			backend:   EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "10.0.0.5",
			wantPort:  49153,
		},
		{
			name:        "missing exposed binding is stale",
			network:     EndpointNetwork{IP: "172.17.0.2", Mode: "bridge"},
			backend:     EndpointBackend{DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     ErrPortNotExposed,
			wantStale:   true,
			wantErrText: `port is not declared or has no usable host binding: 6379/tcp has no host binding in network mode "bridge"`,
		},
		{
			name:        "host network cannot infer a port",
			network:     EndpointNetwork{Mode: NetworkModeHost},
			backend:     EndpointBackend{DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     ErrPortNotExposed,
			wantErrText: "port is not declared or has no usable host binding: Docker host networking has no library-managed host port binding; Host returns the daemon host, but Endpoint does not infer a port",
		},
		{
			name:        "padded host mode is still classified as host networking",
			network:     EndpointNetwork{Mode: " " + NetworkModeHost + " "},
			backend:     EndpointBackend{DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     ErrPortNotExposed,
			wantErrText: "port is not declared or has no usable host binding: Docker host networking has no library-managed host port binding; Host returns the daemon host, but Endpoint does not infer a port",
		},
		{
			name:        "none network has no host binding",
			network:     EndpointNetwork{Mode: NetworkModeNone},
			backend:     EndpointBackend{DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     ErrPortNotExposed,
			wantErrText: "port is not declared or has no usable host binding: Docker network mode \"none\" has no network interface for a host port binding\ncontainer has no reachable host: Docker network mode \"none\" has no reachable host",
		},
		{
			name: "remote loopback-only binding is unreachable",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
				},
			},
			backend:     EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     ErrEndpointUnreachable,
			wantErrText: `container endpoint is unreachable: Docker port 49153 is bound to remote-daemon loopback "127.0.0.1"`,
		},
		{
			name: "remote prefers a non-loopback binding",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "0.0.0.0", HostPort: 49154},
				},
			},
			backend:   EndpointBackend{Remote: true, DefaultHost: "10.0.0.5"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "10.0.0.5",
			wantPort:  49154,
		},
		{
			name: "local preserves binding declaration order",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "0.0.0.0", HostPort: 49154},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "127.0.0.1",
			wantPort:  49153,
		},
		{
			name: "publish host address selects the matching binding",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 16379},
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "::1", HostPort: 16379},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			published: "[::1]:16379:6379/tcp",
			wantHost:  "::1",
			wantPort:  16379,
		},
		{
			name: "udp query matches only a udp binding",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 53, Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 49153},
					{ContainerPort: 53, Proto: "udp", HostAddr: "127.0.0.1", HostPort: 49154},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      PortSpec{Port: 53, Proto: "udp"},
			wantHost:  "127.0.0.1",
			wantPort:  49154,
		},
		{
			name:        "a binding without a host port is unusable",
			network:     EndpointNetwork{IP: "172.17.0.2", Mode: "bridge", Bindings: []PortBinding{{ContainerPort: 6379, Proto: "tcp", HostAddr: "127.0.0.1"}}},
			backend:     EndpointBackend{DefaultHost: "127.0.0.1"},
			container:   "ctr",
			port:        tcp6379,
			wantErr:     ErrPortNotExposed,
			wantStale:   true,
			wantErrText: `port is not declared or has no usable host binding: 6379/tcp has no host binding in network mode "bridge"`,
		},
		{
			name: "unspecified IPv6 binding becomes loopback",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "::", HostPort: 49153},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "::1",
			wantPort:  49153,
		},
		{
			name: "IPv4-mapped IPv6 binding is canonicalized",
			network: EndpointNetwork{
				IP:   "172.17.0.2",
				Mode: "bridge",
				Bindings: []PortBinding{
					{ContainerPort: 6379, Proto: "tcp", HostAddr: "::ffff:192.0.2.1", HostPort: 49153},
				},
			},
			backend:   EndpointBackend{DefaultHost: "127.0.0.1"},
			container: "ctr",
			port:      tcp6379,
			wantHost:  "192.0.2.1",
			wantPort:  49153,
		},
	})
}

// none network reports both the missing-binding and no-host failures so
// callers can match either public sentinel.
func TestResolveEndpointNoneNetworkReportsBothFailures(t *testing.T) {
	_, _, err := ResolveEndpoint(
		EndpointNetwork{Mode: NetworkModeNone},
		EndpointBackend{DefaultHost: "127.0.0.1"},
		EndpointRequest{Container: "ctr", Port: PortSpec{Port: 6379, Proto: "tcp"}},
	)
	if !errors.Is(err, ErrPortNotExposed) || !errors.Is(err, ErrNoReachableHost) {
		t.Fatalf("error = %v, want port and host sentinels", err)
	}
	if EndpointSnapshotStale(err) {
		t.Errorf("EndpointSnapshotStale(%v) = true, want false", err)
	}
}

func TestNetworkEndpointError(t *testing.T) {
	hostErr := NetworkEndpointError(NetworkModeHost)
	if !errors.Is(hostErr, ErrPortNotExposed) || errors.Is(hostErr, ErrNoReachableHost) {
		t.Errorf("host mode error = %v, want only ErrPortNotExposed", hostErr)
	}
	if !errors.Is(NetworkEndpointError(" "+NetworkModeHost+" "), ErrPortNotExposed) {
		t.Errorf("mode with surrounding spaces = %v, want ErrPortNotExposed", NetworkEndpointError(" "+NetworkModeHost+" "))
	}
	noneErr := NetworkEndpointError(NetworkModeNone)
	if !errors.Is(noneErr, ErrPortNotExposed) || !errors.Is(noneErr, ErrNoReachableHost) {
		t.Errorf("none mode error = %v, want port and host sentinels", noneErr)
	}
	if err := NetworkEndpointError("bridge"); err != nil {
		t.Errorf("bridge mode error = %v, want nil", err)
	}
}

func TestEndpointSnapshotStale(t *testing.T) {
	if EndpointSnapshotStale(nil) {
		t.Error("EndpointSnapshotStale(nil) = true, want false")
	}
	if EndpointSnapshotStale(errors.New("other")) {
		t.Error("EndpointSnapshotStale(other error) = true, want false")
	}
}
