package container

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Docker parses stop --time through a signed integer. Cap the value at
// MaxInt32 so the argument is safe for 32-bit Docker CLIs as well as 64-bit
// ones; the daemon's duration conversion is also safe at this limit.
const maxDockerStopSeconds int64 = math.MaxInt32

func (e dockerEngine) runArgs(cfg *config, image, envFile string) []string {
	// The pull policy fetches the image beforehand; --pull=never keeps
	// the run command from pulling a second time behind our back.
	args := []string{"run", "--detach", "--pull", "never", "--name", cfg.name}
	// Publish every declared port the user did not publish explicitly
	// to a daemon-assigned port. Locally this binds loopback; on a
	// remote daemon it binds all interfaces so the client can reach it
	// via defaultHost().
	bindAddr := "127.0.0.1"
	if e.isRemote() {
		bindAddr = "0.0.0.0"
	}
	// checkConfig rejects these combinations before Run reaches this
	// method. Keep the argv builder safe for direct backend tests too:
	// host and none networks must never receive a -p flag.
	runCfg := cfg
	if !cfg.networkExplicit {
		copy := *cfg
		copy.network = ""
		runCfg = &copy
	}
	publishable := dockerNetworkAllowsPublish(runCfg.network)
	if !publishable {
		copy := *runCfg
		copy.published = nil
		runCfg = &copy
	}
	var extraPublish []string
	if publishable {
		for _, spec := range cfg.exposed {
			published := false
			for _, p := range cfg.published {
				if p.ContainerPort == spec.Port && p.Proto == spec.Proto {
					published = true
					break
				}
			}
			if !published {
				extraPublish = append(extraPublish, bindAddr+"::"+spec.String())
			}
		}
	}
	return append(args, runCfg.commonRunArgs(image, envFile, extraPublish)...)
}

func (dockerEngine) inspectArgs(id string) []string {
	// Docker resolves an unqualified target across object types by default.
	// Restrict the CLI lookup to containers so a network or volume cannot
	// shadow the requested container name.
	return []string{"inspect", "--type=container", id}
}

func (dockerEngine) startArgs(id string) []string {
	return startArgsFor(id)
}

func (dockerEngine) restartArgs(id string, timeout *time.Duration) ([]string, error) { //nolint:unparam // part of the lifecycle role signature
	if timeout == nil {
		return []string{"restart", id}, nil
	}
	secs, err := stopTimeoutSeconds(*timeout, maxDockerStopSeconds)
	if err != nil {
		return nil, fmt.Errorf("restart timeout: %w", err)
	}
	return []string{"restart", "-t", strconv.FormatInt(secs, 10), id}, nil
}

func (dockerEngine) killArgs(id string, signal string) []string {
	return killArgsFor(id, signal)
}

func (dockerEngine) pauseArgs(id string) ([]string, error) {
	return []string{"pause", id}, nil
}

func (dockerEngine) unpauseArgs(id string) ([]string, error) {
	return []string{"unpause", id}, nil
}

func (dockerEngine) stopArgs(id string, timeout *time.Duration) ([]string, error) {
	return stopArgsFor(id, timeout, maxDockerStopSeconds)
}

func (dockerEngine) deleteArgs(id string) []string {
	return []string{"rm", "--force", dockerDeleteVolumesFlag, id}
}

func (dockerEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return copyToArgsFor(id, hostPath, containerPath)
}

func (dockerEngine) copyFromArgs(id, containerPath, hostPath string) []string {
	return copyFromArgsFor(id, containerPath, hostPath)
}

func (dockerEngine) reaperSubcommand() string { return "rm" }

func (dockerEngine) reaperDeleteFlags() []string { return []string{dockerDeleteVolumesFlag} }

func (dockerEngine) execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string {
	return execArgsFor(id, cfg, envFile, cmd)
}

func (dockerEngine) logsFollowArgs(id string) []string {
	return logsFollowArgsFor(id)
}

// logsFollowArgsWithOptions builds the follow argv for caller options. Docker
// accepts --tail, --since, and --timestamps together with --follow.
func (dockerEngine) logsFollowArgsWithOptions(id string, opts FollowLogsOptions) ([]string, error) {
	return dockerLogsFollowArgs(id, opts), nil
}

func dockerLogsFollowArgs(id string, opts FollowLogsOptions) []string {
	args := []string{"logs", "--follow"}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		args = append(args, "--since", opts.Since.UTC().Format(time.RFC3339Nano))
	}
	if opts.Timestamps {
		args = append(args, "--timestamps")
	}
	return append(args, id)
}

// logsFollowResumeArgs starts the replacement stream at since. Docker keeps
// the full log across restarts, so following without --since would replay
// every line the previous stream already wrote to the output. since is a
// client-clock instant; a remote daemon must be time-synchronized with the
// client or the boundary can drop/replay output.
func (dockerEngine) logsFollowResumeArgs(id string, since time.Time) []string {
	return dockerLogsFollowArgs(id, FollowLogsOptions{Since: since})
}

// logsArgsWithOptions builds the snapshot argv. --since uses the same UTC
// RFC3339Nano spelling as logsFollowResumeArgs: a coarse timestamp would
// replay entries from the same second that an earlier snapshot already
// delivered, which misleads incremental readers such as ForLog counting.
func (dockerEngine) logsArgsWithOptions(id string, opts LogsOptions) ([]string, error) {
	args := []string{"logs"}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		args = append(args, "--since", opts.Since.UTC().Format(time.RFC3339Nano))
	}
	if opts.Timestamps {
		args = append(args, "--timestamps")
	}
	return append(args, id), nil
}

// logsTailArgs bounds diagnostics at the CLI: last 1000 lines, then
// trimmed to logTailLimit bytes in Go with a fixed-size ring.
func (dockerEngine) logsTailArgs(id string) []string {
	return []string{"logs", "--tail", "1000", id}
}

// listArgs filters daemon-side; the Docker CLI supports label and
// status filters directly. Docker treats repeated status filters as
// alternatives, so both exited and dead are selected without admitting
// created or running containers. Output is full container IDs so prune
// deletes by immutable ID rather than a mutable name.
func (dockerEngine) listArgs() []string {
	return []string{
		"ps", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=" + managedLabel + "=true",
		"--filter", "status=exited",
		"--filter", "status=dead",
	}
}

func (dockerEngine) imageInspectArgs(image, platform string) []string {
	return imageInspectArgsFor(image, platform, true)
}

func (dockerEngine) pullImageArgs(image, platform string) []string {
	return pullImageArgsFor([]string{"pull"}, image, platform)
}

// listReuseGroupArgs requests full container IDs: group prune verifies and
// deletes by immutable ID, so names or truncated IDs would be skipped.
func (dockerEngine) listReuseGroupArgs(group string) []string {
	return []string{
		"ps", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=" + reuseGroupLabel + "=" + group,
	}
}
