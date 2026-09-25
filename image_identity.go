package container

import (
	"fmt"
	"strings"
)

// imageIdentity is the backend's immutable image address when one is
// available. reference is the exact argument that should be passed to
// run; digest and id record the identities behind that argument. pinned
// is false only for the explicit mutable-tag fallback.
//
// id is reserved for a verified backend local image ID (currently
// Docker's Id). A caller-supplied sha256:... value is not an ID merely
// because it has that shape: Apple does not use Docker's image ID ABI,
// and a bare digest has no repository provenance.
type imageIdentity struct {
	reference string
	digest    string
	id        string
	pinned    bool
	// platform and variantDigest describe an explicitly selected image
	// variant. digest remains the root/index identity used for run.
	platform      string
	variantDigest string
	// rootDigest is the registry root/index identity. It can differ from
	// variantDigest when Apple wraps a single manifest in a synthetic
	// local index.
	rootDigest string
	// repository is the canonical registry/repository key captured from a
	// backend-reported reference, when available.
	repository string
	// appleSynthetic marks an Apple-generated local index root that is not
	// itself registry-addressable by name@digest.
	appleSynthetic bool
	// mutableAlias marks an explicitly authorized mutable compatibility
	// state. Caller-supplied name@digest inputs are never downgraded to this
	// state.
	mutableAlias bool
	// notLocal means the backend returned a record but could not prove
	// that the requested platform variant is locally addressable.
	notLocal       bool
	notLocalReason string
	// mismatch means the backend returned a different image identity.
	mismatch bool
}

func imageReferenceWithDigest(requested, reported, digest, id string) imageIdentity {
	digest = strings.TrimSpace(digest)
	id = strings.TrimSpace(id)
	if digest != "" {
		base := imageReferenceBase(requested)
		if base == "" {
			base = imageReferenceBase(reported)
		}
		if base == "" {
			if isImageID(id) && ((isImageID(requested) && strings.EqualFold(requested, id)) || strings.EqualFold(id, digest)) {
				return imageIdentity{reference: id, id: id, pinned: true, rootDigest: digest}
			}
			return imageIdentity{}
		}
		if !validImageDigest(digest) || !imageRE.MatchString(base+"@"+digest) {
			return imageIdentity{}
		}
		return imageIdentity{reference: base + "@" + digest, digest: digest, rootDigest: digest, repository: imageRepository(base), id: id, pinned: true}
	}
	if requestedDigest := imageDigest(requested); validImageDigest(requestedDigest) {
		if imageReferenceBase(requested) == "" {
			return imageIdentity{}
		}
		return imageIdentity{reference: requested, digest: requestedDigest, rootDigest: requestedDigest, repository: imageRepository(requested), id: id, pinned: true}
	}
	if isImageID(id) {
		return imageIdentity{reference: id, id: id, pinned: true}
	}
	return imageIdentity{}
}

