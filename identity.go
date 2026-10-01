package container

import "fmt"

// immutableIDBackend is an optional backend capability. Docker is the
// backend that must never fall back to a logical name when its immutable
// identity is unavailable.
type immutableIDBackend interface {
	requiresImmutableID() bool
}

func requiresImmutableID(eng engine) bool {
	if eng == nil {
		return false
	}
	if backend, ok := eng.(immutableIDBackend); ok {
		return backend.requiresImmutableID()
	}
	return eng.name() == "docker"
}

func validCreationID(generation string) bool {
	return creationRE.MatchString(generation)
}

func validImmutableID(eng engine, id string) bool {
	if requiresImmutableID(eng) {
		return dockerIDRE.MatchString(id)
	}
	return id == ""
}

// identityError is used when a backend operation cannot prove which
// container it is addressing. It deliberately uses the same public
// replacement sentinel as generation mismatches so callers fail closed
// without having to distinguish an unverified target from a replaced one.
func identityError(message string) error {
	return fmt.Errorf("%w: %s", ErrGenerationReplaced, message)
}

// verifiedDeleteTarget returns the only target that may be used for a
// destructive operation. Docker never falls back to a logical name: an
// inspected UID is required even when the operation started with a name.
func verifiedDeleteTarget(eng engine, info *engineInfo, fallback string) (string, error) {
	if requiresImmutableID(eng) {
		if info == nil || !validImmutableID(eng, info.uid) {
			return "", identityError("Docker inspect returned no valid immutable container ID")
		}
		return info.uid, nil
	}
	if fallback == "" {
		return "", identityError("container has no verified logical name")
	}
	return fallback, nil
}

// sameContainerIdentity verifies the generation and backend identity of
// two inspect results. A missing or malformed generation is never treated
// as a wildcard.
func sameContainerIdentity(eng engine, expected, fresh *engineInfo) error {
	if expected == nil || fresh == nil {
		return identityError("inspect returned no container identity")
	}
	expectedGeneration := expected.labels[creationLabel]
	freshGeneration := fresh.labels[creationLabel]
	if !validCreationID(expectedGeneration) || !validCreationID(freshGeneration) {
		return identityError("creation generation is missing or invalid")
	}
	if expectedGeneration != freshGeneration {
		return identityError("creation generation changed")
	}
	if requiresImmutableID(eng) {
		if !validImmutableID(eng, expected.uid) || !validImmutableID(eng, fresh.uid) {
			return identityError("Docker inspect ID is missing or invalid")
		}
		if expected.uid != fresh.uid {
			return identityError("immutable container ID changed")
		}
		return nil
	}
	if expected.uid != fresh.uid {
		return identityError("backend identity changed")
	}
	return nil
}
