package container

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// imageIdentity is the backend's verified image content identity when one
// is available. reference is the exact argument that should be passed to
// run; digest and id record the identities behind that argument. pinned
// records verified content identity, while mutableAlias marks a reference
// spelling that Run may execute only through the explicit mutable fallback.
//
// id is intentionally reserved for a verified backend local image ID
// (currently Docker's Id). A caller-supplied sha256:... value is not an
// ID merely because it has that shape: Apple does not use Docker's image
// ID ABI, and a bare digest has no repository provenance.
type imageIdentity struct {
	reference string
	// digest is the root/index identity used for the run reference. When
	// Apple selected a platform variant, variantDigest records the selected
	// manifest separately rather than replacing the root identity.
	digest string
	// rootDigest is kept separately from digest for callers that need to
	// distinguish a backend-selected manifest from the root index. Parsers
	// set both to the same value when no variant is selected.
	rootDigest string
	// repository is the backend-canonical repository identity. It preserves
	// a custom default registry reported by Apple without rewriting it to
	// Docker Hub during comparison.
	repository string
	id         string
	pinned     bool
	// platform and variantDigest describe an explicitly selected image
	// variant. digest/rootDigest remain the root/index descriptor used as
	// the content identity; the variant is validation metadata.
	platform      string
	variantDigest string
	// appleSynthetic marks a root descriptor synthesized by Apple around a
	// single manifest. That root is an implementation detail, not a
	// registry-addressable pull target; the resolver must try its local tag
	// only as an explicit mutable fallback.
	appleSynthetic bool
	// mutableAlias is retained for source compatibility with the internal
	// identity model. Apple name@digest references are immutable addresses,
	// so parsers no longer set it; it is reserved for an explicitly
	// authorized compatibility state.
	mutableAlias bool
	// notLocal means the backend returned an image record but could not
	// prove that the requested platform variant is locally addressable.
	// It is deliberately distinct from an absent image or an unavailable
	// identity.
	notLocal       bool
	notLocalReason string
	// mismatch means the backend returned an identity for a different
	// image than the one requested. It must never be downgraded to the
	// mutable-tag fallback.
	mismatch bool
}

// engineInfo is the backend-neutral view of one inspected container.
type engineInfo struct {
	state  State
	name   string
	labels map[string]string
	// uid is the backend-assigned immutable identity (Docker's 64-hex
	// Id). Empty when the backend addresses containers by name only
	// (Apple Container), where operations remain name-based.
	uid string
	// image is the image reference the container was created from. It
	// includes a digest when the backend reports one.
	image string
	// imageID is Docker's immutable local image ID when reported.
	imageID string
	// imageDigest is the OCI root/index digest reported for the container
	// image.
	imageDigest string
	// imageVariantDigest is populated when the backend reports the selected
	// platform manifest separately from the root index.
	imageVariantDigest string
	// platform is the platform reported by inspect, normalized when
	// possible. platformMeta retains whether each component was actually
	// reported so an incomplete value cannot silently become a wildcard.
	platform     string
	platformMeta platformMetadata
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
	bound      []boundPort
	exitCode   int
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time
}