func imageIdentitiesCompatible(a, b imageIdentity) bool {
	if !a.pinned || !b.pinned {
		return false
	}
	// A matching verified backend image ID is sufficient. When IDs differ,
	// continue with the descriptor/repository comparison so older Docker
	// fixtures that expose an image ID separately from the container image
	// alias remain comparable.
	aID, aHasID := canonicalDockerImageID(a.id)
	bID, bHasID := canonicalDockerImageID(b.id)
	if aHasID && bHasID && strings.EqualFold(aID, bID) {
		return true
	}
	if !applePlatformMetadataCompatible(a.platform, b.platform) {
		return false
	}
	if a.variantDigest != "" || b.variantDigest != "" {
		if !validImageDigest(a.variantDigest) || !validImageDigest(b.variantDigest) ||
			!strings.EqualFold(a.variantDigest, b.variantDigest) {
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
	abase, bbase := imageReferenceBase(a.reference), imageReferenceBase(b.reference)
	if abase == "" || bbase == "" {
		return false
	}
	if a.repository != "" && b.repository != "" {
		return imageRepositoriesCompatible(a.repository, b.repository)
	}
	if a.repository != "" || b.repository != "" {
		return imageRepositoryPathsCompatible(abase, bbase)
	}
	return imageRepositoriesCompatible(imageRepository(abase), imageRepository(bbase))
}

// imageRootsCompatible checks the content/root identity independently of
// optional platform metadata. It is useful when a backend reports the root
// and selected manifest in separate fields.
//
//nolint:unused // retained as a focused identity predicate for backend adapters
func imageRootsCompatible(a, b imageIdentity) bool {
	if !a.pinned || !b.pinned {
		return false
	}
	aID, aHasID := canonicalDockerImageID(a.id)
	bID, bHasID := canonicalDockerImageID(b.id)
	if aHasID || bHasID {
		return aHasID && bHasID && strings.EqualFold(aID, bID)
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
	if a.repository != "" && b.repository != "" {
		return imageRepositoriesCompatible(a.repository, b.repository)
	}
	abase, bbase := imageReferenceBase(a.reference), imageReferenceBase(b.reference)
	return abase != "" && bbase != "" && imageRepositoryPathsCompatible(abase, bbase)
}

// requestedImageIdentitiesCompatible compares a resolved request with an
// inspected result. Backend-only metadata is additional information when the
// caller did not select it, while an explicit request must be represented in
// the inspected result.
func requestedImageIdentitiesCompatible(requested, actual imageIdentity) bool {
	requestedForComparison, actualForComparison := requested, actual
	// Some older backend inspect responses omit optional platform metadata.
	// Explicit compatibility checks still reject a present mismatch; absent
	// metadata cannot manufacture a mismatch during post-create verification.
	if requested.platform == "" || actualForComparison.platform == "" {
		requestedForComparison.platform = ""
		actualForComparison.platform = ""
	}
	if requested.variantDigest == "" || actualForComparison.variantDigest == "" {
		requestedForComparison.variantDigest = ""
		actualForComparison.variantDigest = ""
	}
	return imageIdentitiesCompatible(requestedForComparison, actualForComparison)
}

func applePlatformMetadataCompatible(a, b string) bool {
	if a == "" && b == "" {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	ap, aok := parseApplePlatformSelector(a)
	bp, bok := parseApplePlatformSelector(b)
	return aok && bok && applePlatformsEqual(ap, bp)
}

func isImageID(ref string) bool {
	const prefix = "sha256:"
	return strings.HasPrefix(ref, prefix) && len(ref) == len(prefix)+64 &&
		isHex(strings.TrimPrefix(ref, prefix)) && imageRE.MatchString(ref)
}

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

func isBareImageID(ref string) bool {
	ref = strings.TrimSpace(ref)
	return len(ref) == 64 && isHex(ref)
}

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

func mutableImageReference(image string, identity imageIdentity) imageIdentity {
	identity.reference = image
	identity.pinned = false
	identity.mutableAlias = false
	return identity
}

func containerImageIdentity(eng engine, info *engineInfo) imageIdentity {
	if info == nil {
		return imageIdentity{}
	}
	id := ""
	if eng != nil && eng.name() == "docker" {
		id = info.imageID
	}
	identity := imageFromInfo(info)
	if identity.pinned {
		identity.id = id
	}
	return identity
}

func verifyContainerImageIdentity(c *Container, info *engineInfo) error {
	if c == nil {
		return nil
	}
	expected := c.imageIdentity
	if !expected.pinned {
		expected = c.image
	}
	if !expected.pinned {
		return nil
	}
	observed := containerImageIdentity(c.eng, info)
	if !observed.pinned {
		return fmt.Errorf("%w: container %s did not report its pinned image identity", ErrImageIdentityUnavailable, c.id)
	}
	if !requestedImageIdentitiesCompatible(expected, observed) {
		return fmt.Errorf("%w: container %s image identity changed after create", ErrImageIdentityMismatch, c.id)
	}
	return nil
}

func imageRepository(ref string) string {
	base := imageReferenceBase(ref)
	if base == "" {
		return ""
	}
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		base = base[:i]
	}
	return strings.TrimSuffix(normalizeImageRef(base), ":latest")
}
