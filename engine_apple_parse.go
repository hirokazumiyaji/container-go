package container

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/domain"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

func (appleEngine) parseRunID([]byte) string { return "" }

func (appleEngine) parseInspect(data []byte, id string) (*engineInfo, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, newInspectTargetNotFound(id, "empty inspect output")
	}
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if strings.TrimSpace(c.ID) == "" && strings.TrimSpace(c.Status.State) != "" {
			return nil, fmt.Errorf("decode container inspect output: container id is required")
		}
	}
	var matchedMissingStatus bool
	for _, c := range containers {
		if !appleInspectTargetMatches(c, id) {
			continue
		}
		if strings.TrimSpace(c.Status.State) == "" {
			matchedMissingStatus = true
			continue
		}
		image := c.Configuration.Image.Reference
		imageDigest := c.Configuration.Image.Descriptor.Digest
		if image != "" && validImageDigest(imageDigest) {
			image = stripImageDigest(image) + "@" + imageDigest
		}
		platform := ""
		if p := c.Configuration.Platform; p.OS != "" || p.Architecture != "" || p.Variant != "" {
			candidate := p.OS + "/" + p.Architecture
			if p.Variant != "" {
				candidate += "/" + p.Variant
			}
			parsed, ok := parseApplePlatformSelector(candidate)
			if !ok {
				return nil, fmt.Errorf("container %s has invalid platform %q", id, candidate)
			}
			platform = formatApplePlatform(parsed)
		}
		cpus := 0.0
		cpusReported := false
		if configured := c.Configuration.Resources.CPUS; configured != nil {
			cpus = *configured
			cpusReported = true
		}
		info := &engineInfo{
			state:              appleState(c.Status.State),
			name:               c.ID,
			labels:             c.Configuration.Labels,
			image:              image,
			imageDigest:        imageDigest,
			imageVariantDigest: c.Configuration.Image.VariantDigest,
			runOptions: engineRunOptions{
				cpus:         cpus,
				cpusReported: cpusReported,
				capAdd:       c.Configuration.CapAdd,
				capDrop:      c.Configuration.CapDrop,
			},
		}
		if c.Configuration.ShmSize != nil {
			info.runOptions.shmSizeBytes = *c.Configuration.ShmSize
			info.runOptions.shmSizeReported = true
		}
		for _, limit := range c.Configuration.InitProcess.Rlimits {
			info.runOptions.ulimits = append(info.runOptions.ulimits, ulimitSpec{
				name: appleRlimitName(limit.Limit),
				soft: int64(limit.Soft),
				hard: int64(limit.Hard),
			})
		}
		pConfig := c.Configuration.Platform
		platformMeta := platformMetadataFromParts(
			pConfig.OS,
			pConfig.Architecture,
			pConfig.Variant,
			pConfig.OSPresent || pConfig.OS != "",
			pConfig.ArchPresent || pConfig.Architecture != "",
			pConfig.VariantPresent || pConfig.Variant != "",
		)
		if platform != "" {
			info.platform = platform
		} else {
			info.platform = platformMeta.normalized()
		}
		info.platformMeta = platformMeta
		if ip, err := c.IPv4(); err == nil {
			info.ip = ip
		}
		networkNames := make([]string, 0, len(c.Status.Networks))
		for _, network := range c.Status.Networks {
			if network.Network != "" {
				networkNames = append(networkNames, network.Network)
			}
		}
		sort.Strings(networkNames)
		info.networkNames = networkNames
		info.createdAt = parseInspectTime(c.Configuration.CreationDate)
		if c.Status.StartedDate != nil {
			info.startedAt = parseInspectTime(*c.Status.StartedDate)
		}
		if c.Status.ExitedAt != nil {
			info.finishedAt = parseInspectTime(*c.Status.ExitedAt)
		}
		if c.Status.ExitCode != nil {
			info.exitCode = *c.Status.ExitCode
			info.exitCodeReported = true
		}
		for _, p := range c.Configuration.PublishedPorts {
			info.bound = append(info.bound, boundPort{
				containerPort: p.ContainerPort,
				proto:         p.Proto,
				hostAddr:      canonicalIP(p.HostAddress),
				hostPort:      p.HostPort,
			})
		}
		return info, nil
	}
	if matchedMissingStatus {
		return nil, fmt.Errorf("decode container inspect output: container status.state is required")
	}
	return nil, newInspectTargetNotFound(id, "inspect output did not contain the requested target")
}

