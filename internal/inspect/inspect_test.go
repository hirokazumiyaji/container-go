package inspect

import (
	"os"
	"strings"
	"testing"
)

func decodeTestdata(t *testing.T, name string) []Container {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	containers, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode(%s): %v", name, err)
	}
	return containers
}

func TestDecodeRealOutputSample(t *testing.T) {
	for _, fixture := range []string{"inspect_v1.2.2.json", "inspect_v1.3.0.json"} {
		t.Run(fixture, func(t *testing.T) {
			containers := decodeTestdata(t, fixture)
			if len(containers) < 1 {
				t.Fatalf("len = %d, want >= 1", len(containers))
			}

			c := containers[0]
			if c.ID != "containergo-1a2b3c4d" {
				t.Errorf("ID = %q", c.ID)
			}
			if got := c.Configuration.Image.Reference; got != "docker.io/library/redis:7-alpine" {
				t.Errorf("Image.Reference = %q", got)
			}
			if got := c.Configuration.Labels["com.github.hirokazumiyaji.container-go.session"]; got != "f00dcafe" {
				t.Errorf("session label = %q", got)
			}
			if got := c.Status.State; got != "running" {
				t.Errorf("State = %q", got)
			}
		})
	}
}

func TestIPv4StripsCIDRSuffix(t *testing.T) {
	for _, fixture := range []string{"inspect_v1.2.2.json", "inspect_v1.3.0.json"} {
		t.Run(fixture, func(t *testing.T) {
			containers := decodeTestdata(t, fixture)
			ip, err := containers[0].IPv4()
			if err != nil {
				t.Fatalf("IPv4: %v", err)
			}
			if ip != "192.168.64.3" {
				t.Errorf("IPv4 = %q, want %q", ip, "192.168.64.3")
			}
		})
	}
}

func TestIPv4ErrorsWithoutNetworks(t *testing.T) {
	containers := decodeTestdata(t, "inspect_v1.2.2.json")

	if _, err := containers[1].IPv4(); err == nil {
		t.Error("IPv4 on container without networks: want error, got nil")
	}
}

func TestDecodePublishedPortsV122(t *testing.T) {
	containers := decodeTestdata(t, "inspect_v1.2.2.json")
	ports := containers[0].Configuration.PublishedPorts
	if len(ports) != 1 {
		t.Fatalf("len(PublishedPorts) = %d, want 1", len(ports))
	}
	p := ports[0]
	if p.HostAddress != "127.0.0.1" || p.HostPort != 16379 || p.ContainerPort != 6379 || p.Proto != "tcp" {
		t.Errorf("PublishedPort = %+v", p)
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	data := []byte(`[{"id":"x","futureField":{"nested":true},"configuration":{"labels":{}},"status":{"state":"running","networks":[]}}]`)
	containers, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if containers[0].ID != "x" {
		t.Errorf("ID = %q", containers[0].ID)
	}
}

func TestDecodeRejectsInvalidJSON(t *testing.T) {
	if _, err := Decode([]byte(`{not json`)); err == nil {
		t.Error("Decode invalid input: want error, got nil")
	}
}

// A null entry decodes to a zero Container, whose empty ID is skipped as a
// non-match. That would report a missing container for output that never
// said whether the target exists.
func TestDecodeRejectsNullEntries(t *testing.T) {
	cases := map[string]string{
		"null entry":            `[null]`,
		"null entry after one":  `[{"id":"x"},null]`,
		"null entry before one": `[null,{"id":"x"}]`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(data))
			if err == nil {
				t.Fatal("Decode null entry: want error, got nil")
			}
			if !strings.Contains(err.Error(), "got null") {
				t.Errorf("Decode = %v, want a schema error naming the null entry", err)
			}
		})
	}
}

func TestDecodeEmptyArrayIsNotAnError(t *testing.T) {
	containers, err := Decode([]byte(`[]`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("len = %d, want 0", len(containers))
	}
}
