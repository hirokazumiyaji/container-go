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
// Backend error matching is operation- and target-aware in errors.go.

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
	if cli.IsNonLivenessError(err) {
		return false
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	if len(cliErr.Args) < 2 || strings.ToLower(cliErr.Args[0]) != "system" || strings.ToLower(cliErr.Args[1]) != "status" {
		return false
	}
	stdout, stderr, ok := cli.DiagnosticText(err)
	if !ok {
		return false
	}
	text := strings.ToLower(stdout + "\n" + stderr)
	for _, fragment := range []string{
		"xpc connection", "container-apiserver", "plugins are unavailable",
		"start the container system services", "system is not running",
		"system service is not running", "apiserver is not running",
		"not registered with launchd", "connection refused", "backend down", "daemon unavailable",
	} {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	return append(args, cfg.commonRunArgs(image, envFile, nil)...)
}

func (appleEngine) parseRunID([]byte) string { return "" }

func (appleEngine) nameAddressedDeletes() bool { return true }

// copyNeedsRunning reports that `container cp` only accepts a running
// container on this backend.
func (appleEngine) copyNeedsRunning() bool { return true }

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
	return nil, fmt.Errorf("%w: %s not in inspect output", ErrContainerNotFound, id)
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
// label or status filter. It retains the list-time generation and state
// so a later name-addressed delete can be revalidated under the name lock.
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
	return exactImageMissingFor(err, "apple")
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

// nameConflict matches Apple Container's duplicate-name wording.
func (appleEngine) nameConflict(err error) bool {
	return exactNameConflictFor(err, "apple")
}

// containerMissing matches a CLI failure for an absent container.
func (appleEngine) containerMissing(err error) bool {
	return exactContainerNotFoundFor(err, "apple")
}
