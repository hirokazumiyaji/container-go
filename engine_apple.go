package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

// appleEngine drives Apple Container's `container` CLI.
type appleEngine struct{}

// Verified against Apple Container CLI 1.2.x–1.3.x (local: 1.3.0).
// Stderr substrings below are matched case-insensitively on CLIError.Stderr.
// Sources (apple/container):
//   - name conflict: ContainerRun.swift throws ContainerizationError(.exists,
//     message: "container with id \(id) already exists")
//   - image missing / container missing: ContainerizationError(.notFound)
//     surfaces as "image not found: …" / "container not found: …"
const (
	appleStderrAlready   = "already"
	appleStderrExist     = "exist"
	appleStderrInUse     = "in use"
	appleStderrTaken     = "taken"
	appleStderrNotFound  = "not found"
	appleStderrNoSuchObj = "no such object"    // defensive; not observed on 1.3.0
	appleStderrNoSuchCtr = "no such container" // defensive; not observed on 1.3.0
)

func (appleEngine) name() string   { return "apple" }
func (appleEngine) binary() string { return "container" }
func (appleEngine) directIP() bool { return true }

func (appleEngine) defaultHost() string { return "127.0.0.1" }

func (appleEngine) probe() cli.Probe {
	return cli.Probe{Args: []string{"system", "status"}, Hint: "run `container system start`"}
}

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	return append(args, cfg.commonRunArgs(image, envFile, nil)...)
}

func (appleEngine) parseRunID([]byte) string { return "" }

func (appleEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

func (appleEngine) parseInspect(data []byte, id string) (*engineInfo, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.ID != id {
			continue
		}
		platform := c.Configuration.Platform
		image := c.Configuration.Image.Reference
		imageDigest := c.Configuration.Image.Descriptor.Digest
		imageVariantDigest := c.Configuration.Image.VariantDigest
		if image != "" && validImageDigest(imageDigest) {
			image = stripImageDigest(image) + "@" + imageDigest
		}
		info := &engineInfo{
			state:              State(c.Status.State),
			labels:             c.Configuration.Labels,
			image:              image,
			imageDigest:        imageDigest,
			imageVariantDigest: imageVariantDigest,
			platform:           formatInspectPlatform(platform.OS, platform.Architecture, platform.Variant),
		}
		if ip, err := c.IPv4(); err == nil {
			info.ip = ip
		}
		for _, p := range c.Configuration.PublishedPorts {
			info.bound = append(info.bound, boundPort{
				containerPort: p.ContainerPort,
				proto:         p.Proto,
				hostAddr:      p.HostAddress,
				hostPort:      p.HostPort,
			})
		}
		return info, nil
	}
	return nil, fmt.Errorf("%w: container %s not in inspect output", ErrContainerNotFound, id)
}

func formatInspectPlatform(os, architecture, variant string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{os, architecture, variant} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

func (appleEngine) stopArgs(id string, timeout *time.Duration) []string {
	args := []string{"stop"}
	if timeout != nil {
		args = append(args, "--time", strconv.Itoa(int(timeout.Seconds())))
	}
	return append(args, id)
}

func (appleEngine) deleteArgs(id string) []string {
	return []string{"delete", "--force", id}
}

func (appleEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return []string{"cp", hostPath, id + ":" + containerPath}
}

func (appleEngine) copyFromArgs(id, containerPath, hostPath string) []string {
	return []string{"cp", id + ":" + containerPath, hostPath}
}

func (appleEngine) reaperSubcommand() string { return "delete" }

func (appleEngine) execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string {
	args := []string{"exec"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if cfg.user != "" {
		args = append(args, "--user", cfg.user)
	}
	if cfg.workdir != "" {
		args = append(args, "--workdir", cfg.workdir)
	}
	args = append(args, id)
	return append(args, cmd...)
}

func (appleEngine) logsArgs(id string, follow bool) []string {
	if follow {
		return []string{"logs", "--follow", id}
	}
	return []string{"logs", id}
}

func (appleEngine) logsTailArgs(id string) []string {
	return []string{"logs", "-n", "1000", id}
}

func (appleEngine) listArgs() []string {
	return []string{"ls", "--all", "--format", "json"}
}

// parseStoppedManaged filters client-side and retains the list-time
// identity needed to close the Apple name-addressed delete race.
func (appleEngine) parseStoppedManaged(data []byte) ([]pruneCandidate, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var candidates []pruneCandidate
	for _, c := range containers {
		labels := c.Configuration.Labels
		if labels[managedLabel] != "true" || c.Status.State != string(StateStopped) {
			continue
		}
		candidates = append(candidates, pruneCandidate{
			id:         c.ID,
			creation:   labels[creationLabel],
			state:      State(c.Status.State),
			managed:    true,
			reuseGroup: labels[reuseGroupLabel],
		})
	}
	return candidates, nil
}

func (appleEngine) imageInspectArgs(image, _ string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "pull", "--platform", platform, image}
	}
	return []string{"image", "pull", image}
}

