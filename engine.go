package container

import (
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// engineInfo is the backend-neutral view of one inspected container.
type engineInfo struct {
	state  State
	labels map[string]string
	// uid is the backend-assigned immutable identity (Docker's 64-hex
	// Id). Empty when the backend addresses containers by name only
	// (Apple Container), where a delete cannot be bound to a generation.
	uid string
	// image is the image reference the container was created from.
	image string
	// ip is the container's address on its first network; empty when
	// the backend did not report one.
	ip string
	// bound lists host-side bindings of container ports, as reported
	// by the backend (Docker's randomly assigned ports land here).
	bound []boundPort
}

// pruneCandidate is the identity-bearing subset of a list result. Apple
// Container addresses containers by name, so a later inspect must prove
// that the same generation and lifecycle state is still present before a
// name-based delete is attempted.
type pruneCandidate struct {
	id         string
	creation   string
	state      State
	managed    bool
	reuseGroup string
}

func verifiedImmutableID(eng engine, id string) bool {
	return eng.immutableID() && dockerIDRE.MatchString(id)
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
	checkConfig(cfg *config) error
	runArgs(cfg *config, image, envFile string) []string
	// parseRunID extracts the immutable container ID from run output;
	// empty when the backend has none (Apple Container prints the name).
	parseRunID(stdout []byte) string
	// immutableID reports whether backend inspection/deletes can address a
	// specific generation without a name lock.
	immutableID() bool
	// nameAddressedDeletes reports whether deletes target a name rather
	// than an immutable backend ID. Name-addressed paths must use the
	// guarded create/inspect/delete protocol.
	nameAddressedDeletes() bool
	inspectArgs(id string) []string
	parseInspect(data []byte, id string) (*engineInfo, error)
	stopArgs(id string, timeout *time.Duration) []string
	deleteArgs(id string) []string
	copyToArgs(id, hostPath, containerPath string) []string
	copyFromArgs(id, containerPath, hostPath string) []string
	execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string
	logsArgs(id string, follow bool) []string
	// logsTailArgs fetches a bounded tail for diagnostics without
	// pulling the full log stream.
	logsTailArgs(id string) []string
	listArgs() []string
	// parseStoppedManaged extracts, from listArgs output, the stopped
	// managed containers this library created, including the metadata
	// needed to revalidate a name-addressed delete.
	parseStoppedManaged(data []byte) ([]pruneCandidate, error)
	// listReuseGroupArgs lists every container tagged with the reuse
	// group label, including running ones.
	listReuseGroupArgs(group string) []string
	// parseReuseGroupIDs extracts containers from listReuseGroupArgs
	// output that carry the given reuse group, with their list-time
	// identity metadata.
	parseReuseGroupIDs(data []byte, group string) ([]pruneCandidate, error)
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
