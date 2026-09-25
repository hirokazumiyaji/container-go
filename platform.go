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

// dockerPlatformMatches accounts for `docker inspect` reporting Platform
// as the OS only. OS must still match; architecture and variant are compared
// when inspect reports them but otherwise remain unverified by this backend.
func dockerPlatformMatches(selector, actual string) bool {
	want, ok := parsePlatform(selector)
	if !ok {
		return false
	}
	have, ok := parsePlatform(actual)
	if !ok || !platformPartMatches(want.os, have.os) {
		return false
	}
	if have.architecture != "" && !platformPartMatches(want.architecture, have.architecture) {
		return false
	}
	if have.variant != "" && !platformPartMatches(want.variant, have.variant) {
		return false
	}
	return true
}
