package container

import (
	"errors"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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
	// imageDigest is the OCI root/index digest reported for the container
	// image.
	imageDigest string
	// imageVariantDigest is populated when the backend reports the selected
	// platform manifest separately from the root index.
	imageVariantDigest string
	// platform is the platform selected when the container was created.
	// Empty means the backend did not report it; it must not be inferred
	// from a root index digest.
	platform string
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

// imageCLIErrorForBackend returns a CLI error only when it came from the
// requested backend image operation. The binary and command checks keep an
// error from one backend from being classified as another backend's absence.
func imageCLIErrorForBackend(err error, backend string) (*cli.CLIError, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return nil, false
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	if binary := strings.ToLower(strings.TrimSpace(cliErr.Binary)); binary != "" && binary != backend {
		return nil, false
	}
	if len(cliErr.Args) == 0 {
		return cliErr, true
	}
	if len(cliErr.Args) < 2 {
		return nil, false
	}
	switch strings.ToLower(cliErr.Args[0]) {
	case "image":
		if len(cliErr.Args) < 3 {
			return nil, false
		}
		switch strings.ToLower(cliErr.Args[1]) {
		case "inspect", "pull":
		default:
			return nil, false
		}
	case "pull":
	default:
		return nil, false
	}
	return cliErr, true
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

func isRepositoryDigestReference(ref string) bool {
	return imageReferenceBase(ref) != "" && validImageDigest(imageDigest(ref))
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

// isBareImageID recognizes the unprefixed 64-hex image-ID spelling. It
// is a local identifier, not a repository name.
func isBareImageID(ref string) bool {
	ref = strings.TrimSpace(ref)
	return len(ref) == 64 && isHex(ref)
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
