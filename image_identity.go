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
	// mutableAlias marks a caller-supplied Apple name@digest. Apple does
	// not expose an atomic run target for that spelling, so it is usable
	// only through the explicit mutable-tag compatibility option.
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

func imageIdentitiesCompatible(a, b imageIdentity) bool {
	if !a.pinned || !b.pinned {
		return false
	}
	if a.platform != "" && b.platform != "" && a.platform != b.platform {
		return false
	}
	if a.variantDigest != "" && b.variantDigest != "" &&
		!strings.EqualFold(a.variantDigest, b.variantDigest) {
		return false
	}
	if isImageID(a.id) && isImageID(b.id) && strings.EqualFold(a.id, b.id) {
		return true
	}
	if !validImageDigest(a.digest) || !validImageDigest(b.digest) || a.digest != b.digest {
		return false
	}
	abase := imageReferenceBase(a.reference)
	bbase := imageReferenceBase(b.reference)
	if abase == "" || bbase == "" {
		return false
	}
	return imageRepository(abase) == imageRepository(bbase)
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
	if validImageDigest(info.imageDigest) {
		id := ""
		if eng != nil && eng.name() == "docker" {
			id = info.imageID
		}
		return imageReferenceWithDigest(info.image, info.image, info.imageDigest, id)
	}
	if eng != nil && eng.name() == "docker" && isImageID(info.imageID) {
		return imageReferenceWithDigest(info.image, info.image, "", info.imageID)
	}
	return imageIdentity{}
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
	if !imageIdentitiesCompatible(expected, observed) {
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
