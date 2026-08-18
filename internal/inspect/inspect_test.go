package inspect

import (
	"os"
	"testing"
)

func decodeTestdata(t *testing.T) []Container {
	t.Helper()
	data, err := os.ReadFile("testdata/inspect_v1.2.2.json")
	if err != nil {
		t.Fatal(err)
	}
	containers, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return containers
}

func TestDecodeRealOutputSample(t *testing.T) {
	containers := decodeTestdata(t)
	if len(containers) != 2 {
		t.Fatalf("len = %d, want 2", len(containers))
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

	ports := c.Configuration.PublishedPorts
	if len(ports) != 1 {
		t.Fatalf("len(PublishedPorts) = %d, want 1", len(ports))
	}
	p := ports[0]
	if p.HostAddress != "127.0.0.1" || p.HostPort != 16379 || p.ContainerPort != 6379 || p.Proto != "tcp" {
		t.Errorf("PublishedPort = %+v", p)
	}
}

func TestIPv4StripsCIDRSuffix(t *testing.T) {
	containers := decodeTestdata(t)

	ip, err := containers[0].IPv4()
	if err != nil {
		t.Fatalf("IPv4: %v", err)
	}
	if ip != "192.168.64.3" {
		t.Errorf("IPv4 = %q, want %q", ip, "192.168.64.3")
	}
}

func TestIPv4ErrorsWithoutNetworks(t *testing.T) {
	containers := decodeTestdata(t)

	if _, err := containers[1].IPv4(); err == nil {
		t.Error("IPv4 on container without networks: want error, got nil")
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
