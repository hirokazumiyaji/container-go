package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/internal/strictjson"
)

func (dockerEngine) parseRunID(stdout []byte) string {
	id := strings.TrimSpace(string(stdout))
	if !domain.IsImmutableContainerID(id) {
		return ""
	}
	return id
}

// dockerImageInspect mirrors the fields of `docker image inspect`
// output needed to pin the image that will be run. RepoDigests is the
// registry digest; Id is the immutable local image ID fallback.
type dockerImageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

// dockerInspect mirrors the fields of `docker inspect` output this
// library reads. Unknown fields are ignored.
type dockerInspect struct {
	ID       string `json:"Id"`
	Image    string `json:"Image"`
	Name     string `json:"Name"`
	Platform string `json:"Platform"`
	Created  string `json:"Created"`
	// State is present on container inspect objects, but not on Docker
	// network or volume inspect objects.
	State *struct {
		Status     string `json:"Status"`
		ExitCode   *int   `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		// Health is null when the image defines no HEALTHCHECK.
		Health *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
		} `json:"Health"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode string   `json:"NetworkMode"`
		NanoCpus    *int64   `json:"NanoCpus"`
		ShmSize     *int64   `json:"ShmSize"`
		Privileged  bool     `json:"Privileged"`
		CapAdd      []string `json:"CapAdd"`
		CapDrop     []string `json:"CapDrop"`
		Ulimits     []struct {
			Name string `json:"Name"`
			Soft int64  `json:"Soft"`
			Hard int64  `json:"Hard"`
		} `json:"Ulimits"`
		ExtraHosts []string `json:"ExtraHosts"`
	} `json:"HostConfig"`
	Config struct {
		Image    string            `json:"Image"`
		Hostname string            `json:"Hostname"`
		Labels   map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Ports     map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]struct {
			IPAddress string   `json:"IPAddress"`
			Aliases   []string `json:"Aliases"`
			DNSNames  []string `json:"DNSNames"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// dockerInspectFields are the entry fields the matcher below depends on to
// recognize the requested container and its state. Nullable collections
// the CLI uses for empty values (Config.Labels, NetworkSettings.Ports,
// which real output emits as null) are deliberately absent: a null there
// is an empty value, not unreadable output.
var dockerInspectFields = []string{"Id", "Name", "State", "State.Status"}

// parseInspect returns the one entry that matches target exactly: the full
// container ID for an ID target, or the slash-prefixed name for a logical
// name. Other entries are ignored, and output the parser cannot read is an
// error rather than a missing container.
func (dockerEngine) parseInspect(data []byte, target string) (*engineInfo, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, newInspectTargetNotFound(target, "empty inspect output")
	}
	// Decode entry by entry so an entry this parser cannot interpret is
	// reported as a schema failure instead of a zero value. A target-naming
	// entry that decoded to nothing would be classified ErrContainerNotFound
	// even though the container exists, which lets a delete that verified
	// nothing pass as a removal that happened.
	containers, err := strictjson.Array[dockerInspect](data, dockerInspectFields)
	if err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(containers) == 0 {
		return nil, newInspectTargetNotFound(target, "inspect output did not contain the requested target")
	}
	match := -1
	var malformedID string
	var malformedIDSeen bool
	var candidateMissingState bool
	if domain.IsImmutableContainerID(target) {
		for i, c := range containers {
			if domain.IsImmutableContainerID(c.ID) && c.ID == target {
				if c.State == nil || strings.TrimSpace(c.State.Status) == "" {
					candidateMissingState = true
					continue
				}
				match = i
				break
			}
		}
	} else {
		for i, c := range containers {
			if target == "" || c.Name != "/"+target {
				continue
			}
			// A name-matched entry without a container ID is unreadable,
			// not absent: skipping it would report a container that exists
			// as missing. An empty ID is as invalid as any other
			// non-canonical one.
			if !domain.IsImmutableContainerID(c.ID) {
				if !malformedIDSeen {
					malformedID = c.ID
					malformedIDSeen = true
				}
				continue
			}
			// Apply the same State/Status validation as ID addressing.
			// Otherwise a target-naming entry whose state cannot be read
			// would be accepted as a match or reported as absent.
			if c.State == nil || strings.TrimSpace(c.State.Status) == "" {
				candidateMissingState = true
				continue
			}
			match = i
			break
		}
	}
	if match < 0 {
		if candidateMissingState {
			return nil, fmt.Errorf("decode docker inspect output: container State.Status is required")
		}
		if malformedIDSeen {
			return nil, fmt.Errorf("decode docker inspect output: container %s has invalid ID %q", target, malformedID)
		}
		return nil, newInspectTargetNotFound(target, "inspect output did not contain the requested target")
	}
	c := containers[match]

	networkNames := make([]string, 0, len(c.NetworkSettings.Networks))
	for name := range c.NetworkSettings.Networks {
		networkNames = append(networkNames, name)
	}
	sort.Strings(networkNames)

	var networkAliases []string
	var networkAliasMap map[string][]string
	for name, endpoint := range c.NetworkSettings.Networks {
		aliases := append([]string(nil), endpoint.Aliases...)
		aliases = append(aliases, endpoint.DNSNames...)
		if len(aliases) == 0 {
			continue
		}
		sort.Strings(aliases)
		if networkAliasMap == nil {
			networkAliasMap = make(map[string][]string, len(c.NetworkSettings.Networks))
		}
		networkAliasMap[name] = aliases
	}
	endpoint := c.NetworkSettings.Networks[c.HostConfig.NetworkMode]
	networkAliases = append(networkAliases, endpoint.Aliases...)
	networkAliases = append(networkAliases, endpoint.DNSNames...)
	sort.Strings(networkAliases)

	info := &engineInfo{
		state:           dockerState(c.State.Status),
		name:            strings.TrimPrefix(c.Name, "/"),
		labels:          c.Config.Labels,
		uid:             c.ID,
		image:           c.Config.Image,
		imageID:         c.Image,
		imageDigest:     imageDigest(c.Config.Image),
		platform:        c.Platform,
		platformMeta:    platformMetadataFromString(c.Platform),
		ip:              c.NetworkSettings.IPAddress,
		networkMode:     c.HostConfig.NetworkMode,
		networkNames:    networkNames,
		networkAliases:  networkAliases,
		networkAliasMap: networkAliasMap,
	}
	info.runOptions = engineRunOptions{
		privileged: c.HostConfig.Privileged,
		capAdd:     c.HostConfig.CapAdd,
		capDrop:    c.HostConfig.CapDrop,
		hostname:   c.Config.Hostname,
	}
	if c.HostConfig.NanoCpus != nil {
		info.runOptions.cpus = float64(*c.HostConfig.NanoCpus) / 1e9
		info.runOptions.cpusReported = true
	}
	if c.HostConfig.ShmSize != nil {
		info.runOptions.shmSizeBytes = uint64(*c.HostConfig.ShmSize)
		info.runOptions.shmSizeReported = true
	}
	for _, limit := range c.HostConfig.Ulimits {
		info.runOptions.ulimits = append(info.runOptions.ulimits, ulimitSpec{
			name: strings.ToLower(limit.Name),
			soft: limit.Soft,
			hard: limit.Hard,
		})
	}
	info.runOptions.extraHosts = parseDockerExtraHosts(c.HostConfig.ExtraHosts)
	info.createdAt = parseInspectTime(c.Created)
	if c.State != nil {
		if c.State.ExitCode != nil {
			info.exitCode = *c.State.ExitCode
			info.exitCodeReported = true
		}
		info.startedAt = parseInspectTime(c.State.StartedAt)
		info.finishedAt = parseInspectTime(c.State.FinishedAt)
		if c.State.Health != nil {
			info.health = c.State.Health.Status
			info.healthFailingStreak = c.State.Health.FailingStreak
		}
	}
	if info.networkMode == "" {
		// HostConfig.NetworkMode is present in current Docker inspect
		// output. The network map fallback keeps endpoint validation
		// meaningful for older CLI versions that omitted HostConfig;
		// some Docker versions leave it empty for user-defined networks.
		if _, ok := c.NetworkSettings.Networks[dockerNetworkHost]; ok {
			info.networkMode = dockerNetworkHost
		} else if _, ok := c.NetworkSettings.Networks[dockerNetworkNone]; ok {
			info.networkMode = dockerNetworkNone
		} else if len(c.NetworkSettings.Networks) == 1 {
			for name := range c.NetworkSettings.Networks {
				info.networkMode = name
			}
		}
	}
	if info.ip == "" {
		for _, name := range networkNames {
			if ip := c.NetworkSettings.Networks[name].IPAddress; ip != "" {
				info.ip = ip
				break
			}
		}
	}
	for portProto, bindings := range c.NetworkSettings.Ports {
		spec, err := parsePortSpec(portProto)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			hostPort, err := strconv.Atoi(b.HostPort)
			if err != nil {
				continue
			}
			info.bound = append(info.bound, boundPort{
				containerPort: spec.Port,
				proto:         spec.Proto,
				hostAddr:      canonicalIP(b.HostIP),
				hostPort:      hostPort,
			})
		}
	}
	return info, nil
}

type dockerNetworkInfo struct {
	Name     string
	Driver   string
	Internal bool
	Options  map[string]string
}

func parseDockerNetworkInspect(data []byte, requested string) (dockerNetworkInfo, error) {
	if requested == "" {
		return dockerNetworkInfo{}, errors.New("docker network inspect requires a non-empty network name")
	}
	var networks []dockerNetworkInfo
	if err := json.Unmarshal(data, &networks); err != nil {
		return dockerNetworkInfo{}, fmt.Errorf("decode Docker network inspect output: %w", err)
	}
	var match *dockerNetworkInfo
	for i := range networks {
		network := &networks[i]
		if network.Name != requested {
			continue
		}
		if match != nil {
			return dockerNetworkInfo{}, fmt.Errorf("docker network inspect returned duplicate entries for %q", requested)
		}
		match = network
	}
	if match == nil {
		return dockerNetworkInfo{}, fmt.Errorf("docker network %q missing from inspect output", requested)
	}
	if strings.TrimSpace(match.Driver) == "" {
		return dockerNetworkInfo{}, fmt.Errorf("docker network %q inspect returned no driver", requested)
	}
	return *match, nil
}

// parseDockerExtraHosts converts Docker's inspect form (host:ip, host=ip, or a
// bracketed IPv6 address) into the backend-neutral extraHostSpec so reuse can
// compare requested and reported entries. strings.Cut splits only at the first
// separator and returns the whole remainder, so an unbracketed IPv6 address
// such as "host:2001:db8::1" keeps its remaining colons. Bracketed and
// unbracketed spellings are both normalized to net.IP's canonical form; Docker
// inspect itself reports the unbracketed form.
func parseDockerExtraHosts(entries []string) []extraHostSpec {
	hosts := make([]extraHostSpec, 0, len(entries))
	for _, entry := range entries {
		host, ip, ok := strings.Cut(entry, "=")
		if !ok {
			host, ip, ok = strings.Cut(entry, ":")
		}
		if !ok || host == "" || ip == "" {
			continue
		}
		if ip != "host-gateway" {
			if parsed := net.ParseIP(strings.Trim(ip, "[]")); parsed != nil {
				ip = parsed.String()
			}
		}
		hosts = append(hosts, extraHostSpec{host: host, ip: ip})
	}
	return hosts
}

// dockerState maps Docker's status vocabulary onto State.
func dockerState(s string) State {
	switch s {
	case "running":
		return StateRunning
	case "created":
		return StateCreated
	case "exited", "dead":
		return StateStopped
	case "restarting":
		// Docker can move a restarting container back to running; keep
		// it distinct so startup readiness retries instead of failing
		// fast as stopped.
		return StateRestarting
	case "removing":
		// A removing container cannot become ready; readiness treats the
		// backend removal transition as terminal.
		return StateStopping
	case "paused":
		// A paused container is stable but not executing; wait treats
		// it as terminal rather than misreporting it as stopped.
		return StatePaused
	default:
		return StateUnknown
	}
}

func (dockerEngine) parseStoppedManaged(data []byte) ([]string, error) {
	return parseDockerPruneIDs(data)
}

func parseDockerPruneIDs(data []byte) ([]string, error) {
	ids := splitNonEmptyLines(data)
	for _, id := range ids {
		if !domain.IsImmutableContainerID(id) {
			return nil, fmt.Errorf("docker ps returned invalid container ID %q", id)
		}
	}
	return ids, nil
}

// parseImageIdentity prefers a registry digest because it remains tied
// to the repository the caller requested, and falls back to Docker's
// local image ID for locally built or otherwise digest-less images. It
// accepts both the canonical sha256:... ID and Docker's 64-hex spelling
// only after the inspected Id matches exactly. Both forms prevent a
// later tag reassignment from changing run's target.
func (dockerEngine) parseImageIdentity(data []byte, image, _ string) imageLookupResult {
	var images []dockerImageInspect
	if err := json.Unmarshal(data, &images); err != nil {
		// A successful CLI invocation with malformed JSON is not proof
		// that the image is absent. Treat it as identity-unavailable so
		// PullMissing cannot turn a parser failure into a fetch, and mark
		// it unreadable so imageExists does not report the image as
		// present.
		return unreadableImage()
	}
	if len(images) == 0 {
		return absentImage()
	}
	requestedID, requestedIsID := canonicalDockerImageID(image)
	if image != "" && isBareImageReference(image) && !requestedIsID {
		// A malformed or unprefixed digest/ID-shaped value is not a
		// Docker image-ID request that inspect can verify.
		return availableImage(imageIdentity{})
	}
	explicitPinned := requestedIsID || (image != "" && validImageDigest(imageDigest(image)))
	sawExplicitConflict := false
	sawDifferentID := false
	fallbackID := ""
	for _, img := range images {
		inspectedID, inspectedIDOK := canonicalDockerImageID(img.ID)
		// An ID-shaped request is only accepted when inspect reports that
		// exact verified local ID. A different, well-formed Docker ID is a
		// known mismatch rather than an unavailable identity.
		if requestedIsID {
			if inspectedIDOK && strings.EqualFold(inspectedID, requestedID) {
				identity := imageReferenceWithDigest(requestedID, "", "", inspectedID)
				identity.rootDigest = ""
				return availableImage(identity)
			}
			if inspectedIDOK {
				sawDifferentID = true
			}
			continue
		}
		for _, repoDigest := range img.RepoDigests {
			digest := imageDigest(repoDigest)
			if !validImageDigest(digest) || imageReferenceBase(repoDigest) == "" {
				continue
			}
			if image != "" {
				requestedDigest := imageDigest(image)
				if requestedDigest != "" {
					// An explicit digest is a claim about both the
					// manifest and its repository. A foreign RepoDigest
					// is a conflict, even when its digest happens to be
					// the same.
					if !strings.EqualFold(requestedDigest, digest) || imageRepository(image) != imageRepository(repoDigest) {
						sawExplicitConflict = true
						continue
					}
				} else if !imagesCompatible(image, repoDigest) && imageRepository(image) != imageRepository(repoDigest) {
					// Docker's RepoDigests commonly omit the tag. In
					// that form the repository still has to match.
					continue
				}
			}
			identityID := ""
			if inspectedIDOK {
				identityID = inspectedID
			}
			identity := imageReferenceWithDigest(image, repoDigest, digest, identityID)
			identity.repository = imageRepository(repoDigest)
			return availableImage(identity)
		}
		if inspectedIDOK {
			// A tag can be a local alias whose RepoDigests came from a
			// different registry. The inspected local ID is still the
			// safe immutable target for that alias. An explicit pinned
			// request remains a conflict when a RepoDigest contradicted
			// it, even if the local content ID is available.
			if !explicitPinned {
				identity := imageReferenceWithDigest(image, "", "", inspectedID)
				identity.repository = imageRepository(image)
				return availableImage(identity)
			}
			if fallbackID == "" {
				fallbackID = inspectedID
			}
		}
		// A non-empty Docker inspect array proves existence even when
		// this old/versioned response has no usable identity fields.
	}
	if explicitPinned {
		if sawExplicitConflict || sawDifferentID {
			return mismatchedImage()
		}
		if fallbackID != "" {
			identity := imageReferenceWithDigest(image, "", "", fallbackID)
			identity.repository = imageRepository(image)
			return availableImage(identity)
		}
	}
	return availableImage(imageIdentity{})
}

func (dockerEngine) parseReuseGroupIDs(data []byte, _ string) ([]string, error) {
	return parseDockerPruneIDs(data)
}