// imageMissing matches the CLI's error for an absent image.
func (appleEngine) imageMissing(err error) bool {
	return appleStderrContains(err, appleStderrNotFound)
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
			Reference     string               `json:"reference"`
			Descriptor    appleImageDescriptor `json:"descriptor"`
			VariantDigest string               `json:"variantDigest"`
		} `json:"image"`
	} `json:"configuration"`
	Variants []appleImageVariant `json:"variants"`
}

func (appleEngine) imageIdentityNeedsLocalCheck() bool { return true }

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	identity, exists := (appleEngine{}).parseImageIdentity(data, "", platform)
	return exists && !identity.notLocal
}

// parseImageIdentity reads the root index descriptor exposed by current
// Apple Container releases and also accepts the older flat image shape.
// When a platform is selected, the root remains the run identity while
// the selected variant is retained as validation metadata.
func (appleEngine) parseImageIdentity(data []byte, image, platform string) (imageIdentity, bool) {
	var records []appleImageInspectRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return imageIdentity{}, true
	}
	if len(records) == 0 {
		return imageIdentity{}, false
	}
	record := records[0]
	if platform == "" && (record.Configuration.Platform.OS != "" || record.Configuration.Platform.Architecture != "" || record.Configuration.Platform.Variant != "") {
		candidate := record.Configuration.Platform.OS + "/" + record.Configuration.Platform.Architecture
		if record.Configuration.Platform.Variant != "" {
			candidate += "/" + record.Configuration.Platform.Variant
		}
		// An incomplete platform field carries no usable selector. Ignore it
		// and fall through to the root descriptor instead of discarding a
		// record whose immutable identity is still proven.
		if parsed, ok := parseApplePlatformSelector(candidate); ok {
			platform = formatInspectPlatform(parsed.os, parsed.architecture, parsed.variant)
		}
	}
	rootDigest, rootOK := appleRootDescriptorDigest(record)
	if platform != "" {
		variantDigest, variantOK := applePlatformVariantDigest(record, platform)
		if !variantOK {
			return imageIdentity{
				notLocal:       true,
				notLocalReason: "Apple image inspect has no complete matching platform variant",
				platform:       platform,
			}, true
		}
		if !rootOK {
			return imageIdentity{}, true
		}
		return finishAppleImageIdentity(image, record, rootDigest, platform, variantDigest), true
	}
	if !rootOK && appleDescriptorPresent(record) {
		return imageIdentity{}, true
	}
	return finishAppleImageIdentity(image, record, rootDigest, "", ""), true
}

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

type applePlatform struct {
	os           string
	architecture string
	variant      string
}

func parseApplePlatformSelector(platform string) (applePlatform, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(platform)), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" ||
		(len(parts) == 3 && parts[2] == "") {
		return applePlatform{}, false
	}
	variant := ""
	if len(parts) == 3 {
		variant = parts[2]
	} else {
		switch parts[1] {
		case "arm", "armhf":
			variant = "v7"
		case "armel":
			variant = "v6"
		case "aarch64", "arm64":
			variant = "v8"
		}
	}
	return canonicalApplePlatform(parts[0], parts[1], variant), true
}

func canonicalApplePlatform(osName, architecture, variant string) applePlatform {
	p := applePlatform{os: osName, architecture: architecture, variant: variant}
	switch architecture {
	case "aarch64", "arm64":
		p.architecture = "arm64"
		if variant == "" || variant == "v8" || variant == "8" {
			p.variant = "v8"
		}
	case "x86_64", "x86-64", "amd64":
		p.architecture = "amd64"
		if variant == "v1" {
			p.variant = ""
		}
	case "armhf", "armel":
		p.architecture = "arm"
	}
	return p
}

