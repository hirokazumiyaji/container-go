package container

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Apple Container parses stop --time as a signed 32-bit integer, even when
// the library itself runs on a 64-bit host.
const maxAppleStopSeconds int64 = math.MaxInt32

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	return append(args, cfg.commonRunArgs(image, envFile, nil)...)
}

func (appleEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

func (appleEngine) startArgs(id string) []string {
	return startArgsFor(id)
}

func (appleEngine) killArgs(id string, signal string) []string {
	return killArgsFor(id, signal)
}

func (appleEngine) stopArgs(id string, timeout *time.Duration) ([]string, error) {
	return stopArgsFor(id, timeout, maxAppleStopSeconds)
}

func (appleEngine) deleteArgs(id string) []string {
	return []string{"delete", "--force", id}
}

func (appleEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return copyToArgsFor(id, hostPath, containerPath)
}

func (appleEngine) reaperSubcommand() string { return "delete" }

func (appleEngine) reaperDeleteFlags() []string { return nil }

func (appleEngine) execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string {
	return execArgsFor(id, cfg, envFile, cmd)
}

func (appleEngine) logsFollowArgs(id string) []string {
	return logsFollowArgsFor(id)
}

// logsFollowArgsWithOptions builds the follow argv for caller options. Apple
// Container maps Tail to -n and has no --since or --timestamps equivalent, so
// those options are rejected rather than silently ignored.
func (appleEngine) logsFollowArgsWithOptions(id string, opts FollowLogsOptions) ([]string, error) {
	if !opts.Since.IsZero() {
		return nil, fmt.Errorf("%w: apple backend does not support logs since", ErrUnsupportedCapability)
	}
	if opts.Timestamps {
		return nil, fmt.Errorf("%w: apple backend does not support log timestamps", ErrUnsupportedCapability)
	}
	args := []string{"logs", "--follow"}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	}
	return append(args, id), nil
}

// logsFollowResumeArgs ignores since: Apple Container truncates stdio.log
// when a container starts, so following from the beginning of the current
// log replays only the replacement process's output. Apple has no --since.
func (e appleEngine) logsFollowResumeArgs(id string, _ time.Time) []string {
	return e.logsFollowArgs(id)
}

func (appleEngine) logsArgsWithOptions(id string, opts LogsOptions) ([]string, error) {
	if !opts.Since.IsZero() {
		return nil, fmt.Errorf("%w: apple backend does not support logs since", ErrUnsupportedCapability)
	}
	if opts.Timestamps {
		return nil, fmt.Errorf("%w: apple backend does not support log timestamps", ErrUnsupportedCapability)
	}
	args := []string{"logs"}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	}
	return append(args, id), nil
}

func (appleEngine) logsTailArgs(id string) []string {
	return []string{"logs", "-n", "1000", id}
}

func (appleEngine) listArgs() []string {
	return []string{"ls", "--all", "--format", "json"}
}

func (appleEngine) imageInspectArgs(image, platform string) []string {
	return imageInspectArgsFor(image, platform, false)
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	return pullImageArgsFor([]string{"image", "pull"}, image, platform)
}

func (appleEngine) listReuseGroupArgs(string) []string {
	return []string{"ls", "--all", "--format", "json"}
}
