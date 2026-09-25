package container

import (
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// imageIdentity is the backend's immutable image address when one is
// available. reference is the exact argument that should be passed to
// run; digest and id record the identities behind that argument. pinned
// is false only for the explicit mutable-tag fallback.
//
// id is intentionally reserved for a verified backend local image ID
// (currently Docker's Id). A caller-supplied sha256:... value is not an
// ID merely because it has that shape: Apple does not use Docker's image
// ID ABI, and a bare digest has no repository provenance.
type imageIdentity struct {
	reference string
	digest    string
	id        string
	pinned    bool
	// mismatch means the backend returned an identity for a different
	// image than the one requested. It must never be downgraded to the
	// mutable-tag fallback.
	mismatch bool
}

// engineInfo is the backend-neutral view of one inspected container.
type engineInfo struct {
	state  State
	labels map[string]string
	// uid is the backend-assigned immutable identity (Docker's 64-hex
	// Id). Empty when the backend addresses containers by name only
	// (Apple Container), where a delete cannot be bound to a generation.
	uid string
	// image is the image reference the container was created from. It
	// includes a digest when the backend reports one.
	image string
	// imageID is Docker's immutable local image ID when reported.
	imageID string
	// imageDigest is the OCI digest reported for the container image.
	imageDigest string
	// ip is the container's address on its first network; empty when
	// the backend did not report one.
	ip string
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
	checkConfig(cfg *config) error
	runArgs(cfg *config, image, envFile string) []string
	// parseRunID extracts the immutable container ID from run output;
	// empty when the backend has none (Apple Container prints the name).
	parseRunID(stdout []byte) string
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

// imageReferenceWithDigest builds the immutable reference used for run.
// A backend may report a canonical registry name, so the caller's name
// is retained as the base whenever possible. A bare digest or Docker
// image-ID-shaped value is not allowed to manufacture repository
// provenance; only a verified backend ID may stand alone.
func imageReferenceWithDigest(requested, reported, digest, id string) imageIdentity {
	digest = strings.TrimSpace(digest)
	id = strings.TrimSpace(id)
	if digest != "" {
		base := imageReferenceBase(requested)
		if base == "" {
			base = imageReferenceBase(reported)
		}
		if base == "" {
			// A verified backend ID may stand alone. This is the only
			// exception to the repository-provenance rule.
			if isImageID(id) && ((isImageID(requested) && strings.EqualFold(requested, id)) || strings.EqualFold(id, digest)) {
				return imageIdentity{reference: id, id: id, pinned: true}
			}
			return imageIdentity{}
		}
		if !validImageDigest(digest) || !imageRE.MatchString(base+"@"+digest) {
			return imageIdentity{}
		}
		return imageIdentity{reference: base + "@" + digest, digest: digest, id: id, pinned: true}
	}
	if requestedDigest := imageDigest(requested); validImageDigest(requestedDigest) {
		if imageReferenceBase(requested) == "" {
			return imageIdentity{}
		}
		return imageIdentity{reference: requested, digest: requestedDigest, id: id, pinned: true}
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
	if isImageID(a.id) && isImageID(b.id) && strings.EqualFold(a.id, b.id) {
		return true
	}
	if !validImageDigest(a.digest) || !validImageDigest(b.digest) || a.digest != b.digest {
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
	return imageRepository(abase) == imageRepository(bbase)
}

// isImageID recognizes Docker's content-addressed local image ID. The
// backend controls this value, but keeping it to a single safe token
// prevents malformed inspect output from becoming an image argument.
// It is not a general image-reference parser: sha256:<64hex> is also a
// valid bare digest, and Apple must not infer an ID from that shape.
func isImageID(ref string) bool {
	const prefix = "sha256:"
	return strings.HasPrefix(ref, prefix) && len(ref) == len(prefix)+64 &&
		isHex(strings.TrimPrefix(ref, prefix)) && imageRE.MatchString(ref)
}

// imageReferenceBase returns a repository-bearing base for an image
// reference. A bare Docker-style ID or digest is deliberately rejected:
// without a repository it cannot be compared with another named image.
func imageReferenceBase(ref string) string {
	base := strings.TrimSpace(stripImageDigest(ref))
	if base == "" || isBareImageReference(base) {
		return ""
	}
	return base
}

// isBareImageDigest recognizes a digest used without a repository, such
// as sha256:<64hex>. It is kept separate from isImageID because the
// latter is the Docker local-ID syntax used only after inspect verifies
// an Id field.
func isBareImageDigest(ref string) bool {
	ref = strings.TrimSpace(ref)
	if strings.Contains(ref, "@") || strings.Contains(ref, "/") {
		return false
	}
	i := strings.IndexByte(ref, ':')
	if i <= 0 || i == len(ref)-1 {
		return false
	}
	switch strings.ToLower(ref[:i]) {
	case "sha256", "sha384", "sha512":
		return true
	default:
		return false
	}
}

// isBareImageID recognizes the unprefixed 64-hex form Apple uses for
// ImageResource.id. It is a local identifier, not a repository name.
func isBareImageID(ref string) bool {
	ref = strings.TrimSpace(ref)
	return len(ref) == 64 && isHex(ref)
}

// isBareImageReference identifies either spelling of an ID-shaped or
// digest-shaped value that has no repository provenance.
func isBareImageReference(ref string) bool {
	return isBareImageDigest(ref) || isBareImageID(ref)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
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
	return len(encoded) == wantLength && isHex(encoded)
}
