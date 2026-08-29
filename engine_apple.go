package container

import (
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

func (appleEngine) name() string   { return "apple" }
func (appleEngine) binary() string { return "container" }
func (appleEngine) directIP() bool { return true }

func (appleEngine) defaultHost() string { return "127.0.0.1" }

func (appleEngine) probe() cli.Probe {
	return cli.Probe{Args: []string{"system", "status"}, Hint: "run `container system start`"}
}

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	for _, k := range sortedKeys(cfg.allLabels()) {
		args = append(args, "--label", k+"="+cfg.allLabels()[k])
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, p := range cfg.published {
		args = append(args, "--publish", p.raw)
	}
	for _, m := range cfg.mounts {
		args = append(args, "--mount", m.arg())
	}
	if cfg.cpus > 0 {
		args = append(args, "--cpus", strconv.Itoa(cfg.cpus))
	}
	if cfg.memory != "" {
		args = append(args, "--memory", cfg.memory)
	}
	if cfg.user != "" {
		args = append(args, "--user", cfg.user)
	}
	if cfg.workdir != "" {
		args = append(args, "--workdir", cfg.workdir)
	}
	if cfg.network != "" {
		args = append(args, "--network", cfg.network)
	}
	if cfg.platform != "" {
		args = append(args, "--platform", cfg.platform)
	}
	if cfg.entrypoint != "" {
		args = append(args, "--entrypoint", cfg.entrypoint)
	}
	args = append(args, image)
	return append(args, cfg.cmd...)
}

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

func (appleEngine) imageInspectArgs(image string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image string) []string {
	return []string{"image", "pull", image}
}

// imageMissing matches the CLI's error for an absent image; the images
// plugin reports ContainerizationError(.notFound).
func (appleEngine) imageMissing(err error) bool {
	var cliErr *cli.CLIError
	return errors.As(err, &cliErr) && strings.Contains(strings.ToLower(cliErr.Stderr), "not found")
}

func (appleEngine) parseImageExists(data []byte) bool {
	images, err := inspect.Decode(data)
	if err != nil {
		return false
	}
	return len(images) > 0
}
