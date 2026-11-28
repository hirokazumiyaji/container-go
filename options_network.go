package container

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// WithExposedPorts declares the container ports ("6379/tcp" or "6379")
// that MappedPort and Endpoint may resolve. Docker auto-publishes these
// ports; host, none, internal, and isolated networks reject that
// combination before container creation.
func WithExposedPorts(ports ...string) Option {
	return func(c *config) error {
		specs := make([]portSpec, 0, len(ports))
		for _, p := range ports {
			spec, err := domain.ParsePortSpec(p)
			if err != nil {
				return newValidationError("WithExposedPorts", p, err)
			}
			specs = append(specs, spec)
		}
		c.exposed = append(c.exposed, specs...)
		return nil
	}
}

// WithPublishedPort publishes a container port on the host
// WithPublishedPort publishes a container port on the host
// ("[host-ip:]host-port:container-port[/proto]"). On Apple Container,
// endpoints resolve to the container's own IP when this is omitted; on
// Docker, WithExposedPorts auto-publishes instead. Docker rejects both
// publish forms on host, none, internal, and isolated networks.
// Publishing a port does not reorder WithExposedPorts declarations used by
// implicit wait probes.
func WithPublishedPort(spec string) Option {
	return func(c *config) error {
		ps, err := parsePublishSpec(spec)
		if err != nil {
			return newValidationError("WithPublishedPort", spec, err)
		}
		c.published = append(c.published, ps)
		return nil
	}
}

// hostnameRE is RFC 1123's DNS-name grammar: dot-separated labels of letters,
// digits, and interior hyphens. A leading dash is rejected so a value can never
// be parsed as a CLI flag.
var hostnameRE = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

const (
	maxHostnameBytes = 253
	// maxContainerHostnameBytes follows Linux's UTS nodename limit (64
	// bytes including the terminator).
	maxContainerHostnameBytes = 63
)

func validateHostname(option, field, name string, limit int) error {
	if name == "" {
		return newValidationErrorWithField(option, field, name, fmt.Errorf("hostname must not be empty"))
	}
	if len(name) > limit {
		return newValidationErrorWithField(option, field, name,
			fmt.Errorf("hostname %q exceeds the %d-byte maximum", name, limit))
	}
	if !hostnameRE.MatchString(name) {
		return newValidationErrorWithField(option, field, name, fmt.Errorf("invalid hostname %q", name))
	}
	return nil
}

// extraHostSpec is one --add-host entry.
type extraHostSpec struct {
	host string
	ip   string
}

func (h extraHostSpec) arg() string {
	return h.host + ":" + h.ip
}

// WithExtraHost adds an /etc/hosts entry mapping host to ip (Docker's
// --add-host). The IP may be the literal host-gateway. Host names use the
// RFC 1123 hostname grammar, and a duplicate or conflicting host is rejected.
// Apple Container has no extra-host capability and rejects it with
// ErrUnsupportedCapability before invoking its CLI.
func WithExtraHost(host, ip string) Option {
	return func(c *config) error {
		if err := validateHostname("WithExtraHost", "host", host, maxHostnameBytes); err != nil {
			return err
		}
		canonical := ip
		if ip != "host-gateway" {
			parsed := net.ParseIP(ip)
			if parsed == nil {
				return newValidationErrorWithField("WithExtraHost", "ip", ip,
					fmt.Errorf("invalid IP address %q", ip))
			}
			canonical = parsed.String()
		}
		for _, existing := range c.extraHosts {
			if !strings.EqualFold(existing.host, host) {
				continue
			}
			if existing.ip == canonical {
				return newValidationErrorWithField("WithExtraHost", "host", host,
					fmt.Errorf("duplicate extra host %q", host))
			}
			return newValidationErrorWithField("WithExtraHost", "host", host,
				fmt.Errorf("conflicting extra host %q: %q and %q", host, existing.ip, canonical))
		}
		c.extraHosts = append(c.extraHosts, extraHostSpec{host: host, ip: canonical})
		return nil
	}
}

// WithHostname sets the container hostname (Docker's --hostname). The name uses
// RFC 1123's hostname grammar and must fit Linux's 63-byte UTS nodename limit.
// Apple Container has no --hostname flag and rejects it with
// ErrUnsupportedCapability before invoking its CLI.
func WithHostname(name string) Option {
	return func(c *config) error {
		if err := validateHostname("WithHostname", "hostname", name, maxContainerHostnameBytes); err != nil {
			return err
		}
		c.hostname = name
		return nil
	}
}

// WithNetwork selects a network. Omitting it leaves Docker's
// daemon-selected default unchanged (bridge on Linux and nat on native
// Windows). Docker's "host" and "none" modes and externally isolated
// networks cannot be combined with WithExposedPorts or WithPublishedPort;
// the Docker backend rejects those combinations before creating the
// container.
func WithNetwork(name string) Option {
	return func(c *config) error {
		if !networkNameRE.MatchString(name) {
			return validationErrorf("WithNetwork", name, "invalid network name %q", name)
		}
		c.network = name
		c.networkExplicit = true
		return nil
	}
}

// WithNetworkAliases adds network-scoped DNS aliases for the container. Docker
// passes each alias as --network-alias. Every alias must be a DNS label of
// 1-63 letters, digits, or hyphens with an alphanumeric first and last
// character; duplicates are rejected. Aliases require an explicitly selected
// user-defined WithNetwork: Docker's default bridge (and its Windows "nat"
// equivalent), host, and none networks do not resolve network-scoped aliases.
// Apple Container has no network-alias capability, so Run returns
// ErrUnsupportedCapability before any backend operation.
func WithNetworkAliases(aliases ...string) Option {
	return func(c *config) error {
		seen := make(map[string]struct{}, len(c.networkAliases)+len(aliases))
		for _, alias := range c.networkAliases {
			seen[alias] = struct{}{}
		}
		for _, alias := range aliases {
			if err := validateNetworkAlias(alias); err != nil {
				return err
			}
			if _, exists := seen[alias]; exists {
				return newValidationErrorWithField("WithNetworkAliases", "alias", alias,
					fmt.Errorf("duplicate network alias %q", alias))
			}
			seen[alias] = struct{}{}
		}
		c.networkAliases = append(c.networkAliases, aliases...)
		return nil
	}
}

func validateNetworkAlias(alias string) error {
	if alias == "" {
		return newValidationErrorWithField("WithNetworkAliases", "alias", alias,
			fmt.Errorf("network alias must not be empty"))
	}
	if !networkAliasRE.MatchString(alias) {
		return newValidationErrorWithField("WithNetworkAliases", "alias", alias,
			fmt.Errorf("invalid network alias %q", alias))
	}
	return nil
}

// portSpec is the domain port value object. The alias keeps the root
// package's unexported spelling while sharing one type with wait and the
// backends, so a parsed specification is never copied into a parallel struct.
type portSpec = domain.PortSpec

// publishSpec is the domain publish value object. Its HostAddr is already
// parsed, so resolving a connect address does not re-parse the caller's
// string.
type publishSpec = domain.PublishSpec

func parsePublishSpec(s string) (publishSpec, error) {
	spec, err := domain.ParsePublishSpec(s)
	if err != nil {
		if field, ok := domain.FieldOf(err); ok {
			return publishSpec{}, invalidOption(field, err.Error())
		}
		return publishSpec{}, err
	}
	return spec, nil
}
