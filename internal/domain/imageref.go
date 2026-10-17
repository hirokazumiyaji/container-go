package domain

import (
	"strings"
)

// NormalizeImageRef expands Docker Hub short names to a canonical
// registry/repo:tag form. The default registry is docker.io, the
// default namespace for single-component repos is library, and the
// default tag is latest.
func NormalizeImageRef(ref string) string {
	tag := "latest"
	name := ref
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
		if tag == "" {
			tag = "latest"
		}
	}
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
	return registry + "/" + repo + ":" + tag
}

// IsRegistry reports whether s looks like a hostname/registry prefix.
func IsRegistry(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":") || s == "localhost"
}

// CanonicalDockerHubRegistry normalizes registry aliases for Docker Hub to "docker.io".
func CanonicalDockerHubRegistry(registry string) string {
	switch strings.ToLower(registry) {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "docker.io"
	default:
		return registry
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

// isHex checks if s contains only hexadecimal characters.
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

// IsBareImageDigest recognizes a digest used without a repository.
func IsBareImageDigest(ref string) bool {
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
	return len(ref) == 64 && isHex(ref)
}

// IsBareImageReference identifies either spelling of an ID-shaped or digest-shaped value without repo.
func IsBareImageReference(ref string) bool {
	return IsBareImageDigest(ref) || IsBareImageID(ref)
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
		reqBase := ImageReferenceBase(requested)
		actBase := ImageReferenceBase(actual)
		return reqBase != "" && actBase != "" &&
			NormalizeImageRef(reqBase) == NormalizeImageRef(actBase)
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
func ImageRegistryOf(ref string) string {
	base := ImageRepositoryBaseWithoutTag(ImageReferenceBase(ref))
	if base == "" {
		return ""
	}
	first, _, _ := strings.Cut(base, "/")
	if !IsRegistry(first) {
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
	if len(parts) > 0 && IsRegistry(parts[0]) {
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
