package container

import (
	"bytes"
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

func (appleEngine) name() string               { return "apple" }
func (appleEngine) binary() string             { return "container" }
func (appleEngine) directIP() bool             { return true }
func (appleEngine) nameAddressedDeletes() bool { return true }

func (appleEngine) checkConfig(*config) error { return nil }

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
		imageDigestValue := imageDigest(image)
		operatorPinned := validOCIDigest(imageDigestValue)
		descriptorDigest := c.Configuration.Image.Descriptor.Digest
		if !operatorPinned && image != "" && validOCIDigest(descriptorDigest) {
			// The reference the operator passed may pin an image index
			// while the descriptor reports the resolved child manifest for
			// this platform. A pinned digest is the identity the caller
			// asked for, so only a reference without one is completed from
			// the descriptor.
			imageDigestValue = descriptorDigest
			image = qualifyImageReference(image, descriptorDigest)
		}
		info := &engineInfo{
			state:       State(c.Status.State),
			labels:      c.Configuration.Labels,
			image:       image,
			imageDigest: imageDigestValue,
			platform:    formatInspectPlatform(c.Configuration.Platform.OS, c.Configuration.Platform.Architecture, c.Configuration.Platform.Variant),
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

// stoppedDeleteArgs omits --force so a generation that started after the
// stopped-state verification is not removed; the delete fails instead.
func (appleEngine) stoppedDeleteArgs(id string) []string {
	return []string{"delete", id}
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

func (appleEngine) platformCompatible(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
}

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	var rawImages []json.RawMessage
	// Decode the complete shape before applying the legacy fallback.
	// Invalid JSON or a platform field of the wrong type is not equivalent
	// to an older backend that simply omitted metadata.
	if err := json.Unmarshal(data, &rawImages); err != nil || len(rawImages) == 0 {
		return false
	}

	// Validate all platform-shaped fields even when no selector was
	// requested. A malformed record must not make an otherwise present
	// image look healthy.
	metadataValid := true
	records := make([]appleImageMetadata, 0, len(rawImages))
	for _, rawImage := range rawImages {
		if appleJSONNull(rawImage) {
			return false
		}
		var image struct {
			Platform json.RawMessage `json:"platform"`
			Variants json.RawMessage `json:"variants"`
		}
		if err := json.Unmarshal(rawImage, &image); err != nil {
			return false
		}
		var rawVariants []json.RawMessage
		if len(image.Variants) != 0 {
			if appleJSONNull(image.Variants) {
				return false
			}
			if err := json.Unmarshal(image.Variants, &rawVariants); err != nil {
				return false
			}
		}
		var platformData appleImagePlatform
		hasPlatform := false
		if len(image.Platform) != 0 {
			if appleJSONNull(image.Platform) {
				return false
			}
			if err := json.Unmarshal(image.Platform, &platformData); err != nil {
				return false
			}
			hasPlatform = true
			if formatInspectPlatform(platformData.OS, platformData.Architecture, platformData.Variant) == "" {
				metadataValid = false
			}
		}
		variants := make([]appleImageVariant, 0, len(rawVariants))
		for _, rawVariant := range rawVariants {
			if appleJSONNull(rawVariant) {
				return false
			}
			var variant struct {
				Platform json.RawMessage `json:"platform"`
			}
			if err := json.Unmarshal(rawVariant, &variant); err != nil {
				return false
			}
			var variantPlatform appleImagePlatform
			hasVariantPlatform := false
			if len(variant.Platform) != 0 {
				if appleJSONNull(variant.Platform) {
					return false
				}
				if err := json.Unmarshal(variant.Platform, &variantPlatform); err != nil {
					return false
				}
				hasVariantPlatform = true
				if formatInspectPlatform(variantPlatform.OS, variantPlatform.Architecture, variantPlatform.Variant) == "" {
					metadataValid = false
				}
			} else {
				metadataValid = false
			}
			variants = append(variants, appleImageVariant{
				platform:  variantPlatform,
				hasFields: hasVariantPlatform,
			})
		}
		records = append(records, appleImageMetadata{
			platform:  platformData,
			hasFields: hasPlatform,
			variants:  variants,
		})
	}

	if !metadataValid {
		return false
	}
	if platform == "" {
		return true
	}
	wantOS, wantArch, wantVariant, selectorOK := parsePlatformParts(platform)
	if !selectorOK {
		return false
	}
	osOnly := wantOS != "" && wantArch == "" && wantVariant == ""
	for _, image := range records {
		hasMetadata := false
		if image.hasFields {
			// A present but empty platform object is not the same as an
			// omitted field: it is malformed metadata and must not enable
			// the legacy fallback below.
			hasMetadata = true
			actual := formatInspectPlatform(
				image.platform.OS,
				image.platform.Architecture,
				image.platform.Variant,
			)
			if actual != "" && platformSelectorMatches(platform, actual) {
				return true
			}
		}
		for _, variant := range image.variants {
			hasMetadata = true
			if !variant.hasFields {
				continue
			}
			actual := formatInspectPlatform(
				variant.platform.OS,
				variant.platform.Architecture,
				variant.platform.Variant,
			)
			if actual != "" && platformSelectorMatches(platform, actual) {
				return true
			}
		}
		if !hasMetadata && osOnly {
			// Only an actually absent platform field gets the legacy
			// OS-only presence fallback. Known mismatching metadata is
			// deliberately not treated as an unconstrained image.
			return true
		}
	}
	return false
}

type appleImagePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

type appleImageVariant struct {
	platform  appleImagePlatform
	hasFields bool
}

type appleImageMetadata struct {
	platform  appleImagePlatform
	hasFields bool
	variants  []appleImageVariant
}

func appleJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

//nolint:unused // retained for package-local platform parsing compatibility.
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