func applePlatformsEqual(want, have applePlatform) bool {
	return want.os == have.os && want.architecture == have.architecture && want.variant == have.variant
}

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
		// An entry without a usable platform or digest cannot match the
		// requested selector; it is not evidence of ambiguity. Attestation
		// and SBOM descriptors commonly appear alongside real variants.
		if variant.Platform.OS == "" || variant.Platform.Architecture == "" || !validImageDigest(variant.Digest) {
			continue
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

func finishAppleImageIdentity(requested string, record appleImageInspectRecord, digest, platform, variantDigest string) imageIdentity {
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
			return imageIdentity{}
		}
		if !appleImageIDMatchesDescriptor(requested, record.ID, digest) {
			return imageIdentity{mismatch: true}
		}
	} else if requested != "" && reference != "" && !appleImageReferencesCompatible(requested, reference) {
		return imageIdentity{mismatch: true}
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
				identity.repository = imageRepository(base)
				identity.appleSynthetic = false
			}
		}
		return identity
	}
	if isImageID(record.ID) {
		// Apple must not treat a Docker-style ID field as an immutable
		// run target without a descriptor proving what that ID names.
		return imageIdentity{}
	}
	if len(record.ID) == 64 && isHex(record.ID) {
		// Older Apple image-inspect responses exposed the local content ID
		// without a descriptor. Keep the compatibility fallback, but do
		// not classify it as a Docker image ID.
		digest := "sha256:" + record.ID
		identity := imageReferenceWithDigest(appleRunReferenceBase(requested, reference), reference, digest, "")
		identity.rootDigest = digest
		identity.platform = platform
		identity.variantDigest = variantDigest
		identity.repository = imageRepository(identity.reference)
		return identity
	}
	return imageIdentity{}
}

// appleSyntheticIndex identifies Apple's local index wrapper around one
// manifest. Such a digest is present in the content store but is not a
// registry reference that can safely be pulled by name@digest.
func appleSyntheticIndex(record appleImageInspectRecord, rootDigest, variantDigest string) bool {
	if len(record.Variants) != 1 {
		return false
	}
	if variantDigest == "" {
		variantDigest = record.Variants[0].Digest
	}
	if !validImageDigest(rootDigest) {
		return false
	}
	if !validImageDigest(variantDigest) {
		// A single-variant record with an unusable manifest digest is not
		// evidence that the generated root is registry-addressable.
		return true
	}
	if strings.EqualFold(rootDigest, variantDigest) {
		return false
	}
	descriptor := record.Configuration.Descriptor
	if descriptor.Digest == "" {
		descriptor = record.Configuration.Image.Descriptor
	}
	if descriptor.MediaType != "" {
		mediaType := strings.ToLower(descriptor.MediaType)
		if !strings.Contains(mediaType, "index") && !strings.Contains(mediaType, "manifest.list") {
			return false
		}
	}
	if descriptor.Annotations != nil {
		if value, ok := descriptor.Annotations["com.apple.containerization.index.indirect"]; ok {
			return strings.EqualFold(value, "true") || value == "1"
		}
	}
	// Current Apple releases omit the indirect annotation from the
	// serialized descriptor. A one-variant root whose digest differs from
	// that variant is therefore the observable synthetic-index shape.
	return true
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
	// sides carry digests, their claims must agree; descriptor-backed
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
	return len(recordID) == 64 && isHex(recordID) && strings.EqualFold("sha256:"+recordID, digest)
}

func splitPlatform(p string) (os, arch, variant string) {
	parts := strings.Split(p, "/")
	if len(parts) > 0 {
		os = parts[0]
	}
	if len(parts) > 1 {
		arch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return os, arch, variant
}

func (appleEngine) listReuseGroupArgs(string) []string {
	return []string{"ls", "--all", "--format", "json"}
}

func (appleEngine) parseReuseGroupIDs(data []byte, group string) ([]pruneCandidate, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var candidates []pruneCandidate
	for _, c := range containers {
		labels := c.Configuration.Labels
		if labels[reuseGroupLabel] != group {
			continue
		}
		candidates = append(candidates, pruneCandidate{
			id:         c.ID,
			creation:   labels[creationLabel],
			state:      State(c.Status.State),
			managed:    labels[managedLabel] == "true",
			reuseGroup: labels[reuseGroupLabel],
		})
	}
	return candidates, nil
}

func (appleEngine) nameAddressedDeletes() bool { return true }

// nameConflict matches Apple Container's duplicate-name wording.
func (appleEngine) nameConflict(err error) bool {
	s, ok := appleCLIStderr(err)
	if !ok {
		return false
	}
	return strings.Contains(s, appleStderrAlready) &&
		(strings.Contains(s, appleStderrExist) ||
			strings.Contains(s, appleStderrInUse) ||
			strings.Contains(s, appleStderrTaken))
}

// containerMissing matches a CLI failure for an absent container.
func (appleEngine) containerMissing(err error) bool {
	s, ok := appleCLIStderr(err)
	if !ok {
		return false
	}
	return strings.Contains(s, appleStderrNotFound) ||
		strings.Contains(s, appleStderrNoSuchObj) ||
		strings.Contains(s, appleStderrNoSuchCtr)
}

func appleCLIStderr(err error) (string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return "", false
	}
	return strings.ToLower(cliErr.Stderr), true
}

func appleStderrContains(err error, substr string) bool {
	s, ok := appleCLIStderr(err)
	return ok && strings.Contains(s, substr)
}
