package container

import "fmt"

// immutableIDBackend is optional so test and third-party engines that
// implement the existing engine interface are not broken. Docker is the
// backend that must never fall back to a name when identity is unknown.
type immutableIDBackend interface {
	requiresImmutableID() bool
}

func requiresImmutableID(eng engine) bool {
	if eng == nil {
		return false
	}
	if e, ok := eng.(immutableIDBackend); ok {
		return e.requiresImmutableID()
	}
	return eng.name() == "docker"
}

func validCreationID(generation string) bool {
	return creationRE.MatchString(generation)
}

func validImmutableID(eng engine, id string) bool {
	if !requiresImmutableID(eng) {
		return id == ""
	}
	return dockerIDRE.MatchString(id)
}

// verifiedDeleteTarget returns a safe deletion target for a fresh inspect.
// Docker is intentionally not allowed to fall back to a logical name: a
// missing or malformed inspect ID is not proof that the named object is
// still the one that was inspected.
func verifiedDeleteTarget(eng engine, info *engineInfo, fallback string) (string, error) {
	if requiresImmutableID(eng) {
		if info == nil || !validImmutableID(eng, info.uid) {
			return "", fmt.Errorf("refusing Docker name fallback: inspect returned no valid container ID")
		}
		return info.uid, nil
	}
	if fallback == "" {
		return "", fmt.Errorf("refusing empty container name delete")
	}
	return fallback, nil
}

// sameContainerIdentity verifies both the creation generation and the
// backend identity. The generation is always required, including on
// Docker, so a missing label cannot turn into an apparently successful
// adoption or delete. Docker additionally requires the same full ID.
func sameContainerIdentity(eng engine, expected, fresh *engineInfo) error {
	if expected == nil || fresh == nil {
		return fmt.Errorf("cannot verify container identity: inspect returned no identity")
	}
	expectedGeneration := expected.labels[creationLabel]
	freshGeneration := fresh.labels[creationLabel]
	if !validCreationID(expectedGeneration) {
		return fmt.Errorf("%w: expected creation generation is missing or invalid", ErrGenerationReplaced)
	}
	if !validCreationID(freshGeneration) {
		return fmt.Errorf("%w: live creation generation is missing or invalid", ErrGenerationReplaced)
	}
	if expectedGeneration != freshGeneration {
		return fmt.Errorf("%w: creation generation changed", ErrGenerationReplaced)
	}
	for _, key := range []string{managedLabel, reuseLabel} {
		if expected.labels[key] != "" && expected.labels[key] != fresh.labels[key] {
			return fmt.Errorf("%w: ownership label %s changed", ErrGenerationReplaced, key)
		}
	}

	if requiresImmutableID(eng) {
		if !validImmutableID(eng, expected.uid) || !validImmutableID(eng, fresh.uid) {
			return fmt.Errorf("cannot verify container identity: Docker inspect ID is missing or invalid")
		}
		if expected.uid != fresh.uid {
			return fmt.Errorf("%w: immutable container ID changed", ErrGenerationReplaced)
		}
		return nil
	}
	if expected.uid != fresh.uid {
		return fmt.Errorf("%w: backend identity changed", ErrGenerationReplaced)
	}
	return nil
}
