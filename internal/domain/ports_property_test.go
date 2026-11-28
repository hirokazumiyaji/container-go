package domain

import (
	"strconv"
	"strings"
	"testing"

	"go.ymotongpoo.com/pbt"
)

type portNumberCase struct {
	text  string
	value int
	valid bool
}

func portNumbers() pbt.Gen[portNumberCase] {
	return pbt.Custom("port number", func(t *pbt.T) portNumberCase {
		n := pbt.OneOf(pbt.IntRange(0, 65536), pbt.SampledFrom([]int{0, 1, 65535, 65536})).Draw(t, "number")
		digits := strconv.Itoa(n)
		padding := strings.Repeat("0", pbt.IntRange(1, 5).Draw(t, "padding"))
		form := pbt.SampledFrom([]struct {
			text  string
			valid bool
		}{
			{digits, true},
			{padding + digits, true},
			{"+" + digits, false},
			{"-" + digits, false},
			{" " + digits, false},
			{digits + " ", false},
			{digits + "x", false},
			{"", false},
			{"１", false},
			{"999999999999999999999999999999", false},
		}).Draw(t, "form")
		return portNumberCase{text: form.text, value: n, valid: form.valid && n > 0 && n <= 65535}
	})
}

func TestPortGrammarProperty(t *testing.T) {
	pbt.Check(t, func(t *pbt.T) {
		n := portNumbers().Draw(t, "port")
		protocol := pbt.SampledFrom([]struct{ suffix, proto string }{
			{"", "tcp"},
			{"/tcp", "tcp"},
			{"/udp", "udp"},
			{"/", ""},
			{"/TCP", ""},
			{"/sctp", ""},
			{"/tcp/udp", ""},
		}).Draw(t, "protocol")
		validProtocol := protocol.proto != ""
		input := n.text + protocol.suffix
		t.Classify(n.valid, "valid number")
		t.Classify(!n.valid, "invalid number")
		t.Classify(validProtocol, "valid protocol")
		for _, parser := range []struct {
			name  string
			parse func(string) (PortSpec, error)
			valid bool
		}{
			{"ParsePortSpec", ParsePortSpec, n.valid && validProtocol},
			{"ParseTCPPortSpec", ParseTCPPortSpec, n.valid && protocol.proto == "tcp"},
		} {
			got, err := parser.parse(input)
			if (err == nil) != parser.valid {
				t.Fatalf("%s(%q) = (%+v, %v), want valid=%t", parser.name, input, got, err, parser.valid)
			}
			if parser.valid && got != (PortSpec{Port: n.value, Proto: protocol.proto}) {
				t.Fatalf("%s(%q) = %+v, want port=%d proto=%s", parser.name, input, got, n.value, protocol.proto)
			}
		}
		for _, ports := range []struct {
			text                string
			hostPort, container int
		}{
			{n.text + ":80", n.value, 80},
			{"80:" + n.text, 80, n.value},
		} {
			for _, host := range []string{"", "127.0.0.1:", "[::1]:"} {
				raw := host + ports.text + protocol.suffix
				got, err := ParsePublishSpec(raw)
				valid := n.valid && validProtocol
				if (err == nil) != valid {
					t.Fatalf("ParsePublishSpec(%q) = (%+v, %v), want valid=%t", raw, got, err, valid)
				}
				if valid && (got.HostPort != ports.hostPort || got.ContainerPort != ports.container || got.Proto != protocol.proto || got.Raw != raw) {
					t.Fatalf("ParsePublishSpec(%q) = %+v, want hostPort=%d containerPort=%d proto=%s", raw, got, ports.hostPort, ports.container, protocol.proto)
				}
			}
		}
	})
}