func appleInspectTargetMatches(c inspect.Container, target string) bool {
	target = strings.TrimPrefix(strings.TrimSpace(target), "/")
	if target == "" {
		return false
	}
	return strings.TrimPrefix(strings.TrimSpace(c.ID), "/") == target
}

// appleRlimitName maps Apple's inspected RLIMIT_* spelling to the lowercase
// name the public WithUlimit option uses.
func appleRlimitName(limit string) string {
	return strings.TrimPrefix(strings.ToLower(limit), "rlimit_")
}

func appleState(state string) State {
	switch state {
	case string(StateRunning):
		return StateRunning
	case string(StateStopped):
		return StateStopped
	case string(StateStopping):
		return StateStopping
	case string(StateCreated):
		return StateCreated
	default:
		return StateUnknown
	}
}

// parseStoppedManaged filters client-side: the Apple CLI exposes no
// label or status filter.
func (appleEngine) parseStoppedManaged(data []byte) ([]string, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		if c.Configuration.Labels[managedLabel] == "true" && c.Status.State == string(StateStopped) {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

type appleImageDescriptor struct {
	Digest      string            `json:"digest"`
	MediaType   string            `json:"mediaType"`
	Annotations map[string]string `json:"annotations"`
}

type appleImageVariant struct {
	Digest   string `json:"digest"`
	Platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type appleImageInspectRecord struct {
	ID            string               `json:"id"`
	Reference     string               `json:"reference"`
	Name          string               `json:"name"`
	Descriptor    appleImageDescriptor `json:"descriptor"`
	Configuration struct {
		Name       string               `json:"name"`
		Reference  string               `json:"reference"`
		Descriptor appleImageDescriptor `json:"descriptor"`
		Platform   struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
		Image struct {
			Reference  string               `json:"reference"`
			Descriptor appleImageDescriptor `json:"descriptor"`
		} `json:"image"`
	} `json:"configuration"`
	Variants []appleImageVariant `json:"variants"`
}

// parseImageIdentity reads the root index descriptor exposed by current
// Apple Container releases and also accepts the older flat
// ImageDescription shape. When a platform is selected, the variant is
// validated separately while the root descriptor remains the run and
// reuse identity.
func (appleEngine) parseImageIdentity(data []byte, image, platform string) imageLookupResult {
	var records []appleImageInspectRecord
	if err := json.Unmarshal(data, &records); err != nil {
		// A successful CLI invocation with malformed JSON is not proof
		// that the image is absent. Treat it as an identity-unavailable
		// result so PullMissing cannot turn a parser failure into a fetch,
		// and mark it unreadable so imageExists does not report the image
		// as present.
		return unreadableImage()
	}
	if len(records) == 0 {
		return absentImage()
	}
	record := records[0]
	if platform == "" && (record.Configuration.Platform.OS != "" || record.Configuration.Platform.Architecture != "" || record.Configuration.Platform.Variant != "") {
		candidate := record.Configuration.Platform.OS + "/" + record.Configuration.Platform.Architecture
		if record.Configuration.Platform.Variant != "" {
			candidate += "/" + record.Configuration.Platform.Variant
		}
		parsed, ok := parseApplePlatformSelector(candidate)
		if !ok {
			return availableImage(imageIdentity{})
		}
		platform = formatApplePlatform(parsed)
	}
	rootDigest, rootOK := appleRootDescriptorDigest(record)
	variantDigest := ""
	if platform != "" {
		if len(record.Variants) == 0 && !rootOK {
			if len(record.ID) == 64 && domain.IsHex(record.ID) {
				// An old ID-only record proves that some local content
				// exists, but it cannot prove that the host variant is
				// addressable by a digest reference.
				return notLocalImage(imageIdentity{platform: platform}, "Apple image inspect has no platform variant metadata")
			}
			// Without a usable root descriptor or a local ID, the record
			// is identity-unavailable rather than evidence that a requested
			// platform is absent from the local store.
			return availableImage(imageIdentity{})
		}
		var variantOK bool
		variantDigest, variantOK = applePlatformVariantDigest(record, platform)
		if !variantOK {
			return notLocalImage(imageIdentity{platform: platform}, "Apple image inspect has no complete matching platform variant")
		}
		if !rootOK {
			// A selected variant is not a substitute for the root
			// index identity used by run and reuse.
			return availableImage(imageIdentity{})
		}
	}
	if !rootOK && appleDescriptorPresent(record) {
		// A malformed descriptor is not equivalent to an older
		// descriptor-less response. Do not fall back to an ID when the
		// backend supplied an unusable descriptor field.
		return availableImage(imageIdentity{})
	}
	return finishAppleImageIdentity(image, record, rootDigest, platform, variantDigest)
}

// appleRootDescriptorDigest returns the index/root descriptor digest,
// rejecting conflicting or malformed descriptor fields.
func appleRootDescriptorDigest(record appleImageInspectRecord) (string, bool) {
	var digest string
	for _, candidate := range []string{
		record.Configuration.Descriptor.Digest,
		record.Configuration.Image.Descriptor.Digest,
		record.Descriptor.Digest,
	} {
		if candidate == "" {
			continue
		}
		if !validImageDigest(candidate) {
			return "", false
		}
		if digest != "" && !strings.EqualFold(digest, candidate) {
			return "", false
		}
		digest = candidate
	}
	return digest, digest != ""
}

func appleDescriptorPresent(record appleImageInspectRecord) bool {
	return record.Configuration.Descriptor.Digest != "" ||
		record.Configuration.Image.Descriptor.Digest != "" ||
		record.Descriptor.Digest != ""
}

// applePlatformVariantDigest validates every listed variant before
// selecting one. An empty or incomplete list is not local evidence for
// an explicit platform request.
func applePlatformVariantDigest(record appleImageInspectRecord, platform string) (string, bool) {
	if len(record.Variants) == 0 {
		return "", false
	}
	want, ok := parseApplePlatformSelector(platform)
	if !ok {
		return "", false
	}
	selected := ""
	for _, variant := range record.Variants {
		if variant.Platform.OS == "" || variant.Platform.Architecture == "" || !validImageDigest(variant.Digest) {
			return "", false
		}
		have := canonicalApplePlatform(variant.Platform.OS, variant.Platform.Architecture, variant.Platform.Variant)
		if applePlatformsEqual(want, have) {
			if selected != "" && !strings.EqualFold(selected, variant.Digest) {
				return "", false
			}
			selected = variant.Digest
		}
	}
	return selected, selected != ""
}

func finishAppleImageIdentity(requested string, record appleImageInspectRecord, digest, platform, variantDigest string) imageLookupResult {
	reference := record.Configuration.Name
	if reference == "" {
		reference = record.Configuration.Reference
	}
	if reference == "" {
		reference = record.Configuration.Image.Reference
	}
	if reference == "" {
		reference = record.Reference
	}
	if reference == "" {
		reference = record.Name
	}

	// A bare sha256:... is Docker's image-ID syntax, not an Apple
	// repository reference. It is safe to handle only when the inspected
	// Apple record proves the same identity through its descriptor and
	// gives us a repository base for the normalized run reference.
	if isImageID(requested) || isBareImageID(requested) {
		if imageReferenceBase(reference) == "" || !validImageDigest(digest) {
			return availableImage(imageIdentity{})
		}
		if !appleImageIDMatchesDescriptor(requested, record.ID, digest) {
			return mismatchedImage()
		}
	} else if requested != "" && reference != "" && !appleImageReferencesCompatible(requested, reference) {
		return mismatchedImage()
	}

	if validImageDigest(digest) {
		// The descriptor is authoritative. Do not copy an ID-shaped field
		// into imageIdentity.id: that field is a Docker-only local-ID ABI.
		runReference := appleRunReferenceBase(requested, reference)
		identity := imageReferenceWithDigest(runReference, reference, digest, "")
		identity.rootDigest = digest
		identity.platform = platform
		identity.variantDigest = variantDigest
		identity.repository = imageRepository(runReference)
		identity.appleSynthetic = appleSyntheticIndex(record, digest, variantDigest)
		// A caller may pin the actual manifest of a single-manifest image.
		// That digest is registry-addressable even though Apple's synthetic
		// root index digest is not, so retain the caller's tag plus digest.
		if requestedDigest := imageDigest(requested); validImageDigest(requestedDigest) && validImageDigest(variantDigest) && strings.EqualFold(requestedDigest, variantDigest) {
			if base := imageReferenceBase(requested); base != "" {
				identity.reference = base + "@" + variantDigest
				identity.appleSynthetic = false
			}
		}
		return availableImage(identity)
	}
	if isImageID(record.ID) {
		// Apple must not treat a Docker-style ID field as an immutable
		// run target without a descriptor proving what that ID names.
		return availableImage(imageIdentity{})
	}
	if len(record.ID) == 64 && domain.IsHex(record.ID) {
		// Older Apple image-inspect responses exposed the local content ID
		// without a descriptor. Keep the compatibility fallback, but do
		// not classify it as a Docker image ID.
		digest := "sha256:" + record.ID
		identity := imageReferenceWithDigest(appleRunReferenceBase(requested, reference), reference, digest, "")
		identity.rootDigest = digest
		identity.platform = platform
		identity.variantDigest = variantDigest
		identity.repository = imageRepository(identity.reference)
		return availableImage(identity)
	}
	return availableImage(imageIdentity{})
}

// appleSyntheticIndex identifies Apple's explicitly annotated local index
// wrapper. A one-entry OCI index is not, by itself, evidence of a
// synthetic root: platform-scoped inspect responses can legitimately expose
// only one manifest from a multi-platform index. Callers use the exact-root
// probe when this reliable signal is absent.
func appleSyntheticIndex(record appleImageInspectRecord, _, _ string) bool {
	for _, descriptor := range []appleImageDescriptor{
		record.Configuration.Descriptor,
		record.Configuration.Image.Descriptor,
		record.Descriptor,
	} {
		value, ok := descriptor.Annotations["com.apple.containerization.index.indirect"]
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "true") || value == "1" {
			return true
		}
	}
	return false
}

// appleRunReferenceBase preserves a backend's canonical custom-registry
// name when the caller used an unqualified reference. Docker Hub's familiar
// short form remains unchanged for compatibility.
func appleRunReferenceBase(requested, reported string) string {
	requestedBase := imageReferenceBase(requested)
	reportedBase := imageReferenceBase(reported)
	if requestedBase == "" {
		return reportedBase
	}
	if isUnqualifiedImageReference(requestedBase) && reportedBase != "" && !isDockerRegistryReference(reportedBase) {
		return reportedBase
	}
	return requestedBase
}

func appleImageReferencesCompatible(requested, actual string) bool {
	requestedDigest := imageDigest(requested)
	actualDigest := imageDigest(actual)
	if requestedDigest != "" && !validImageDigest(requestedDigest) {
		return false
	}
	if actualDigest != "" && !validImageDigest(actualDigest) {
		return false
	}
	if imagesCompatible(requested, actual) {
		return true
	}
	// A custom default registry is reported canonically by Apple, while the
	// caller may have supplied an unqualified name. When a digest is
	// present, the tag is only a mutable alias spelling: content and
	// repository provenance, not the tag, determine compatibility. If both
	// sides carry digests, their claims must agree; a descriptor-backed
	// identity is checked separately against the selected root/variant.
	if requestedDigest != "" && actualDigest != "" && !strings.EqualFold(requestedDigest, actualDigest) {
		return false
	}
	if requestedDigest != "" || actualDigest != "" {
		return imageRepositoryPathsCompatibleIgnoringTags(requested, actual)
	}
	return imageRepositoryPathsCompatible(requested, actual)
}

func imageRepositoryPathsCompatibleIgnoringTags(a, b string) bool {
	abase := imageRepositoryBaseWithoutTag(imageReferenceBase(a))
	bbase := imageRepositoryBaseWithoutTag(imageReferenceBase(b))
	return imageRepositoryPathsCompatible(abase, bbase)
}

// appleImageIDMatchesDescriptor verifies the only safe interpretation of
// an ID-shaped value supplied to Apple: the descriptor must independently
// report the same digest, and any non-empty Apple ID field must agree.
func appleImageIDMatchesDescriptor(requested, recordID, digest string) bool {
	requestedDigest := requested
	if isBareImageID(requested) {
		requestedDigest = "sha256:" + requested
	}
	if (!isImageID(requested) && !isBareImageID(requested)) || !validImageDigest(digest) || !strings.EqualFold(digest, requestedDigest) {
		return false
	}
	if recordID == "" {
		return true
	}
	if strings.HasPrefix(recordID, "sha256:") {
		return isImageID(recordID) && strings.EqualFold(recordID, digest)
	}
	return len(recordID) == 64 && domain.IsHex(recordID) && strings.EqualFold("sha256:"+recordID, digest)
}

func (appleEngine) parseReuseGroupIDs(data []byte, group string) ([]string, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		if c.Configuration.Labels[reuseGroupLabel] == group {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}
