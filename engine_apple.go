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
	appleStderrAlready  = "already"
	appleStderrExist    = "exist"
	appleStderrInUse    = "in use"
	appleStderrTaken    = "taken"
	appleStderrNotFound = "not found"
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

func (appleEngine) reaperDeleteFlags() []string { return nil }

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

// containerMissing matches the command-specific Apple Container forms
// for an absent container. The command, binary, and exact target are
// required so application output containing "not found" is not treated
// as a backend result.
func (appleEngine) containerMissing(err error) bool {
	if !cliErrorBelongsTo(err, "container") {
		return false
	}
	command, args, ok := cliCommandParts(err)
	if !ok {
		return false
	}
	target := cliCommandTarget(command, args)
	if target == "" {
		return false
	}
	switch command {
	case "inspect", "exec", "stop", "delete", "rm", "logs":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleContainerMissingLine(line, target, command)
		})
	default:
		return false
	}
}

func appleContainerMissingLine(line, target, command string) bool {
	if appleTypedNotFoundLine(line, target, command) {
		return true
	}
	switch command {
	case "inspect":
		rest, ok := strings.CutPrefix(line, "container not found:")
		return ok && cliTargetListMatches(rest, target)
	case "exec":
		return appleExecMissingLine(line, target)
	case "stop":
		return appleStateMissingLine(line, target, "failed to stop container:")
	case "delete", "rm":
		return appleStateMissingLine(line, target, "failed to delete container:")
	case "logs":
		return appleLogsMissingLine(line, target)
	default:
		return false
	}
}

// appleTypedNotFoundLine handles the structured ContainerizationError
// spelling emitted by Apple Container 1.3.0. The CLI prints descriptions
// such as notFound: "container not found: id" and can wrap that value in
// internalError: "..." (cause: "...") layers. Only these typed forms
// are accepted; an arbitrary application line containing "not found" is
// deliberately not a backend absence result.
func appleTypedNotFoundLine(line, target, command string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleContainerNotFoundMessage(message, target, command)
}

func appleTypedContainerIDNotFoundLine(line, target string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleIDMessageMatches(message, target)
}

func appleTypedNotFoundMessage(line string) (string, bool) {
	current := strings.ToLower(strings.TrimSpace(line))
	for range 6 {
		current = strings.TrimSpace(current)
		current = strings.Trim(current, "()")
		current = strings.TrimSpace(current)
		if strings.HasPrefix(current, "notfound:") {
			message := strings.TrimSpace(strings.TrimPrefix(current, "notfound:"))
			message = strings.TrimSpace(strings.Trim(message, `"'`))
			message = strings.ReplaceAll(message, `\"`, `"`)
			return message, message != ""
		}
		if strings.HasPrefix(current, "internalerror:") || strings.HasPrefix(current, "cause:") {
			index := strings.Index(current, "cause:")
			if index < 0 {
				return "", false
			}
			current = strings.TrimSpace(current[index+len("cause:"):])
			if len(current) >= 2 && current[0] == '"' && current[len(current)-1] == '"' {
				current = current[1 : len(current)-1]
			}
			current = strings.ReplaceAll(current, `\"`, `"`)
			continue
		}
		return "", false
	}
	return "", false
}

func appleContainerNotFoundMessage(message, target, command string) bool {
	message = strings.TrimSpace(strings.Trim(message, `"'`))
	if rest, ok := strings.CutPrefix(message, "container not found:"); ok {
		return cliTargetListMatches(rest, target)
	}
	if rest, ok := strings.CutPrefix(message, "get failed:"); ok {
		return command == "exec" && appleExecMissingLine("get failed:"+rest, target)
	}
	if appleIDMessageMatches(message, target) {
		switch command {
		case "inspect", "exec", "stop", "delete", "rm", "logs":
			return true
		default:
			return false
		}
	}
	return false
}

func appleIDMessageMatches(message, target string) bool {
	return appleIDMissingLine(strings.TrimSpace(strings.Trim(message, `"'`)), target)
}

func appleExecMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "get failed:")
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	rest, ok = strings.CutPrefix(rest, "container ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || strings.EqualFold(id, target))
}

func appleIDMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "container with id ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || strings.EqualFold(id, target))
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
	const prefix = "failed to get logs for container "
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return appleIDMissingLine(line, target)
	}
	rest = strings.TrimSpace(rest)
	separator := strings.Index(rest, ":")
	if separator < 0 {
		return false
	}
	logID := strings.TrimSpace(rest[:separator])
	if logID == "" || (target != "" && !strings.EqualFold(logID, target)) {
		return false
	}
	nested := strings.TrimSpace(rest[separator+1:])
	const openPrefix = "failed to open container logs: "
	for range 3 {
		if appleIDMissingLine(nested, target) || appleExecMissingLine(nested, target) {
			return true
		}
		openRest, ok := strings.CutPrefix(nested, openPrefix)
		if !ok {
			return false
		}
		nested = strings.TrimSpace(openRest)
	}
	return false
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
