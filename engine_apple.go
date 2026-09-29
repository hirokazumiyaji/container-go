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
		platform := c.Configuration.Platform
		platformMeta := platformMetadataFromParts(
			platform.OS,
			platform.Architecture,
			platform.Variant,
			platform.OSPresent || platform.OS != "",
			platform.ArchPresent || platform.Architecture != "",
			platform.VariantPresent || platform.Variant != "",
		)
		info := &engineInfo{
			state:        State(c.Status.State),
			labels:       c.Configuration.Labels,
			image:        c.Configuration.Image.Reference,
			platform:     platformMeta.normalized(),
			platformMeta: platformMeta,
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

//nolint:unused // retained for callers formatting OCI platform components
func formatInspectPlatform(os, architecture, variant string) string {
	return platformMetadataFromParts(
		os, architecture, variant,
		os != "", architecture != "", variant != "",
	).normalized()
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

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return false
	}
	if platform == "" {
		return true
	}
	selector, ok := parsePlatform(platform)
	if !ok {
		return false
	}
	// A bare OS selector intentionally means "any architecture on this
	// OS". It therefore retains the image-presence behavior even when an
	// older Apple response does not include a decodable variants array.
	// Architecture/variant selectors are different: an absent or malformed
	// variants list cannot prove that the requested image is present.
	osOnly := selector.architecture == "" && selector.variant == ""
	var images []struct {
		Variants []struct {
			Platform struct {
				OS           *string `json:"os"`
				Architecture *string `json:"architecture"`
				Variant      *string `json:"variant"`
			} `json:"platform"`
		} `json:"variants"`
	}
	if err := json.Unmarshal(data, &images); err != nil {
		return osOnly
	}
	for _, img := range images {
		if len(img.Variants) == 0 {
			if osOnly {
				return true
			}
			continue
		}
		for _, v := range img.Variants {
			actualMeta := platformMetadata{
				valid:      true,
				osSet:      v.Platform.OS != nil,
				archSet:    v.Platform.Architecture != nil,
				variantSet: v.Platform.Variant != nil,
			}
			if v.Platform.OS != nil {
				actualMeta.os = *v.Platform.OS
			}
			if v.Platform.Architecture != nil {
				actualMeta.architecture = *v.Platform.Architecture
			}
			if v.Platform.Variant != nil {
				actualMeta.variant = *v.Platform.Variant
			}
			// Image presence is allowed to fall back to the unconstrained
			// OS-only result for old responses that omit the entire
			// variants platform object. An explicitly empty selected field
			// is retained and does not satisfy an architecture selector.
			if osOnly && !actualMeta.osSet && !actualMeta.archSet && !actualMeta.variantSet {
				return true
			}
			if platformMetadataMatches(platform, actualMeta) {
				return true
			}
		}
	}
	return false
}

func (appleEngine) platformCompatible(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
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
