package domain

import "regexp"

// OwnershipExpected declares the ownership facts a caller requires of an
// inspected container. The zero value requires nothing, so each call site
// states only the facts its operation depends on.
type OwnershipExpected struct {
	// Creation requires the observed creation label to equal Creation
	// exactly. It implies a structurally valid generation.
	Creation string
	// RequireCreation requires a structurally valid observed generation
	// without pinning a specific value. Creation implies this.
	RequireCreation bool
	// Session requires the observed session label to equal Session.
	Session string
	// ReuseGroup requires the observed reuse-group label to equal
	// ReuseGroup.
	ReuseGroup string
	// UID requires the observed backend identity to equal UID.
	UID string
	// RequireManaged requires the managed marker.
	RequireManaged bool
	// RequireReuse requires the reuse marker.
	RequireReuse bool
	// RequireImmutableID requires a structurally valid immutable backend ID.
	RequireImmutableID bool
	// RequireNoImmutableID rejects any immutable backend ID. Name-addressed
	// targets use it: a reported ID would be an identity the caller cannot
	// verify.
	RequireNoImmutableID bool
}

// OwnershipObserved captures the facts an inspection reported about the
// target container.
type OwnershipObserved struct {
	// UID is the backend identity the inspection reported. It is empty when
	// the backend addresses the container by name only.
	UID string
	// Labels is the inspected label set.
	Labels map[string]string
}

// OwnershipFailure names the first expected ownership fact that did not
// match an observation.
type OwnershipFailure string

const (
	OwnershipFailureNone       OwnershipFailure = ""
	OwnershipFailureManaged    OwnershipFailure = "managed"
	OwnershipFailureReuse      OwnershipFailure = "reuse"
	OwnershipFailureReuseGroup OwnershipFailure = "reuse-group"
	OwnershipFailureSession    OwnershipFailure = "session"
	OwnershipFailureCreation   OwnershipFailure = "creation"
	OwnershipFailureIdentity   OwnershipFailure = "identity"
)

var (
	creationIDRE           = regexp.MustCompile(`^[0-9a-f]{16}$`)
	immutableContainerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsCreationID reports whether generation has the structural form of a
// container-go generation identifier. A label carrying any other value cannot
// prove that an observation names the generation a caller created.
func IsCreationID(generation string) bool {
	return creationIDRE.MatchString(generation)
}

// IsImmutableContainerID reports whether id has Docker's immutable container
// ID form. Destructive operations bind to this value only after it passes.
func IsImmutableContainerID(id string) bool {
	return immutableContainerIDRE.MatchString(id)
}

// VerifyOwnership applies the library's ownership and generation rules to one
// observation. It is pure: it reads only its arguments and returns the first
// mismatching fact, which the imperative shell maps to its operation-specific
// behavior and messages. OwnershipFailureNone means every expected fact
// matched. The verification order is fixed: managed marker, reuse marker,
// reuse group, session, creation generation, backend identity.
func VerifyOwnership(expected OwnershipExpected, observed OwnershipObserved) OwnershipFailure {
	labels := observed.Labels
	if expected.RequireManaged && labels[LabelManaged] != "true" {
		return OwnershipFailureManaged
	}
	if expected.RequireReuse && labels[LabelReuse] != "true" {
		return OwnershipFailureReuse
	}
	if expected.ReuseGroup != "" && labels[LabelReuseGroup] != expected.ReuseGroup {
		return OwnershipFailureReuseGroup
	}
	if expected.Session != "" && labels[LabelSession] != expected.Session {
		return OwnershipFailureSession
	}
	if expected.Creation != "" || expected.RequireCreation {
		actual := labels[LabelCreation]
		if !IsCreationID(actual) || (expected.Creation != "" && actual != expected.Creation) {
			return OwnershipFailureCreation
		}
	}
	if expected.RequireNoImmutableID && observed.UID != "" {
		return OwnershipFailureIdentity
	}
	if expected.RequireImmutableID && !IsImmutableContainerID(observed.UID) {
		return OwnershipFailureIdentity
	}
	if expected.UID != "" && observed.UID != expected.UID {
		return OwnershipFailureIdentity
	}
	return OwnershipFailureNone
}

// SameOwnershipLabels reports whether two observations carry identical values
// for every ownership-bearing label. Custom labels are deliberately excluded:
// they do not identify the generation.
func SameOwnershipLabels(a, b map[string]string) bool {
	for _, key := range [...]string{
		LabelManaged,
		LabelSession,
		LabelCreation,
		LabelReuse,
		LabelReuseGroup,
	} {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}