// PortBinding describes a host-to-container port mapping.
type PortBinding struct {
	ContainerPort int
	Proto         string
	HostAddr      string
	HostPort      int
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
	startArgs(id string) []string
	restartArgs(id string, timeout *time.Duration) ([]string, error)
	killArgs(id string, signal string) []string
	pauseArgs(id string) ([]string, error)
	unpauseArgs(id string) ([]string, error)
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
	// parseStoppedManaged extracts, from listArgs output, the
	// identifiers of stopped containers this library created. The
	// value is whatever the backend's own deleteArgs accepts: Apple
	// returns container IDs, and Docker returns full IDs from
	// --no-trunc --format {{.ID}}.
	parseStoppedManaged(data []byte) ([]string, error)
	// listReuseGroupArgs lists every container tagged with the reuse
	// group label, including running ones.
	listReuseGroupArgs(group string) []string
	// parseReuseGroupIDs extracts, from listReuseGroupArgs output, the
	// identifiers of containers carrying the given reuse group. As
	// with parseStoppedManaged these are deleteArgs-compatible backend
	// identifiers, not necessarily immutable IDs.
	parseReuseGroupIDs(data []byte, group string) ([]string, error)
	// nameConflict reports whether a failed run means the container
	// name is already taken by another create.
	nameConflict(err error) bool
	nameConflictForTarget(err error, target string) bool
	// containerMissing reports whether a backend error specifically means
	// that the requested container is absent. It must use the CLI operation
	// and target rather than matching arbitrary "not found" text.
	containerMissing(err error) bool
	// reaperSubcommand is the delete subcommand the watchdog reaper
	// runs as `<binary> <subcommand> --force [backend flags] <id>`.
	reaperSubcommand() string
	// reaperDeleteFlags returns backend-specific delete options passed
	// after --force and before the container ID.
	reaperDeleteFlags() []string
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
	imageMissingForTarget(err error, target string) bool
	// parseImageExists interprets image inspect output, considering the
	// requested platform variant when set.
	parseImageExists(data []byte, platform string) bool
	// platformCompatible compares a requested platform selector with the
	// platform this backend reports from container inspect.
	platformCompatible(selector, actual string) bool
	// imageStoreID identifies the backend's image store so a presence
	// cache entry recorded against one daemon or default platform is not
	// reused against another (for Docker: DOCKER_HOST, DOCKER_CONTEXT,
	// DOCKER_CONFIG, DOCKER_DEFAULT_PLATFORM).
	imageStoreID() string
	// targetID returns a string distinguishing this backend target
	// (backend name and daemon endpoint/context) from others.
	targetID() string
	// isRemote reports whether the target daemon lives on another machine.
	isRemote() bool
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

// imageReferenceWithDigest builds the immutable reference used for run.
// A backend may report a canonical registry name, so the caller's name
// is retained as the base whenever possible. A bare digest or Docker
// image-ID-shaped value is not allowed to manufacture repository
// provenance; only a verified backend ID may stand alone.
func imageReferenceWithDigest(requested, reported, digest, id string) imageIdentity {
	digest = strings.TrimSpace(digest)
	id = strings.TrimSpace(id)
	if canonicalID, ok := canonicalDockerImageID(id); ok {
		id = canonicalID
	}
	if digest != "" {
		base := imageReferenceBase(requested)
		if base == "" {
			base = imageReferenceBase(reported)
		}
		if base == "" {
			// A verified backend ID may stand alone. This is the only
			// exception to the repository-provenance rule.
			if isImageID(id) && ((isImageID(requested) && strings.EqualFold(requested, id)) || strings.EqualFold(id, digest)) {
				return imageIdentity{reference: id, id: id, pinned: true, rootDigest: digest}
			}
			return imageIdentity{}
		}
		if !validImageDigest(digest) || !imageRE.MatchString(base+"@"+digest) {
			return imageIdentity{}
		}
		return imageIdentity{reference: base + "@" + digest, digest: digest, rootDigest: digest, id: id, pinned: true}
	}
	if requestedDigest := imageDigest(requested); validImageDigest(requestedDigest) {
		if imageReferenceBase(requested) == "" {
			return imageIdentity{}
		}
		return imageIdentity{reference: requested, digest: requestedDigest, rootDigest: requestedDigest, id: id, pinned: true}
	}
	if isImageID(id) {
		return imageIdentity{reference: id, id: id, pinned: true}
	}
	return imageIdentity{}
}

// imageIdentitiesCompatible compares only immutable identities. A
// mutable name or tag is not identity proof: a container can retain the
// tag it was created with after that tag is reassigned. A bare digest
// without repository provenance is also not proof; it is accepted only
// when both sides carry the same verified backend image ID.
func imageIdentitiesCompatible(a, b imageIdentity) bool {
	if !a.pinned || !b.pinned {
		return false
	}

	// A Docker local ID is a stronger identity than a registry digest. If
	// either side reports one, a missing or different ID is not compatible;
	// equal content digests must not hide a known-different local image.
	aID, aHasID := canonicalDockerImageID(a.id)
	bID, bHasID := canonicalDockerImageID(b.id)
	if aHasID || bHasID {
		return aHasID && bHasID && strings.EqualFold(aID, bID)
	}

	if !applePlatformMetadataCompatible(a.platform, b.platform) {
		return false
	}
	if a.variantDigest != "" && !validImageDigest(a.variantDigest) {
		return false
	}
	if b.variantDigest != "" && !validImageDigest(b.variantDigest) {
		return false
	}
	if a.variantDigest != "" || b.variantDigest != "" {
		if a.variantDigest == "" || b.variantDigest == "" || !strings.EqualFold(a.variantDigest, b.variantDigest) {
			return false
		}
	}

	aroot, broot := a.rootDigest, b.rootDigest
	if aroot == "" {
		aroot = a.digest
	}
	if broot == "" {
		broot = b.digest
	}
	if !validImageDigest(aroot) || !validImageDigest(broot) || !strings.EqualFold(aroot, broot) {
		return false
	}
	// A digest is content-addressed, but a reference also names the
	// repository from which that content was resolved. Do not accept a
	// same-digest identity from a different registry or namespace, and do
	// not accept a bare digest with no repository provenance.
	abase := imageReferenceBase(a.reference)
	bbase := imageReferenceBase(b.reference)
	if abase == "" || bbase == "" {
		return false
	}
	if a.repository != "" && b.repository != "" {
		return imageRepositoriesCompatible(a.repository, b.repository)
	}
	if a.repository != "" || b.repository != "" {
		// One side may be a caller spelling while the other carries the
		// backend's canonical registry. Compare the repository paths and
		// tags without accepting a bare digest as a wildcard.
		return imageRepositoryPathsCompatible(abase, bbase)
	}
	return imageRepositoriesCompatible(imageRepository(abase), imageRepository(bbase))
}

// requestedImageIdentitiesCompatible compares a resolved request with an
// inspected result. The request may omit platform metadata when the caller
// did not select a platform; metadata reported only by the inspected
// container is then additional information. Conversely, an explicit request
// must not be accepted when the actual result omits that metadata.
func requestedImageIdentitiesCompatible(requested, actual imageIdentity) bool {
	actualForComparison := actual
	if requested.platform == "" {
		actualForComparison.platform = ""
	}
	if requested.variantDigest == "" {
		actualForComparison.variantDigest = ""
	}
	return imageIdentitiesCompatible(requested, actualForComparison)
}

func applePlatformMetadataCompatible(a, b string) bool {
	if a == "" && b == "" {
		return true
	}
	if a == "" || b == "" {
		// A requested platform is not verifiable from metadata that omits
		// the selected platform. Fail closed rather than attaching an
		// unknown variant under a known root index.
		return false
	}
	ap, aok := parseApplePlatformSelector(a)
	bp, bok := parseApplePlatformSelector(b)
	return aok && bok && applePlatformsEqual(ap, bp)
}

// isImageID recognizes Docker's content-addressed local image ID. The
// backend controls this value, but keeping it to a single safe token
// prevents malformed inspect output from becoming an image argument.
// It is not a general image-reference parser: sha256:<64hex> is also a
// valid bare digest, and Apple must not infer an ID from that shape.
func isImageID(ref string) bool {
	const prefix = "sha256:"
	return strings.HasPrefix(ref, prefix) && len(ref) == len(prefix)+64 &&
		domain.IsHex(strings.TrimPrefix(ref, prefix)) && imageRE.MatchString(ref)
}

// imageReferenceBase returns a repository-bearing base for an image
// reference. A bare Docker-style ID or digest is deliberately rejected:
// without a repository it cannot be compared with another named image.
func imageReferenceBase(ref string) string {
	return domain.ImageReferenceBase(ref)
}

func isRepositoryDigestReference(ref string) bool {
	return imageReferenceBase(ref) != "" && validImageDigest(imageDigest(ref))
}

// isBareImageID recognizes the unprefixed 64-hex image-ID spelling. It
// is a local identifier, not a repository name.
func isBareImageID(ref string) bool {
	return domain.IsBareImageID(ref)
}

// canonicalDockerImageID normalizes Docker's accepted 64-hex and
// sha256:<64hex> ID spellings to the canonical run argument. It returns
// false for any other reference.
func canonicalDockerImageID(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if isImageID(ref) {
		return ref, true
	}
	if isBareImageID(ref) {
		return "sha256:" + ref, true
	}
	return "", false
}

// isBareImageReference identifies either spelling of an ID-shaped or
// digest-shaped value that has no repository provenance.
func isBareImageReference(ref string) bool {
	return domain.IsBareImageReference(ref)
}

func validImageDigest(digest string) bool {
	i := strings.IndexByte(digest, ':')
	if i <= 0 || i == len(digest)-1 {
		return false
	}
	algorithm := strings.ToLower(digest[:i])
	encoded := digest[i+1:]
	wantLength := 0
	switch algorithm {
	case "sha256":
		wantLength = 64
	case "sha384":
		wantLength = 96
	case "sha512":
		wantLength = 128
	default:
		return false
	}
	return len(encoded) == wantLength && domain.IsHex(encoded)
}

// parseInspectTime decodes a backend timestamp. The backends express "not
// set" as an empty string or the Go zero-value form; both map to the zero
// time, which is the documented unknown marker on InspectInfo. A value that
// matches neither accepted RFC 3339 layout is reported the same way: a
// cosmetic field must not fail an otherwise usable Inspect, and the library
// never fabricates an instant from a value it cannot read.
func parseInspectTime(s string) time.Time {
	switch s {
	case "", "0001-01-01T00:00:00Z":
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
	}
	if err != nil {
		return time.Time{}
	}
	return t
}
