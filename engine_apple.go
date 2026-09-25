package container

import (
	"encoding/json"
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
// Matchers are both command- and binary-aware. Apple uses different
// ContainerizationError messages for the client-side inspect path and
// for the API-client paths used by exec/stop/delete/logs:
//   - inspect: "container not found: …"
//   - exec: "get failed: container <id> not found"
//   - stop/delete: "container with ID <id> not found" (often wrapped by
//     "failed to stop/delete container: …")
//   - logs: "failed to get logs for container <id>: …"
//
// Generic application output containing "not found" is not a backend match.
const (
	appleStderrNameConflict  = "container with id"
	appleStderrAlreadyExists = "already exists"
	appleStderrImageNotFound = "image not found:"
)

func (appleEngine) name() string   { return "apple" }
func (appleEngine) binary() string { return "container" }
func (appleEngine) directIP() bool { return true }

func (appleEngine) checkConfig(*config) error { return nil }

func (appleEngine) defaultHost() string { return "127.0.0.1" }

func (appleEngine) probe() cli.Probe {
	return cli.Probe{
		Args:          []string{"system", "status"},
		Hint:          "run `container system start`",
		IsUnavailable: appleProbeUnavailable,
	}
}

func appleProbeUnavailable(err error) bool {
	ctx, ok := backendCLIError(err, "container")
	if !ok || ctx.operation != "system" {
		return false
	}
	if cli.IsProbeConfigurationError(err) || cli.IsNonLivenessError(err) {
		return false
	}
	lines, ok := cliErrorLines(err)
	if !ok {
		return false
	}
	for _, line := range lines {
		for _, fragment := range []string{
			"xpc connection",
			"container-apiserver",
			"plugins are unavailable",
			"start the container system services",
			"system is not running",
			"system service is not running",
			"apiserver is not running",
			"not registered with launchd",
			"connection refused",
		} {
			if strings.Contains(line, fragment) {
				return true
			}
		}
	}
	return false
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
		info := &engineInfo{
			state:  State(c.Status.State),
			labels: c.Configuration.Labels,
			image:  c.Configuration.Image.Reference,
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

func (appleEngine) imageInspectArgs(image, _ string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "pull", "--platform", platform, image}
	}
	return []string{"image", "pull", image}
}

// imageMissing matches only Apple's image-inspect error. A pull failure
// with similar text is not evidence that the local image check was missing.
func (appleEngine) imageMissing(err error) bool {
	ctx, ok := backendCLIError(err, "container")
	if !ok || ctx.operation != "image inspect" {
		return false
	}
	return hasCLIErrorLine(err, func(line string) bool {
		rest, ok := strings.CutPrefix(line, appleStderrImageNotFound)
		return ok && cliTargetListMatches(rest, ctx.target)
	})
}

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return false
	}
	if platform == "" {
		return true
	}
	var images []struct {
		Variants []struct {
			Platform struct {
				Os           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"variants"`
	}
	if err := json.Unmarshal(data, &images); err != nil {
		return true
	}
	wantOS, wantArch, wantVariant := splitPlatform(platform)
	for _, img := range images {
		if len(img.Variants) == 0 {
			return true
		}
		for _, v := range img.Variants {
			if wantOS != "" && v.Platform.Os != wantOS {
				continue
			}
			if wantArch != "" && v.Platform.Architecture != wantArch {
				continue
			}
			if wantVariant != "" && v.Platform.Variant != wantVariant {
				continue
			}
			return true
		}
	}
	return false
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

// nameConflict matches Apple Container's duplicate-name wording on a
// create/run command. Other commands may legitimately contain the words
// "already" or "exists" in application/configuration diagnostics.
func (appleEngine) nameConflict(err error) bool {
	ctx, ok := backendCLIError(err, "container")
	if !ok || ctx.operation != "run" {
		return false
	}
	return hasCLIErrorLine(err, func(line string) bool {
		return appleNameConflictLine(line, ctx.target)
	})
}

func appleNameConflictLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, appleStderrNameConflict)
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	suffix := " " + appleStderrAlreadyExists
	if !strings.HasSuffix(rest, suffix) {
		return false
	}
	id := strings.TrimSpace(strings.TrimSuffix(rest, suffix))
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || sameCLITarget(id, target))
}

// containerMissing matches the command-specific Apple Container form for
// an absent container. The command and target are required so an app that
// prints "container not found" cannot be mistaken for a backend result.
func (appleEngine) containerMissing(err error) bool {
	ctx, ok := backendCLIError(err, "container")
	if !ok {
		return false
	}
	switch ctx.operation {
	case "inspect":
		return hasCLIErrorLine(err, func(line string) bool {
			rest, ok := strings.CutPrefix(line, "container not found:")
			return ok && cliTargetListMatches(rest, ctx.target)
		})
	case "exec":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleExecMissingLine(line, ctx.target)
		})
	case "stop":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleStateMissingLine(line, ctx.target, "failed to stop container:")
		})
	case "delete", "rm":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleStateMissingLine(line, ctx.target, "failed to delete container:")
		})
	case "logs":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleLogsMissingLine(line, ctx.target)
		})
	default:
		return false
	}
}

func appleExecMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "get failed:")
	if !ok {
		return false
	}
	return appleContainerMissingLine(strings.TrimSpace(rest), target)
}

func appleContainerMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "container ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.TrimSpace(strings.TrimSuffix(rest, " not found"))
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || sameCLITarget(id, target))
}

func appleIDMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "container with id ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.TrimSpace(strings.TrimSuffix(rest, " not found"))
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || sameCLITarget(id, target))
}

func appleStateMissingLine(line, target, wrapper string) bool {
	current := line
	for range 3 {
		if appleIDMissingLine(current, target) {
			return true
		}
		rest, ok := strings.CutPrefix(current, wrapper)
		if !ok {
			return false
		}
		current = strings.TrimSpace(rest)
	}
	return false
}

func appleLogsMissingLine(line, target string) bool {
	const logsPrefix = "failed to get logs for container "
	current := line
	if rest, ok := strings.CutPrefix(current, logsPrefix); ok {
		rest = strings.TrimSpace(rest)
		separator := strings.Index(rest, ":")
		if separator < 0 {
			return false
		}
		logID := strings.TrimSpace(rest[:separator])
		if logID == "" || (target != "" && !sameCLITarget(logID, target)) {
			return false
		}
		current = strings.TrimSpace(rest[separator+1:])
	}

	const (
		openPrefix = "failed to open container logs: "
		getPrefix  = "get failed:"
	)
	for range 3 {
		if appleIDMissingLine(current, target) || appleExecMissingLine(current, target) {
			return true
		}
		if rest, ok := strings.CutPrefix(current, openPrefix); ok {
			current = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(current, getPrefix); ok {
			current = strings.TrimSpace(rest)
			if appleContainerMissingLine(current, target) {
				return true
			}
			continue
		}
		return false
	}
	return false
}
