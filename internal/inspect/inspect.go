// Package inspect decodes the JSON emitted by `container ls --format
// json` and `container inspect`. Unknown fields are ignored so that
// additive CLI changes do not break decoding.
package inspect

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// Container is one element of the array both commands emit.
type Container struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Configuration Configuration `json:"configuration"`
	Status        Status        `json:"status"`
}

type Configuration struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          Image             `json:"image"`
	Labels         map[string]string `json:"labels"`
	PublishedPorts []PublishedPort   `json:"publishedPorts"`
}

type Image struct {
	Reference string `json:"reference"`
}

type PublishedPort struct {
	HostAddress   string `json:"hostAddress"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Proto         string `json:"proto"`
}

type Status struct {
	State    string    `json:"state"`
	Networks []Network `json:"networks"`
}

type Network struct {
	IPv4Address string `json:"ipv4Address"`
	Hostname    string `json:"hostname"`
	Network     string `json:"network"`
}

// Decode parses the JSON array output.
func Decode(data []byte) ([]Container, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode container inspect output: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, fmt.Errorf("decode container inspect output: expected a container array, got null")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode container inspect output: %w", err)
	}
	containers := make([]Container, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(string(entry)) == "null" {
			return nil, fmt.Errorf("decode container inspect output: null container entry")
		}
		var container Container
		if err := json.Unmarshal(entry, &container); err != nil {
			return nil, fmt.Errorf("decode container inspect output: %w", err)
		}
		if container.ID == "" || container.Status.State == "" {
			return nil, fmt.Errorf("decode container inspect output: container id and status.state are required")
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// IPv4 returns the container's address on its first attached network,
// without the CIDR suffix.
func (c Container) IPv4() (string, error) {
	if len(c.Status.Networks) == 0 {
		return "", fmt.Errorf("container %s has no attached networks", c.ID)
	}
	prefix, err := netip.ParsePrefix(c.Status.Networks[0].IPv4Address)
	if err != nil {
		return "", fmt.Errorf("container %s: parse ipv4Address %q: %w", c.ID, c.Status.Networks[0].IPv4Address, err)
	}
	return prefix.Addr().String(), nil
}
