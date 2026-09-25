// Package inspect decodes the JSON emitted by `container ls --format
// json` and `container inspect`. Unknown fields are ignored so that
// additive CLI changes do not break decoding.
package inspect

import (
	"encoding/json"
	"fmt"
	"net/netip"
)

// Container is one element of the array both commands emit.
type Container struct {
	ID            string        `json:"id"`
	Configuration Configuration `json:"configuration"`
	Status        Status        `json:"status"`
}

type Configuration struct {
	Image          Image             `json:"image"`
	Labels         map[string]string `json:"labels"`
	PublishedPorts []PublishedPort   `json:"publishedPorts"`
	// Platform is the platform selected when the container was created.
	// Older CLI versions may omit it; callers must treat a zero value as
	// unknown rather than silently treating it as the host platform.
	Platform Platform `json:"platform"`
}

// Platform is the OCI platform reported by Apple Container. It is kept
// separate from the image descriptor because the descriptor identifies the
// root index while this field identifies the selected variant.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

type Image struct {
	Reference  string     `json:"reference"`
	Descriptor Descriptor `json:"descriptor"`
	// VariantDigest is present in some Apple CLI versions for the selected
	// manifest. Older versions report only the root index descriptor.
	VariantDigest string `json:"variantDigest"`
}

// Descriptor is the OCI descriptor reported alongside an image
// reference. Older CLI versions may omit it, so callers must treat a
// zero digest as identity-unavailable rather than infer one.
type Descriptor struct {
	Digest string `json:"digest"`
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
	var containers []Container
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("decode container inspect output: %w", err)
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
