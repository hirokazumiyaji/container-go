package domain

import (
	"errors"
	"testing"

	"go.ymotongpoo.com/pbt"
)

func TestResolveEndpointProperty(t *testing.T) {
	pbt.Check(t, func(t *pbt.T) {
		containerPort := pbt.IntRange(1, 65534).Draw(t, "container port")
		hostPort := pbt.IntRange(1, 65534).Draw(t, "host port")
		proto := pbt.SampledFrom([]string{"tcp", "udp"}).Draw(t, "protocol")
		otherProto := "udp"
		if proto == otherProto {
			otherProto = "tcp"
		}
		address := pbt.SampledFrom([]struct{ bind, connect string }{
			{"", "192.0.2.10"},
			{"0.0.0.0", "192.0.2.10"},
			{"::", "192.0.2.10"},
			{"192.0.2.20", "192.0.2.20"},
			{"2001:db8::20", "2001:db8::20"},
		}).Draw(t, "address")
		request := EndpointRequest{Container: "generated", Port: PortSpec{Port: containerPort, Proto: proto}}
		distractors := []PortBinding{
			{ContainerPort: containerPort + 1, Proto: proto, HostAddr: address.bind, HostPort: hostPort},
			{ContainerPort: containerPort, Proto: otherProto, HostAddr: address.bind, HostPort: hostPort},
			{ContainerPort: containerPort, Proto: proto, HostAddr: address.bind, HostPort: 0},
		}
		if pbt.Bool().Draw(t, "published") {
			request.Published = &PublishSpec{ContainerPort: containerPort, HostPort: hostPort, Proto: proto}
			distractors = append(distractors, PortBinding{ContainerPort: containerPort, Proto: proto, HostAddr: address.bind, HostPort: hostPort + 1})
		}
		t.Classify(request.Published != nil, "published")
		t.Classify(request.Published == nil, "exposed")
		loopback := PortBinding{ContainerPort: containerPort, Proto: proto, HostPort: hostPort,
			HostAddr: pbt.SampledFrom([]string{"127.0.0.1", "::1", "::ffff:127.0.0.1"}).Draw(t, "remote loopback")}
		target := PortBinding{ContainerPort: containerPort, Proto: proto, HostAddr: address.bind, HostPort: hostPort}
		for _, scenario := range []struct {
			name    string
			extra   []PortBinding
			wantErr error
			stale   bool
		}{
			{name: "reachable", extra: []PortBinding{loopback, target}},
			{name: "remote loopback", extra: []PortBinding{loopback}, wantErr: ErrEndpointUnreachable},
			{name: "absent", wantErr: ErrPortNotExposed, stale: true},
		} {
			bindings := append(append([]PortBinding{}, distractors...), scenario.extra...)
			bindings = pbt.Permutation(bindings).Draw(t, scenario.name)
			host, port, err := ResolveEndpoint(EndpointNetwork{Bindings: bindings},
				EndpointBackend{Remote: true, DefaultHost: "192.0.2.10"}, request)
			if !errors.Is(err, scenario.wantErr) || EndpointSnapshotStale(err) != scenario.stale {
				t.Fatalf("%s bindings=%+v: error=%v stale=%t, want error=%v stale=%t", scenario.name, bindings, err, EndpointSnapshotStale(err), scenario.wantErr, scenario.stale)
			}
			if scenario.wantErr == nil && (host != address.connect || port != hostPort) {
				t.Fatalf("bindings=%+v: endpoint=(%q, %d), want (%q, %d)", bindings, host, port, address.connect, hostPort)
			}
		}
	})
}
