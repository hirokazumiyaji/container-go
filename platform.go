package container

import "strings"

type platformParts struct {
	os           string
	architecture string
	variant      string
}

// platformMetadata preserves the presence of each OCI platform component.
// A plain normalized string cannot distinguish "the backend omitted the
// architecture" from "the backend reported an empty architecture"; both
// must fail closed when a selector names that component.
type platformMetadata struct {
	os           string
	architecture string
	variant      string
	osSet        bool
	archSet      bool
	variantSet   bool
	valid        bool
}

func emptyPlatformMetadata() platformMetadata {
	return platformMetadata{valid: true}
}

func platformMetadataFromParts(os, architecture, variant string, osSet, archSet, variantSet bool) platformMetadata {
	return platformMetadata{
		os:           os,
		architecture: architecture,
		variant:      variant,
		osSet:        osSet,
		archSet:      archSet,
		variantSet:   variantSet,
		valid:        true,
	}
}

func platformMetadataFromString(value string) platformMetadata {
	if value == "" {
		return emptyPlatformMetadata()
	}
	parts := strings.Split(value, "/")
	if len(parts) > 3 {
		return platformMetadata{}
	}
	meta := platformMetadata{valid: true, osSet: len(parts) >= 1}
	if len(parts) >= 1 {
		meta.os = parts[0]
	}
	if len(parts) >= 2 {
		meta.archSet = true
		meta.architecture = parts[1]
	}
	if len(parts) >= 3 {
		meta.variantSet = true
		meta.variant = parts[2]
	}
	return meta
}

func (m platformMetadata) normalized() string {
	if !m.valid || (!m.osSet && !m.archSet && !m.variantSet) {
		return ""
	}
	// Join through the highest reported component rather than skipping
	// empty values. For example, an OS-less arm64 result remains "/arm64".
	parts := make([]string, 0, 3)
	if m.osSet {
		parts = append(parts, m.os)
	}
	if m.archSet {
		if len(parts) == 0 {
			parts = append(parts, "")
		}
		parts = append(parts, m.architecture)
	}
	if m.variantSet {
		for len(parts) < 2 {
			parts = append(parts, "")
		}
		parts = append(parts, m.variant)
	}
	return strings.Join(parts, "/")
}

func (m platformMetadata) has(selector platformParts) (string, bool, bool) {
	// The second result says whether the selected components all matched;
	// the third says whether the observed metadata was structurally valid.
	if !m.valid {
		return "", false, false
	}
	if selector.os != "" {
		if !m.osSet || m.os == "" || !strings.EqualFold(selector.os, m.os) {
			return "", false, true
		}
	}
	if selector.architecture != "" {
		if !m.archSet || m.architecture == "" || !strings.EqualFold(selector.architecture, m.architecture) {
			return "", false, true
		}
	}
	if selector.variant != "" {
		if !m.variantSet || m.variant == "" || !strings.EqualFold(selector.variant, m.variant) {
			return "", false, true
		}
	}
	return "", true, true
}

func parsePlatform(value string) (platformParts, bool) {
	if value == "" {
		return platformParts{}, false
	}
	parts := strings.Split(value, "/")
	if len(parts) > 3 {
		return platformParts{}, false
	}
	var p platformParts
	for i, part := range parts {
		if part == "" {
			return platformParts{}, false
		}
		switch i {
		case 0:
			p.os = part
		case 1:
			p.architecture = part
		case 2:
			p.variant = part
		}
	}
	return p, true
}

// platformSelectorMatches treats omitted selector components as
// unconstrained. Actual platforms must report every selected component;
// an omitted or malformed component never becomes an implicit wildcard.
func platformSelectorMatches(selector, actual string) bool {
	if selector == "" {
		return true
	}
	return platformMetadataMatches(selector, platformMetadataFromString(actual))
}

func platformMetadataMatches(selector string, actual platformMetadata) bool {
	if selector == "" {
		return true
	}
	want, ok := parsePlatform(selector)
	if !ok {
		return false
	}
	_, matches, valid := actual.has(want)
	return valid && matches
}

//nolint:unused // retained for callers comparing individual platform components
func platformPartMatches(selector, actual string) bool {
	return selector == "" || strings.EqualFold(selector, actual)
}

func platformSelectorUnverifiable(selector, actual string) bool {
	if selector == "" {
		return false
	}
	want, ok := parsePlatform(selector)
	if !ok {
		return true
	}
	have := platformMetadataFromString(actual)
	_, matches, valid := have.has(want)
	return !valid || !matches
}

// dockerPlatformMatches uses the same fail-closed rule as the other
// backends. A legacy OS-only Docker inspect result cannot satisfy an
// architecture- or variant-specific request.
func dockerPlatformMatches(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
}

func enginePlatformCompatible(eng engine, selector string, info *engineInfo) bool {
	if info == nil {
		return false
	}
	if info.platformMeta.valid {
		return platformMetadataMatches(selector, info.platformMeta)
	}
	return eng.platformCompatible(selector, info.platform)
}

func sameEnginePlatform(a, b *engineInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.platformMeta.valid && b.platformMeta.valid {
		return a.platformMeta.osSet == b.platformMeta.osSet &&
			a.platformMeta.archSet == b.platformMeta.archSet &&
			a.platformMeta.variantSet == b.platformMeta.variantSet &&
			strings.EqualFold(a.platformMeta.os, b.platformMeta.os) &&
			strings.EqualFold(a.platformMeta.architecture, b.platformMeta.architecture) &&
			strings.EqualFold(a.platformMeta.variant, b.platformMeta.variant)
	}
	return a.platform == b.platform
}
