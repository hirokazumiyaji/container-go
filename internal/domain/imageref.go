package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ImageRef is an image reference split into its repository name, tag, and
// digest. Name keeps the caller's registry/repository spelling; tag and digest
// are optional.
type ImageRef struct {
	Name   string
	Tag    string
	Digest string
}

// ParseImageRef splits an image reference into its repository, tag, and
// digest. The digest is separated first, so the ":" inside "@sha256:..." can
// never be mistaken for a tag separator. A missing tag is left empty for the
// caller's defaulting rule; a trailing ":" or "@" is a malformed separator,
// not a missing component.
func ParseImageRef(raw string) (ImageRef, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return ImageRef{}, errors.New("image reference is empty")
	}
	name, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest && digest == "" {
		return ImageRef{}, fmt.Errorf("invalid image reference %q: empty digest", raw)
	}
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
		if tag == "" {
			return ImageRef{}, fmt.Errorf("invalid image reference %q: empty tag", raw)
		}
	}
	if name == "" {
		return ImageRef{}, fmt.Errorf("invalid image reference %q", raw)
	}
	return ImageRef{Name: name, Tag: tag, Digest: digest}, nil
}

// NormalizeImageRef expands Docker Hub short names to a canonical
// registry/repo:tag form. The default registry is docker.io, the
// default namespace for single-component repos is library, and the
// default tag is latest. A digest suffix is preserved after the tag so a
// reference such as "redis:7@sha256:..." stays a digest-pinned reference.
func NormalizeImageRef(ref string) string {
	parsed, err := ParseImageRef(ref)
	if err != nil {
		return ref
	}
	tag := parsed.Tag
	if tag == "" {
		tag = "latest"
	}
	name := parsed.Name
	var registry, repo string
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 1:
		registry = "docker.io"
		repo = "library/" + parts[0]
	case len(parts) == 2 && !IsRegistry(parts[0]):
		registry = "docker.io"
		repo = name
	case IsRegistry(parts[0]):
		registry = CanonicalDockerHubRegistry(parts[0])
		repo = strings.Join(parts[1:], "/")
		if registry == "docker.io" && !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	default:
		registry = "docker.io"
		repo = name
	}
	if repo == "" {
		repo = name
	}
	normalized := registry + "/" + repo + ":" + tag
	if parsed.Digest != "" {
		normalized += "@" + parsed.Digest
	}
	return normalized
}

// IsRegistry reports whether s looks like a hostname/registry prefix.
func IsRegistry(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":") || s == "localhost"
}

// CanonicalDockerHubRegistry normalizes registry aliases for Docker Hub to
// "docker.io" and lowercases every registry hostname: registry names are
// case-insensitive DNS names, so GHCR.IO and ghcr.io address the same store.
func CanonicalDockerHubRegistry(registry string) string {
	switch strings.ToLower(registry) {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "docker.io"
	default:
		return strings.ToLower(registry)
	}
}

// ImageDigest returns the digest portion of an image reference, or empty string if absent.
func ImageDigest(ref string) string {
	i := strings.Index(ref, "@")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}

