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

func (appleEngine) checkConfig(cfg *config) error {
	if err := resolveEffectivePlatform(cfg); err != nil {
		return err
	}
	if cfg.platform != "" {
		return validateApplePlatform(cfg.platform)
	}
	return nil
}

// validateApplePlatform mirrors the platform grammar accepted by Apple
// Container. Docker keeps the generic platformRE grammar.
func validateApplePlatform(platform string) error {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid platform %q for Apple backend: expected os/arch[/variant]", platform)
	}
	if len(parts) > 3 || (len(parts) == 3 && parts[2] == "") {
		return fmt.Errorf("invalid platform %q for Apple backend: expected os/arch[/variant]", platform)
	}
	if parts[0] != "linux" {
		return fmt.Errorf("apple backend supports only linux platforms, got %q", platform)
	}
	if len(parts) == 2 {
		return nil
	}

	arch, variant := parts[1], parts[2]
	valid := false
	switch arch {
	case "arm":
		valid = variant == "v5" || variant == "v6" || variant == "v7" || variant == "v8"
	case "armhf":
		valid = variant == "v7"
	case "armel":
		valid = variant == "v6"
	case "aarch64", "arm64":
		valid = variant == "v8" || variant == "8"
	case "x86_64", "x86-64", "amd64":
		valid = variant == "v1"
	}
	if !valid {
		return fmt.Errorf("invalid platform %q for Apple backend: variant %q is not valid for architecture %q", platform, variant, arch)
	}
	return nil
}

// applePlatform is the normalized selector used when matching an Apple
// image variant. Apple treats architecture aliases and the default arm64
// variant as equivalent.
type applePlatform struct {
	os           string
	architecture string
	variant      string
}

func parseApplePlatformSelector(platform string) (applePlatform, bool) {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return applePlatform{}, false
	}
	if len(parts) == 3 && parts[2] == "" {
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
	return want.os == have.os &&
		want.architecture == have.architecture &&
		want.variant == have.variant
}

func formatApplePlatform(platform applePlatform) string {
	if platform.os == "" || platform.architecture == "" {
		return ""
	}
	value := platform.os + "/" + platform.architecture
	if platform.variant != "" {
		value += "/" + platform.variant
	}
	return value
}

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
		info := &engineInfo{
			state:              State(c.Status.State),
			labels:             c.Configuration.Labels,
			image:              image,
			imageDigest:        imageDigest,
			imageVariantDigest: c.Configuration.Image.VariantDigest,
			platform:           platform,
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
	return nil, fmt.Errorf("container %s not in inspect output", id)
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

func (appleEngine) imageInspectArgs(image, _ string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "pull", "--platform", platform, image}
	}
	return []string{"image", "pull", image}
}

// imageMissing matches the backend's image-absence reason, not an
// arbitrary occurrence of "not found" in an image name or application
// message. Apple reports the reason as `image not found: <name>`.
func (appleEngine) imageMissing(err error) bool {
	cliErr, ok := imageCLIErrorForBackend(err, "container")
	return ok && appleImageMissingStderr(cliErr.Stderr)
}

func appleImageMissingStderr(stderr string) bool {
	for _, raw := range strings.Split(strings.ToLower(stderr), "\n") {
		line := strings.TrimSpace(raw)
		line = strings.Trim(line, "()[]{} \t:,")
		for range 3 {
			line = strings.Trim(line, " \t\"'")
			matchedPrefix := false
			for _, prefix := range []string{"notfound:", "error:", "failed:"} {
				if strings.HasPrefix(line, prefix) {
					line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
					matchedPrefix = true
					break
				}
			}
			if !matchedPrefix {
				break
			}
		}
		if strings.HasPrefix(line, "image not found:") {
			return true
		}
	}
	return false
}

func (appleEngine) imageIdentityNeedsLocalCheck() bool { return true }

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	identity, exists := (appleEngine{}).parseImageIdentity(data, "", platform)
	return exists && !identity.notLocal
}

// parseImageIdentity reads the root index descriptor exposed by current
// Apple Container releases and also accepts the older flat
// ImageDescription shape. When a platform is selected, the variant is
// validated separately while the root descriptor remains the run and
// reuse identity.
func (appleEngine) parseImageIdentity(data []byte, image, platform string) (imageIdentity, bool) {
	var records []appleImageInspectRecord
	if err := json.Unmarshal(data, &records); err != nil {
		// A successful CLI invocation with malformed JSON is not proof
		// that the image is absent. Treat it as an identity-unavailable
		// result so PullMissing cannot turn a parser failure into a fetch.
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
		parsed, ok := parseApplePlatformSelector(candidate)
		if !ok {
			return imageIdentity{}, true
		}
		platform = formatApplePlatform(parsed)
	}
	rootDigest, rootOK := appleRootDescriptorDigest(record)
	variantDigest := ""
	if platform != "" {
		var variantOK bool
		variantDigest, variantOK = applePlatformVariantDigest(record, platform)
		if !variantOK {
			return imageIdentity{
				notLocal:       true,
				notLocalReason: "Apple image inspect has no complete matching platform variant",
				platform:       platform,
			}, true
		}
		if !rootOK {
			// A selected variant is not a substitute for the root
			// index identity used by run and reuse.
			return imageIdentity{}, true
		}
	}
	if !rootOK && appleDescriptorPresent(record) {
		// A malformed descriptor is not equivalent to an older
		// descriptor-less response. Do not fall back to an ID when the
		// backend supplied an unusable descriptor field.
		return imageIdentity{}, true
	}
	return finishAppleImageIdentity(image, record, rootDigest, platform, variantDigest), true
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
	if imagesCompatible(requested, actual) {
		return true
	}
	// A custom default registry is reported canonically by Apple, while the
	// caller may have supplied an unqualified name. Accept that backend
	// spelling only when the repository path and tag agree. If both sides
	// carry digests, their digest claims must also agree; a descriptor-backed
	// identity is checked separately against the selected root/variant.
	requestedDigest := imageDigest(requested)
	actualDigest := imageDigest(actual)
	if requestedDigest != "" && actualDigest != "" && !strings.EqualFold(requestedDigest, actualDigest) {
		return false
	}
	return imageRepositoryPathsCompatible(requested, actual)
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
	return len(recordID) == 64 && isHex(recordID) && strings.EqualFold("sha256:"+recordID, digest)
}

func (appleEngine) listReuseGroupArgs(string) []string {
	return []string{"ls", "--all", "--format", "json"}
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
	return appleStderrContains(err, appleStderrNotFound) ||
		appleStderrContains(err, appleStderrNoSuchObj) ||
		appleStderrContains(err, appleStderrNoSuchCtr)
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
