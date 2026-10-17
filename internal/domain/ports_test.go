package domain

import (
	"testing"
)

func TestParsePortSpec(t *testing.T) {
	tests := []struct {
		input   string
		want    PortSpec
		wantErr bool
	}{
		{"80", PortSpec{Port: 80, Proto: "tcp"}, false},
		{"80/tcp", PortSpec{Port: 80, Proto: "tcp"}, false},
		{"53/udp", PortSpec{Port: 53, Proto: "udp"}, false},
		{"0", PortSpec{}, true},
		{"65536", PortSpec{}, true},
		{"abc", PortSpec{}, true},
	}

	for _, tt := range tests {
		got, err := ParsePortSpec(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePortSpec(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParsePortSpec(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParsePublishSpec(t *testing.T) {
	tests := []struct {
		input   string
		want    PublishSpec
		wantErr bool
	}{
		{
			input: "8080:80",
			want: PublishSpec{
				HostAddr:      "",
				HostPort:      8080,
				ContainerPort: 80,
				Proto:         "tcp",
				Raw:           "8080:80",
			},
		},
		{
			input: "127.0.0.1:8080:80/udp",
			want: PublishSpec{
				HostAddr:      "127.0.0.1",
				HostPort:      8080,
				ContainerPort: 80,
				Proto:         "udp",
				Raw:           "127.0.0.1:8080:80/udp",
			},
		},
		{
			input: "[::1]:8080:80",
			want: PublishSpec{
				HostAddr:      "::1",
				HostPort:      8080,
				ContainerPort: 80,
				Proto:         "tcp",
				Raw:           "[::1]:8080:80",
			},
		},
		{input: "invalid", wantErr: true},
		{input: "8080:80:80:80", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParsePublishSpec(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePublishSpec(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if got.HostAddr != tt.want.HostAddr || got.HostPort != tt.want.HostPort || got.ContainerPort != tt.want.ContainerPort || got.Proto != tt.want.Proto {
				t.Errorf("ParsePublishSpec(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		}
	}
}

func TestIPHelpers(t *testing.T) {
	if !IPIs4("127.0.0.1") {
		t.Error("127.0.0.1 should be IPv4")
	}
	if !IPIs6("::1") {
		t.Error("::1 should be IPv6")
	}
	if !IPIsLoopback("127.0.0.1") || !IPIsLoopback("::1") {
		t.Error("expected loopback")
	}
	if !IPIsUnspecified("0.0.0.0") || !IPIsUnspecified("::") {
		t.Error("expected unspecified")
	}
	if !IsLoopbackOrUnspecified("localhost") || !IsLoopbackOrUnspecified("") {
		t.Error("expected loopback or unspecified")
	}
}
