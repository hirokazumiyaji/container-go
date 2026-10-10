// Package inspect decodes the JSON emitted by `container ls --format
// json` and `container inspect`. Unknown fields are ignored so that
// additive CLI changes do not break decoding.
package inspect

import (
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/hirokazumiyaji/container-go/internal/strictjson"
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
	Platform       Platform          `json:"platform"`
	PublishedPorts []PublishedPort   `json:"publishedPorts"`
}

type Image struct {
	Reference string `json:"reference"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`

	// Presence bits distinguish an omitted field from an explicitly empty
	// field. The latter is still incomplete metadata, not a wildcard.
	OSPresent      bool `json:"-"`
	ArchPresent    bool `json:"-"`
	VariantPresent bool `json:"-"`
}

func (p *Platform) UnmarshalJSON(data []byte) error {
	type plain Platform
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Platform(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, p.OSPresent = fields["os"]
	_, p.ArchPresent = fields["architecture"]
	_, p.VariantPresent = fields["variant"]
	return nil
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

// containerFields are the entry fields Decode callers depend on to
// recognize a container and read its state. Nullable collections the CLI
// uses for empty maps and arrays (labels, publishedPorts, networks,
// maskedPaths) are deliberately absent: a null there is an empty value,
// not unreadable output.
var containerFields = []string{"id", "configuration", "status", "status.state"}

// Decode parses the JSON array output. Output that cannot be read is an
// error rather than a zero Container: an entry that decoded to nothing
// would be skipped as a non-match, reporting a missing container for
// output that never said whether the target exists.
func Decode(data []byte) ([]Container, error) {
	containers, err := strictjson.Array[Container](data, containerFields)
	if err != nil {
		return nil, fmt.Errorf("decode container inspect output: %w", err)
	}
	return containers, nil
}

// IPv4 returns the address from the first network in this Apple inspect
// model, without the CIDR suffix. Docker's multi-network selection is
// handled separately and is unspecified when no top-level address exists.
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
