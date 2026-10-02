package container

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// engineInfo is the backend-neutral view of one inspected container.
type engineInfo struct {
	state  State
	labels map[string]string
	// uid is the backend-assigned immutable identity (Docker's 64-hex
	// Id). Empty when the backend addresses containers by name only
	// (Apple Container), where operations remain name-based.
	uid string
	// image is the image reference the container was created from.
	image string
	// ip is the container's address on its first network; empty when
	// the backend did not report one.
	ip string
	// networkMode is the backend's reported network mode. Docker uses
	// this to distinguish a real host binding from a request that the
	// daemon discarded (for example, -p with host networking).
	networkMode string
	// networkNames contains the actual attached network names reported by
	// Docker. It resolves the API's special "default" mode to the daemon's
	// concrete default network (bridge on Linux, nat on Windows).
	networkNames []string
	// defaultNetwork is the authoritative Docker daemon default identity
	// (bridge on Linux, nat on Windows). It is daemon metadata rather than
	// container identity, so it is not cached with the immutable fields.
	defaultNetwork string
	// bound lists host-side bindings of container ports, as reported
	// by the backend (Docker's randomly assigned ports land here).
	bound []boundPort
}

type boundPort struct {
	containerPort int
	proto         string
	hostAddr      string
	hostPort      int
}

// engine encapsulates what differs between container backends: how CLI
// argv vectors are built and how inspect output is read. Process
// execution, waiting, validation, and cleanup are shared.
type engine interface {
	name() string
	binary() string
	probe() cli.Probe
	// checkConfig rejects option combinations this backend cannot
	// honor before anything is created.
	checkConfig(ctx context.Context, cfg *config) error
	runArgs(cfg *config, image, envFile string) []string
	// parseRunID extracts the immutable container ID from run output;
	// empty when the backend has none (Apple Container prints the name).
	parseRunID(stdout []byte) string
	inspectArgs(id string) []string
	parseInspect(data []byte, id string) (*engineInfo, error)
	stopArgs(id string, timeout *time.Duration) ([]string, error)
	deleteArgs(id string) []string
	copyToArgs(id, hostPath, containerPath string) []string
	copyFromArgs(id, containerPath, hostPath string) []string
	// checkCopyFileFromContainer rejects backends whose copy-out cannot
	// preserve file types and reject links/special files before host open.
	checkCopyFileFromContainer() error
	// checkCopyFileFromContainerVersion verifies backend-specific
	// minimum versions before a copy-out creates a private temp directory.
	checkCopyFileFromContainerVersion(context.Context, cli.Runner) error
	execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string
	// logsFollowArgs builds the streaming follow argv.
	logsFollowArgs(id string) []string
	// logsArgsWithOptions builds snapshot args and rejects options the
	// backend cannot honor.
	logsArgsWithOptions(id string, opts LogsOptions) ([]string, error)
	// logsTailArgs fetches a bounded tail for diagnostics without
	// pulling the full log stream.
	logsTailArgs(id string) []string
	listArgs() []string
	// parseStoppedManaged extracts, from listArgs output, the IDs of
	// stopped containers this library created.
	parseStoppedManaged(data []byte) ([]string, error)
	// listReuseGroupArgs lists every container tagged with the reuse
	// group label, including running ones.
	listReuseGroupArgs(group string) []string
	// parseReuseGroupIDs extracts container IDs from listReuseGroupArgs
	// output that carry the given reuse group.
	parseReuseGroupIDs(data []byte, group string) ([]string, error)
	// nameConflict reports whether a failed run means the container
	// name is already taken by another create.
	nameConflict(err error) bool
	// reaperSubcommand is the delete subcommand the watchdog reaper
	// runs as `<binary> <subcommand> --force <id>`.
	reaperSubcommand() string
	// directIP reports whether clients connect straight to the
	// container IP (Apple Container) instead of published host ports
	// (Docker).
	directIP() bool
	// defaultHost is the client-facing host in published-port mode.
	defaultHost() string
	// imageInspectArgs inspects an image reference in the backend's
	// local store; failure with imageMissing means the image is absent.
	// When platform is set, the inspect targets that variant.
	imageInspectArgs(image, platform string) []string
	// pullImageArgs fetches the image into the local store. When
	// platform is set, only that variant is fetched.
	pullImageArgs(image, platform string) []string
	// imageMissing reports whether a failed image inspect means the
	// image is not in the local store.
	imageMissing(err error) bool
	// parseImageExists interprets image inspect output, considering the
	// requested platform variant when set.
	parseImageExists(data []byte, platform string) bool
}

func stopArgsFor(id string, timeout *time.Duration, maxSeconds int64) ([]string, error) {
	args := []string{"stop"}
	if timeout != nil {
		seconds, err := stopTimeoutSeconds(*timeout, maxSeconds)
		if err != nil {
			return nil, err
		}
		args = append(args, "--time", strconv.FormatInt(seconds, 10))
	}
	return append(args, id), nil
}

// stopTimeoutSeconds rounds up so the backend never grants less grace than
// the caller requested, then checks the rounded value against the backend's
// seconds limit. The result is int64 so the validation does not depend on the
// host architecture's native int width.
func stopTimeoutSeconds(timeout time.Duration, maxSeconds int64) (int64, error) {
	if timeout < 0 {
		return 0, fmt.Errorf("stop timeout must be non-negative: %s", timeout)
	}
	seconds := int64(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds > maxSeconds {
		return 0, fmt.Errorf("stop timeout %s exceeds backend limit of %d seconds", timeout, maxSeconds)
	}
	return seconds, nil
}
