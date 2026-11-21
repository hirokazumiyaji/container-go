package container

import (
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// requiresImmutableID reports whether the backend must never fall back to a
// logical name when its immutable identity is unavailable.
func requiresImmutableID(eng engine) bool {
	if eng == nil {
		return false
	}
	return eng.capabilities().immutableID
}

func validCreationID(generation string) bool {
	return domain.IsCreationID(generation)
}

func validImmutableID(eng engine, id string) bool {
	if requiresImmutableID(eng) {
		return domain.IsImmutableContainerID(id)
	}
	return id == ""
}

// ownershipObserved projects an inspect result onto the pure ownership
// policy's input. Callers pass a non-nil info.
func ownershipObserved(info *engineInfo) domain.OwnershipObserved {
	return domain.OwnershipObserved{UID: info.uid, Labels: info.labels}
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
			return "", identityError(fmt.Sprintf("%s inspect returned no valid immutable container ID", eng.name()))
		}
		return info.uid, nil
	}
	if fallback == "" {
		return "", identityError("container has no verified logical name")
	}
	return fallback, nil
}

// sameContainerIdentity verifies the generation and backend identity of
// two inspect results. A missing or malformed generation or backend identity
// on either side is never treated as a wildcard. The comparison rule lives in
// domain.VerifyOwnership; this shell keeps the operation-specific diagnostics.
func sameContainerIdentity(eng engine, expected, fresh *engineInfo) error {
	if expected == nil || fresh == nil {
		return identityError("inspect returned no container identity")
	}
	// Verify the expectation before comparing. VerifyOwnership only makes
	// an empty Creation or UID meaningful when the caller did not request a
	// specific value, so an empty expected fact would otherwise match any
	// well-formed observation.
	expectedGeneration := expected.labels[creationLabel]
	if !validCreationID(expectedGeneration) {
		return identityError("creation generation is missing or invalid")
	}
	if requiresImmutableID(eng) {
		if !validImmutableID(eng, expected.uid) {
			return identityError(fmt.Sprintf("%s inspect ID is missing or invalid", eng.name()))
		}
	} else if expected.uid != "" {
		return identityError("backend identity changed")
	}
	expectedFacts := domain.OwnershipExpected{
		Creation:        expectedGeneration,
		RequireCreation: true,
	}
	if requiresImmutableID(eng) {
		expectedFacts.UID = expected.uid
		expectedFacts.RequireImmutableID = true
	} else {
		expectedFacts.RequireNoImmutableID = true
	}
	switch domain.VerifyOwnership(expectedFacts, ownershipObserved(fresh)) {
	case domain.OwnershipFailureNone:
		return nil
	case domain.OwnershipFailureCreation:
		if !validCreationID(fresh.labels[creationLabel]) {
			return identityError("creation generation is missing or invalid")
		}
		return identityError("creation generation changed")
	case domain.OwnershipFailureIdentity:
		if requiresImmutableID(eng) {
			if !validImmutableID(eng, fresh.uid) {
				return identityError(fmt.Sprintf("%s inspect ID is missing or invalid", eng.name()))
			}
			return identityError("immutable container ID changed")
		}
		return identityError("backend identity changed")
	default:
		return identityError("container identity changed")
	}
}
