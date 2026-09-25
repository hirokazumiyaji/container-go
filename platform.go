package container

import "strings"

type platformParts struct {
	os           string
	architecture string
	variant      string
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
// unconstrained. Actual platforms must report every selected component.
func platformSelectorMatches(selector, actual string) bool {
	want, ok := parsePlatform(selector)
	if !ok {
		return false
	}
	have, ok := parsePlatform(actual)
	if !ok {
		return false
	}
	return platformPartMatches(want.os, have.os) &&
		platformPartMatches(want.architecture, have.architecture) &&
		platformPartMatches(want.variant, have.variant)
}

func platformPartMatches(selector, actual string) bool {
	return selector == "" || strings.EqualFold(selector, actual)
}

func platformSelectorUnverifiable(selector, actual string) bool {
	want, ok := parsePlatform(selector)
	if !ok {
		return true
	}
	have, ok := parsePlatform(actual)
	if !ok {
		return true
	}
	if want.os != "" && have.os != "" && !strings.EqualFold(want.os, have.os) {
		return false
	}
	return (want.architecture != "" && have.architecture == "") ||
		(want.variant != "" && have.variant == "")
}

// dockerPlatformMatches treats omitted selector components as
// unconstrained, but requires Docker to report every explicitly selected
// component. In particular, an OS-only legacy inspect result cannot
// satisfy an architecture- or variant-specific request.
func dockerPlatformMatches(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
}