// StripImageDigest returns the reference without its @sha256:... digest.
func StripImageDigest(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// IsHex checks if s contains only hexadecimal characters.
func IsHex(s string) bool {
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

// isDigestShaped reports whether ref has the shape of a digest without a
// repository: a known algorithm prefix and an encoded value. It is the
// provenance test, so a malformed encoded value is still recognized as not
// being a repository reference.
func isDigestShaped(ref string) bool {
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

// IsBareImageID recognizes the unprefixed 64-hex image-ID spelling.
func IsBareImageID(ref string) bool {
	ref = strings.TrimSpace(ref)
	return len(ref) == 64 && IsHex(ref)
}

// IsBareImageReference identifies either spelling of an ID-shaped or
// digest-shaped value without repository provenance. A digest-shaped value is
// rejected here even when its encoded portion is malformed: without a
// repository it still cannot be compared with a named image.
func IsBareImageReference(ref string) bool {
	return isDigestShaped(ref) || IsBareImageID(ref)
}

// ImageReferenceBase returns a repository-bearing base for an image reference.
func ImageReferenceBase(ref string) string {
	base := strings.TrimSpace(StripImageDigest(ref))
	if base == "" || IsBareImageReference(base) {
		return ""
	}
	return base
}

// ImageRepositoryBaseWithoutTag strips any trailing tag from an image base.
func ImageRepositoryBaseWithoutTag(base string) string {
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		return base[:i]
	}
	return base
}

// ImageRepository returns the canonical repository portion of an image reference,
// discarding any tag or digest.
func ImageRepository(ref string) string {
	base := ImageReferenceBase(ref)
	if base == "" {
		return ""
	}
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		base = base[:i]
	}
	return strings.TrimSuffix(NormalizeImageRef(base), ":latest")
}

// ImagesCompatible reports whether two image references identify the same image.
func ImagesCompatible(requested, actual string) bool {
	if requested == "" || actual == "" {
		return requested == actual
	}
	if IsBareImageReference(requested) || IsBareImageReference(actual) {
		return false
	}
	if requested == actual {
		return true
	}
	reqDigest := ImageDigest(requested)
	actDigest := ImageDigest(actual)
	if reqDigest != "" {
		if !strings.EqualFold(reqDigest, actDigest) {
			return false
		}
		// A matching digest pins the same content, so the advisory tag
		// on either side must not make the references incompatible.
		// The repository still has to be the same.
		reqRepo := ImageRepository(requested)
		actRepo := ImageRepository(actual)
		return reqRepo != "" && strings.EqualFold(reqRepo, actRepo)
	}
	if actDigest != "" {
		// The inspected side pins content without naming a tag. Its absent
		// tag is not an implicit ":latest", so the requested tag cannot be
		// compared with it; only an explicitly tagged digest reference (for
		// example "redis:7@sha256:...") keeps its tag claim.
		if _, tag, _ := ImageRepositoryParts(actual); tag == "" {
			return false
		}
	}
	req := ImageReferenceBase(requested)
	act := ImageReferenceBase(actual)
	return req != "" && act != "" && NormalizeImageRef(req) == NormalizeImageRef(act)
}

// ImageRepositoryPathsCompatible compares the repository path and tag
// while allowing one side to be an unqualified backend-default name.
func ImageRepositoryPathsCompatible(a, b string) bool {
	aName, aTag, aExplicit := ImageRepositoryParts(a)
	bName, bTag, bExplicit := ImageRepositoryParts(b)
	if aName == "" || bName == "" {
		return false
	}
	if aExplicit && bExplicit {
		ra, rb := ImageRegistryOf(a), ImageRegistryOf(b)
		if ra == "" || rb == "" || !strings.EqualFold(ra, rb) {
			return false
		}
	}
	if !TagsCompatible(aTag, bTag) {
		return false
	}
	if aExplicit && bExplicit {
		return aName == bName
	}
	if aExplicit {
		return repositoryNameMatches(bName, aName)
	}
	if bExplicit {
		return repositoryNameMatches(aName, bName)
	}
	return NormalizeImageRef(StripImageDigest(a)) == NormalizeImageRef(StripImageDigest(b))
}

// ImageRegistryOf returns the registry of an image reference if explicit.
// A registry prefix only exists before a repository path; a single-component
// name such as "my.image" is a Docker Hub repository, not a registry.
func ImageRegistryOf(ref string) string {
	base := ImageRepositoryBaseWithoutTag(ImageReferenceBase(ref))
	if base == "" {
		return ""
	}
	first, _, found := strings.Cut(base, "/")
	if !found || !IsRegistry(first) {
		return ""
	}
	return CanonicalDockerHubRegistry(first)
}

// ImageRepositoryParts returns the repo name, tag, and whether the registry is explicit.
func ImageRepositoryParts(ref string) (name, tag string, explicit bool) {
	base := ImageReferenceBase(ref)
	if base == "" {
		return "", "", false
	}
	name = base
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 && IsRegistry(parts[0]) {
		explicit = true
		registry := CanonicalDockerHubRegistry(parts[0])
		name = strings.Join(parts[1:], "/")
		if registry == "docker.io" && !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	return name, tag, explicit
}

// TagsCompatible checks whether two tags are compatible (empty defaults to latest).
func TagsCompatible(a, b string) bool {
	if a == "" {
		a = "latest"
	}
	if b == "" {
		b = "latest"
	}
	return a == b
}

func repositoryNameMatches(unqualified, qualified string) bool {
	if unqualified == qualified {
		return true
	}
	parts := strings.Split(unqualified, "/")
	if len(parts) == 1 {
		return qualified == unqualified || qualified == "library/"+unqualified
	}
	return false
}
