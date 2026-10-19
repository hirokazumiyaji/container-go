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

func TestParsePublishSpecErrorFields(t *testing.T) {
	tests := []struct {
		input   string
		field   string
		message string
	}{
		{"8080:80/sctp", "published port", "protocol must be tcp or udp"},
		{"[::1:8080:80", "published port", "bracketed IPv6 form is malformed"},
		{"invalid", "published port", "want [host-ip:]host-port:container-port[/proto]"},
		{"70000:80", "port", "must be 1-65535"},
		{"8080:99999", "port", "must be 1-65535"},
	}
	for _, tt := range tests {
		_, err := ParsePublishSpec(tt.input)
		if err == nil {
			t.Errorf("ParsePublishSpec(%q) = nil error", tt.input)
			continue
		}
		field, ok := FieldOf(err)
		if !ok || field != tt.field {
			t.Errorf("ParsePublishSpec(%q) field = %q, %t; want %q, true", tt.input, field, ok, tt.field)
		}
		if err.Error() != tt.message {
			t.Errorf("ParsePublishSpec(%q) error = %q, want %q", tt.input, err, tt.message)
		}
	}

	// A malformed host address is not tied to an option field: the caller
	// wraps it with the full publish specification as the diagnostic value.
	_, err := ParsePublishSpec("badhost:8080:80")
	if err == nil {
		t.Fatal("ParsePublishSpec(badhost) = nil error")
	}
	if field, ok := FieldOf(err); ok {
		t.Errorf("host address error field = %q, want no field", field)
	}
}
