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
	PublishedPorts []PublishedPort   `json:"publishedPorts"`
	// Platform is the platform selected when the container was created.
	// Older CLI versions may omit it; callers must treat a zero value as
	// unknown rather than silently treating it as the host platform.
	Platform Platform `json:"platform"`
	// CreationDate is the timestamp the backend reported for creation.
	// Omitting the field and reporting an empty value are equivalent: both
	// mean the time is unknown.
	CreationDate string `json:"creationDate"`
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
	State       string    `json:"state"`
	Networks    []Network `json:"networks"`
	StartedDate *string   `json:"startedDate"`
	ExitedAt    *string   `json:"exitedAt"`
	ExitCode    *int      `json:"exitCode"`
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
